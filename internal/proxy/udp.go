package proxy

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ParsaKSH/SlipStream-Plus/internal/balancer"
	"github.com/ParsaKSH/SlipStream-Plus/internal/engine"
)

// UDPRelay relays UDP datagrams through SOCKS5 tunnels to slipstream instances.
type UDPRelay struct {
	listenAddr     string
	manager        *engine.Manager
	balancer       balancer.Balancer
	activeConns    atomic.Int64
	connID         atomic.Uint64
	bufferSize     int
	sessionTimeout time.Duration
	sessions       sync.Map // map[string]*UDPSession (key: "src_ip:src_port:dst_ip:dst_port")
}

type UDPSession struct {
	clientAddr   *net.UDPAddr
	targetAddr   string
	upstreamConn net.Conn
	inst         *engine.Instance
	lastActivity time.Time
	mu           sync.Mutex
}

func NewUDPRelay(listenAddr string, bufferSize int, mgr *engine.Manager, bal balancer.Balancer) *UDPRelay {
	return &UDPRelay{
		listenAddr:     listenAddr,
		manager:        mgr,
		balancer:       bal,
		bufferSize:     bufferSize,
		sessionTimeout: 5 * time.Minute,
	}
}

func (u *UDPRelay) ListenAndServe() error {
	addr, err := net.ResolveUDPAddr("udp", u.listenAddr)
	if err != nil {
		return fmt.Errorf("resolve UDP addr %s: %w", u.listenAddr, err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen UDP %s: %w", u.listenAddr, err)
	}
	defer conn.Close()

	log.Printf("[proxy-udp] UDP relay listening on %s (buffer=%d)", u.listenAddr, u.bufferSize)

	// Start session cleanup goroutine
	go u.cleanupSessions()

	// Reuse single buffer for all datagrams (concurrent reads use separate buffers via lock-free queue)
	// Read loop - SINGLE goroutine for better cache locality
	for {
		buf := make([]byte, u.bufferSize)
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[proxy-udp] read error: %v", err)
			continue
		}

		// Process immediately in same goroutine if small, or spawn for large payloads
		// Small payloads: keep in-thread; large: spawn goroutine to avoid blocking
		if n > 4096 {
			// Large payload: spawn goroutine to avoid blocking main loop
			id := u.connID.Add(1)
			go u.handleDatagram(conn, buf[:n], remoteAddr, id)
		} else {
			// Small payload: handle inline (avoid goroutine overhead)
			u.handleDatagram(conn, buf[:n], remoteAddr, u.connID.Add(1))
		}
	}
}

