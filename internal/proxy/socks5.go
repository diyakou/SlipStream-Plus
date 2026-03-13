package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ParsaKSH/SlipStream-Plus/internal/balancer"
	"github.com/ParsaKSH/SlipStream-Plus/internal/engine"
	"github.com/ParsaKSH/SlipStream-Plus/internal/users"
)

// Server is a SOCKS5 proxy with optional user auth, load balancing across instances.
type Server struct {
	listenAddr     string
	bufferSize     int
	maxConnections int
	manager        *engine.Manager
	balancer       balancer.Balancer
	userMgr        *users.Manager
	activeConns    atomic.Int64
	bufPool        sync.Pool
	connID         atomic.Uint64
}

func NewServer(listenAddr string, bufferSize int, maxConns int, mgr *engine.Manager, bal balancer.Balancer, umgr *users.Manager) *Server {
	return &Server{
		listenAddr:     listenAddr,
		bufferSize:     bufferSize,
		maxConnections: maxConns,
		manager:        mgr,
		balancer:       bal,
		userMgr:        umgr,
		bufPool: sync.Pool{
			New: func() any {
				buf := make([]byte, bufferSize)
				return &buf
			},
		},
	}
}

func (s *Server) ListenAndServe() error {
	lc := listenConfig()
	ln, err := lc.Listen(context.Background(), "tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.listenAddr, err)
	}
	defer ln.Close()

	authMode := "no-auth"
	if s.userMgr != nil && s.userMgr.HasUsers() {
		authMode = "username/password"
	}
	log.Printf("[proxy] SOCKS5 proxy listening on %s (auth=%s, buffer=%d, max_conns=%d)",
		s.listenAddr, authMode, s.bufferSize, s.maxConnections)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[proxy] accept error: %v", err)
			continue
		}
		current := s.activeConns.Load()
		if current >= int64(s.maxConnections) {
			conn.Close()
			continue
		}
		s.activeConns.Add(1)
		id := s.connID.Add(1)
		go s.handleConnection(conn, id)
	}
}

