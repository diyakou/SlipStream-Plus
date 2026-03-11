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

	// ──── SOCKS5 CONNECT Request ────
	// Read: VER(1) CMD(1) RSV(1) ATYP(1) + ADDR + PORT(2)
	if _, err := io.ReadFull(clientConn, buf[:4]); err != nil {
		return
	}
	if buf[1] != 0x01 { // only CONNECT supported
		clientConn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	atyp := buf[3]

	// Capture target address raw bytes for replaying to upstream
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

	candidates := append([]*engine.Instance(nil), socksHealthy...)
	var inst *engine.Instance
	var upstreamConn net.Conn
	var lastErr error

	for len(candidates) > 0 {
		inst = s.balancer.Pick(candidates)
		if inst == nil {
			break
		}

		upstreamConn, lastErr = s.connectThroughInstance(inst, atyp, addrBytes, portBytes)
		if lastErr == nil {
			break
		}

		log.Printf("[proxy] conn#%d: instance %d failed, trying next: %v", connID, inst.ID(), lastErr)
		candidates = removeInstanceByID(candidates, inst.ID())
		inst = nil
	}

	if upstreamConn == nil || inst == nil {
		if lastErr != nil {
			log.Printf("[proxy] conn#%d: all instances failed: %v", connID, lastErr)
		}
		clientConn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstreamConn.Close()

	// ──── Success! Tell client and start relay ────
	clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	inst.IncrConns()
	defer inst.DecrConns()

	port := binary.BigEndian.Uint16(portBytes)
	log.Printf("[proxy] conn#%d: connected via instance %d, port %d", connID, inst.ID(), port)

	s.relay(clientConn, upstreamConn, inst, user, connID)
}

func (s *Server) connectThroughInstance(inst *engine.Instance, atyp byte, addrBytes, portBytes []byte) (net.Conn, error) {
	upstreamConn, err := inst.Dial()
	if err != nil {
		return nil, err
	}

	if tc, ok := upstreamConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
	}

	if _, err := upstreamConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream greeting write: %w", err)
	}

	greetResp := make([]byte, 2)
	if _, err := io.ReadFull(upstreamConn, greetResp); err != nil {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream greeting read: %w", err)
	}
	if greetResp[0] != 0x05 || greetResp[1] == 0xFF {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream greeting invalid: %v", greetResp)
	}

	connectReq := make([]byte, 0, 4+len(addrBytes)+2)
	connectReq = append(connectReq, 0x05, 0x01, 0x00, atyp)
	connectReq = append(connectReq, addrBytes...)
	connectReq = append(connectReq, portBytes...)
	if _, err := upstreamConn.Write(connectReq); err != nil {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream connect write: %w", err)
	}

	connectResp := make([]byte, 4)
	if _, err := io.ReadFull(upstreamConn, connectResp); err != nil {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream connect header: %w", err)
	}
	if err := discardSocksReplyAddress(upstreamConn, connectResp[3]); err != nil {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream connect payload: %w", err)
	}
	if connectResp[1] != 0x00 {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream refused with code %d", connectResp[1])
	}

	return upstreamConn, nil
}

func discardSocksReplyAddress(conn net.Conn, atyp byte) error {
	switch atyp {
	case 0x01:
		_, err := io.ReadFull(conn, make([]byte, 4+2))
		return err
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return err
		}
		_, err := io.ReadFull(conn, make([]byte, int(lenBuf[0])+2))
		return err
	case 0x04:
		_, err := io.ReadFull(conn, make([]byte, 16+2))
		return err
	default:
		_, err := io.ReadFull(conn, make([]byte, 4+2))
		return err
	}
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