func (u *UDPRelay) handleDatagram(udpConn *net.UDPConn, data []byte, clientAddr *net.UDPAddr, connID uint64) {
	// For simplicity, we'll parse the datagram as if it's already a SOCKS5 request
	// or we can use a simple format: [domain_len:domain:port(2 bytes):payload]
	// For now, let's use raw forwarding with a simple header

	if len(data) < 3 {
		return // Too small
	}

	// Very simple format to extract target:
	// First byte: reserved (0x00)
	// Remaining bytes: target:port:payload
	// We'll parse it as: [target string (null-terminated):port(2):payload]

	if data[0] != 0x00 {
		return // Invalid format
	}

	// Find null terminator for target
	var nullIdx int
	for i := 1; i < len(data); i++ {
		if data[i] == 0x00 {
			nullIdx = i
			break
		}
	}

	if nullIdx == 0 || nullIdx+2 >= len(data) {
		return // Invalid format
	}

	target := string(data[1:nullIdx])
	port := int(data[nullIdx+1])<<8 | int(data[nullIdx+2])
	payload := data[nullIdx+3:]

	targetAddr := fmt.Sprintf("%s:%d", target, port)
	sessionKey := fmt.Sprintf("%s:%s", clientAddr.String(), targetAddr)

	// Get or create session
	sessionI, _ := u.sessions.LoadOrStore(sessionKey, &UDPSession{
		clientAddr: clientAddr,
		targetAddr: targetAddr,
	})
	session := sessionI.(*UDPSession)

	session.mu.Lock()
	defer session.mu.Unlock()

	session.lastActivity = time.Now()

	// Create upstream connection if needed (lazy initialization)
	if session.upstreamConn == nil {
		healthy := u.manager.HealthyInstances()
		// Reuse slice to avoid allocation
		socksHealthy := healthy[:0]
		for _, inst := range healthy {
			if inst.Config.Mode != "ssh" {
				socksHealthy = append(socksHealthy, inst)
			}
		}
		if len(socksHealthy) == 0 {
			return // No healthy instances - silently drop packet
		}

		inst := u.balancer.Pick(socksHealthy)
		if inst == nil {
			return // No instance available
		}

		conn, err := inst.Dial()
		if err != nil {
			return // Failed to dial - silently drop
		}

		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(30 * time.Second)
			tc.SetNoDelay(true)
			tc.SetReadBuffer(u.bufferSize)
			tc.SetWriteBuffer(u.bufferSize)
		}

		session.upstreamConn = conn
		session.inst = inst
		inst.IncrConns()

		// Start response reader goroutine
		go u.readUpstreamResponses(udpConn, session, connID)
	}

	// Send payload to upstream
	// Format: [0x00:target:0x00:port(2):payload]
	req := []byte{0x00}
	req = append(req, target...)
	req = append(req, 0x00)
	req = append(req, byte(port>>8), byte(port&0xFF))
	req = append(req, payload...)

	if _, err := session.upstreamConn.Write(req); err != nil {
		log.Printf("[proxy-udp] conn#%d: write to upstream failed: %v", connID, err)
		session.upstreamConn.Close()
		session.upstreamConn = nil
		u.sessions.Delete(sessionKey)
		return
	}

	session.inst.AddTx(int64(len(req)))
}

func (u *UDPRelay) readUpstreamResponses(udpConn *net.UDPConn, session *UDPSession, connID uint64) {
	buf := make([]byte, u.bufferSize)
	for {
		session.mu.Lock()
		conn := session.upstreamConn
		session.mu.Unlock()

		if conn == nil {
			return
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Minute))
		n, err := conn.Read(buf)
		if err != nil {
			log.Printf("[proxy-udp] conn#%d: read from upstream failed: %v", connID, err)
			session.mu.Lock()
			conn.Close()
			session.upstreamConn = nil
			session.inst.DecrConns()
			session.mu.Unlock()
			return
		}

		// Parse response: [0x00:target:0x00:port(2):payload]
		if n < 4 {
			continue
		}

		if buf[0] != 0x00 {
			continue
		}

		// Find null terminator
		var nullIdx int
		for i := 1; i < n; i++ {
			if buf[i] == 0x00 {
				nullIdx = i
				break
			}
		}

		if nullIdx == 0 || nullIdx+2 >= n {
			continue
		}

		payload := buf[nullIdx+3 : n]

		// Send back to client
		_, err = udpConn.WriteToUDP(payload, session.clientAddr)
		if err != nil {
			log.Printf("[proxy-udp] conn#%d: write to client failed: %v", connID, err)
			return
		}

		session.inst.AddRx(int64(len(payload)))
	}
}

// cleanupSessions removes inactive sessions
func (u *UDPRelay) cleanupSessions() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		u.sessions.Range(func(key, value interface{}) bool {
			session := value.(*UDPSession)
			session.mu.Lock()
			if now.Sub(session.lastActivity) > u.sessionTimeout {
				session.mu.Unlock()
				if session.upstreamConn != nil {
					session.upstreamConn.Close()
					session.inst.DecrConns()
				}
				u.sessions.Delete(key)
				return true
			}
			session.mu.Unlock()
			return true
		})
	}
}