func (s *Server) handleConnection(clientConn net.Conn, connID uint64) {
	defer func() {
		clientConn.Close()
		s.activeConns.Add(-1)
	}()

	if tc, ok := clientConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
		tc.SetReadBuffer(s.bufferSize)
		tc.SetWriteBuffer(s.bufferSize)
	}

	clientIP := users.ExtractIP(clientConn.RemoteAddr())

	// ──── SOCKS5 Greeting ────
	buf := make([]byte, 258)
	if _, err := io.ReadFull(clientConn, buf[:2]); err != nil {
		return
	}
	if buf[0] != 0x05 {
		return
	}
	nMethods := int(buf[1])
	if _, err := io.ReadFull(clientConn, buf[:nMethods]); err != nil {
		return
	}

	requireAuth := s.userMgr != nil && s.userMgr.HasUsers()
	var user *users.User

	if requireAuth {
		// Require method 0x02 (username/password)
		found := false
		for i := 0; i < nMethods; i++ {
			if buf[i] == 0x02 {
				found = true
				break
			}
		}
		if !found {
			clientConn.Write([]byte{0x05, 0xFF})
			return
		}
		clientConn.Write([]byte{0x05, 0x02})

		// ──── RFC 1929 Username/Password ────
		if _, err := io.ReadFull(clientConn, buf[:2]); err != nil {
			return
		}
		uLen := int(buf[1])
		if _, err := io.ReadFull(clientConn, buf[:uLen]); err != nil {
			return
		}
		username := string(buf[:uLen])

		if _, err := io.ReadFull(clientConn, buf[:1]); err != nil {
			return
		}
		pLen := int(buf[0])
		if _, err := io.ReadFull(clientConn, buf[:pLen]); err != nil {
			return
		}
		password := string(buf[:pLen])

		var ok bool
		user, ok = s.userMgr.Authenticate(username, password)
		if !ok {
			clientConn.Write([]byte{0x01, 0x01})
			log.Printf("[proxy] conn#%d: auth failed user=%q ip=%s", connID, username, clientIP)
			return
		}
		clientConn.Write([]byte{0x01, 0x00})

		if reason := user.CheckConnect(clientIP); reason != "" {
			log.Printf("[proxy] conn#%d: user %q denied: %s", connID, username, reason)
			// Wait for CONNECT request then reply with general failure
			io.ReadFull(clientConn, buf[:4]) // read VER+CMD+RSV+ATYP
			clientConn.Write([]byte{0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return
		}

		user.MarkConnect(clientIP)
		defer user.MarkDisconnect(clientIP)
	} else {
		clientConn.Write([]byte{0x05, 0x00})
	}

	// ──── SOCKS5 CONNECT/UDP ASSOCIATE Request ────
	// Read: VER(1) CMD(1) RSV(1) ATYP(1) + ADDR + PORT(2)
	if _, err := io.ReadFull(clientConn, buf[:4]); err != nil {
		return
	}
	cmd := buf[1]
	if cmd != 0x01 && cmd != 0x03 { // only CONNECT + UDP ASSOCIATE supported
		clientConn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	atyp := buf[3]

	// Capture target address raw bytes for replaying to upstream (or drain for UDP ASSOCIATE)
	var addrBytes []byte
	switch atyp {
	case 0x01: // IPv4
		addrBytes = make([]byte, 4)
		if _, err := io.ReadFull(clientConn, addrBytes); err != nil {
			return
		}
	case 0x03: // Domain
		if _, err := io.ReadFull(clientConn, buf[:1]); err != nil {
			return
		}
		domLen := int(buf[0])
		addrBytes = make([]byte, 1+domLen)
		addrBytes[0] = buf[0]
		if _, err := io.ReadFull(clientConn, addrBytes[1:]); err != nil {
			return
		}
	case 0x04: // IPv6
		addrBytes = make([]byte, 16)
		if _, err := io.ReadFull(clientConn, addrBytes); err != nil {
			return
		}
	default:
		clientConn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, portBytes); err != nil {
		return
	}

	// ──── Pick upstream slipstream instance ────
	healthy := s.manager.HealthyInstances()
	socksHealthy := make([]*engine.Instance, 0, len(healthy))
	for _, inst := range healthy {
		if inst.Config.Mode != "ssh" {
			socksHealthy = append(socksHealthy, inst)
		}
	}
	if len(socksHealthy) == 0 {
		clientConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	inst := s.balancer.Pick(socksHealthy)
	if inst == nil {
		clientConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	if cmd == 0x03 {
		s.handleUDPAssociate(clientConn, inst, user, connID)
		return
	}

	upstreamConn, err := inst.Dial()
	if err != nil {
		log.Printf("[proxy] conn#%d: dial instance %d failed: %v", connID, inst.ID(), err)
		clientConn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstreamConn.Close()

	if tc, ok := upstreamConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
		tc.SetReadBuffer(s.bufferSize)
		tc.SetWriteBuffer(s.bufferSize)
	}

	// ──── Pipelined SOCKS5 negotiation with upstream ────
	// Combine greeting + CONNECT into ONE write to minimize DNS tunnel round-trips.
	// Greeting: VER=5, NMETHODS=1, METHOD=0x00 (no auth)
	// CONNECT:  VER=5, CMD=1, RSV=0, ATYP, ADDR, PORT
	pipelined := make([]byte, 0, 3+4+len(addrBytes)+2)
	pipelined = append(pipelined, 0x05, 0x01, 0x00)       // greeting
	pipelined = append(pipelined, 0x05, 0x01, 0x00, atyp) // CONNECT header
	pipelined = append(pipelined, addrBytes...)           // target addr
	pipelined = append(pipelined, portBytes...)           // target port
	if _, err := upstreamConn.Write(pipelined); err != nil {
		clientConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// Read greeting response (2 bytes) + CONNECT response header (4 bytes) = 6 bytes
	resp := make([]byte, 6)
	if _, err := io.ReadFull(upstreamConn, resp); err != nil {
		clientConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	// resp[0..1] = greeting reply, resp[2..5] = CONNECT reply header

	// Drain the CONNECT reply's bind address + port
	repAtyp := resp[5]
	switch repAtyp {
	case 0x01:
		io.ReadFull(upstreamConn, make([]byte, 4+2))
	case 0x03:
		lenBuf := make([]byte, 1)
		io.ReadFull(upstreamConn, lenBuf)
		io.ReadFull(upstreamConn, make([]byte, int(lenBuf[0])+2))
	case 0x04:
		io.ReadFull(upstreamConn, make([]byte, 16+2))
	default:
		io.ReadFull(upstreamConn, make([]byte, 4+2))
	}

	if resp[3] != 0x00 { // CONNECT reply status
		clientConn.Write([]byte{0x05, resp[3], 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// ──── Success! Tell client and start relay ────
	clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	inst.IncrConns()
	defer inst.DecrConns()

	port := binary.BigEndian.Uint16(portBytes)
	log.Printf("[proxy] conn#%d: connected via instance %d, port %d", connID, inst.ID(), port)

	s.relay(clientConn, upstreamConn, inst, user, connID)
}

func (s *Server) handleUDPAssociate(clientConn net.Conn, inst *engine.Instance, user *users.User, connID uint64) {
	// Create a local UDP socket for the client to send datagrams to.
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		clientConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer udpConn.Close()

	// Reply with the local bind address so the client knows where to send UDP.
	localAddr := udpConn.LocalAddr().(*net.UDPAddr)
	reply := make([]byte, 10)
	reply[0] = 0x05
	reply[1] = 0x00
	reply[2] = 0x00
	reply[3] = 0x01
	copy(reply[4:8], localAddr.IP.To4())
	binary.BigEndian.PutUint16(reply[8:], uint16(localAddr.Port))
	clientConn.Write(reply)

	// Establish SOCKS5 UDP ASSOCIATE with upstream instance.
	upstreamConn, err := inst.Dial()
	if err != nil {
		log.Printf("[proxy] conn#%d: dial instance %d failed: %v", connID, inst.ID(), err)
		return
	}
	defer upstreamConn.Close()

	if tc, ok := upstreamConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
		tc.SetReadBuffer(s.bufferSize)
		tc.SetWriteBuffer(s.bufferSize)
	}

	// Request UDP ASSOCIATE from upstream.
	if _, err := upstreamConn.Write([]byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		log.Printf("[proxy] conn#%d: upstream udp associate write failed: %v", connID, err)
		return
	}

	resp := make([]byte, 6)
	if _, err := io.ReadFull(upstreamConn, resp); err != nil {
		log.Printf("[proxy] conn#%d: upstream udp associate read failed: %v", connID, err)
		return
	}
	if resp[1] != 0x00 {
		log.Printf("[proxy] conn#%d: upstream udp associate failed: %d", connID, resp[1])
		return
	}

	// Determine upstream UDP relay address
	repAtyp := resp[5]
	var upAddr *net.UDPAddr
	switch repAtyp {
	case 0x01:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(upstreamConn, addr); err != nil {
			log.Printf("[proxy] conn#%d: read upstream udp addr failed: %v", connID, err)
			return
		}
		portBuf := make([]byte, 2)
		if _, err := io.ReadFull(upstreamConn, portBuf); err != nil {
			log.Printf("[proxy] conn#%d: read upstream udp port failed: %v", connID, err)
			return
		}
		upAddr = &net.UDPAddr{IP: net.IP(addr), Port: int(binary.BigEndian.Uint16(portBuf))}
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(upstreamConn, lenBuf); err != nil {
			log.Printf("[proxy] conn#%d: read upstream udp domain len failed: %v", connID, err)
			return
		}
		domLen := int(lenBuf[0])
		dom := make([]byte, domLen)
		if _, err := io.ReadFull(upstreamConn, dom); err != nil {
			log.Printf("[proxy] conn#%d: read upstream udp domain failed: %v", connID, err)
			return
		}
		portBuf := make([]byte, 2)
		if _, err := io.ReadFull(upstreamConn, portBuf); err != nil {
			log.Printf("[proxy] conn#%d: read upstream udp port failed: %v", connID, err)
			return
		}
		upAddr = &net.UDPAddr{IP: net.ParseIP(string(dom)), Port: int(binary.BigEndian.Uint16(portBuf))}
	case 0x04:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(upstreamConn, addr); err != nil {
			log.Printf("[proxy] conn#%d: read upstream udp addr failed: %v", connID, err)
			return
		}
		portBuf := make([]byte, 2)
		if _, err := io.ReadFull(upstreamConn, portBuf); err != nil {
			log.Printf("[proxy] conn#%d: read upstream udp port failed: %v", connID, err)
			return
		}
		upAddr = &net.UDPAddr{IP: net.IP(addr), Port: int(binary.BigEndian.Uint16(portBuf))}
	default:
		log.Printf("[proxy] conn#%d: upstream udp associate returned unknown atyp %d", connID, repAtyp)
		return
	}

	upstreamUDP, err := net.DialUDP("udp", nil, upAddr)
	if err != nil {
		log.Printf("[proxy] conn#%d: dial upstream udp %s failed: %v", connID, upAddr, err)
		return
	}
	defer upstreamUDP.Close()

	inst.IncrConns()
	defer inst.DecrConns()

	// Relay UDP datagrams between client and upstream.
	done := make(chan struct{}, 2)
	var clientAddr *net.UDPAddr

	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if clientAddr == nil {
				clientAddr = addr
			}
			if _, err := upstreamUDP.Write(buf[:n]); err != nil {
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := upstreamUDP.Read(buf)
			if err != nil {
				break
			}
			if clientAddr != nil {
				udpConn.WriteToUDP(buf[:n], clientAddr)
			}
		}
		done <- struct{}{}
	}()

	<-done
	<-done
}

func (s *Server) relay(clientConn, upstreamConn net.Conn, inst *engine.Instance, user *users.User, connID uint64) {
	var clientToUpstream, upstreamToClient int64
	done := make(chan struct{}, 2)

	go func() {
		var dst io.Writer = upstreamConn
		var src io.Reader = clientConn
		if user != nil {
			src = user.WrapReader(src)
		}
		bufPtr := s.bufPool.Get().(*[]byte)
		n, _ := io.CopyBuffer(dst, src, *bufPtr)
		s.bufPool.Put(bufPtr)
		clientToUpstream = n
		if tc, ok := upstreamConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	go func() {
		var dst io.Writer = clientConn
		var src io.Reader = upstreamConn
		if user != nil {
			dst = user.WrapWriter(dst)
		}
		bufPtr := s.bufPool.Get().(*[]byte)
		n, _ := io.CopyBuffer(dst, src, *bufPtr)
		s.bufPool.Put(bufPtr)
		upstreamToClient = n
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	<-done
	<-done

	inst.AddTx(clientToUpstream)
	inst.AddRx(upstreamToClient)
}

func (s *Server) ActiveConnections() int64 {
	return s.activeConns.Load()
}
