package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ParsaKSH/SlipStream-Plus/internal/balancer"
	"github.com/ParsaKSH/SlipStream-Plus/internal/engine"
	"github.com/ParsaKSH/SlipStream-Plus/internal/users"
)

// HTTPServer is an HTTP CONNECT proxy that forwards connections to slipstream instances.
type HTTPServer struct {
	listenAddr     string
	bufferSize     int
	maxConnections int
	manager        *engine.Manager
	balancer       balancer.Balancer
	userMgr        *users.Manager
	activeConns    atomic.Int64
	bufPool        sync.Pool
	connID         atomic.Uint64
	server         *http.Server
}

func NewHTTPServer(listenAddr string, bufferSize int, maxConns int, mgr *engine.Manager, bal balancer.Balancer, umgr *users.Manager) *HTTPServer {
	return &HTTPServer{
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

func (h *HTTPServer) ListenAndServe() error {
	lc := listenConfig()
	ln, err := lc.Listen(context.Background(), "tcp", h.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", h.listenAddr, err)
	}

	log.Printf("[proxy-http] HTTP CONNECT proxy listening on %s (buffer=%d, max_conns=%d)",
		h.listenAddr, h.bufferSize, h.maxConnections)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[proxy-http] accept error: %v", err)
			continue
		}

		current := h.activeConns.Load()
		if current >= int64(h.maxConnections) {
			conn.Close()
			continue
		}

		h.activeConns.Add(1)
		id := h.connID.Add(1)
		go h.handleConnection(conn, id)
	}
}

func (h *HTTPServer) handleConnection(clientConn net.Conn, connID uint64) {
	defer func() {
		clientConn.Close()
		h.activeConns.Add(-1)
	}()

	if tc, ok := clientConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
		tc.SetReadBuffer(h.bufferSize)
		tc.SetWriteBuffer(h.bufferSize)
	}

	reader := bufio.NewReader(clientConn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		if err != io.EOF {
			log.Printf("[proxy-http] conn#%d: read request failed: %v", connID, err)
		}
		return
	}

	// Handle CONNECT request (for HTTPS/HTTP tunneling)
	if req.Method == http.MethodConnect {
		h.handleConnect(clientConn, req, connID)
		return
	}

	// Handle regular HTTP requests (GET, POST, etc.)
	h.handleHTTP(clientConn, req, reader, connID)
}

// handleConnect implements HTTP CONNECT tunneling (for HTTPS and WebSockets)
func (h *HTTPServer) handleConnect(clientConn net.Conn, req *http.Request, connID uint64) {
	targetHost := req.RequestURI

	// Pick upstream instance
	healthy := h.manager.HealthyInstances()
	socksHealthy := make([]*engine.Instance, 0, len(healthy))
	for _, inst := range healthy {
		if inst.Config.Mode != "ssh" {
			socksHealthy = append(socksHealthy, inst)
		}
	}
	if len(socksHealthy) == 0 {
		clientConn.Write([]byte("HTTP/1.0 503 Service Unavailable\r\n\r\n"))
		return
	}

	inst := h.balancer.Pick(socksHealthy)
	if inst == nil {
		clientConn.Write([]byte("HTTP/1.0 503 Service Unavailable\r\n\r\n"))
		return
	}

	// Dial upstream SOCKS5 proxy
	upstreamConn, err := inst.Dial()
	if err != nil {
		log.Printf("[proxy-http] conn#%d: dial instance %d failed: %v", connID, inst.ID(), err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstreamConn.Close()

	if tc, ok := upstreamConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
		tc.SetReadBuffer(h.bufferSize)
		tc.SetWriteBuffer(h.bufferSize)
	}

	// Build SOCKS5 CONNECT request for target
	socksReq := h.buildSOCKS5Connect(targetHost)
	if _, err := upstreamConn.Write(socksReq); err != nil {
		log.Printf("[proxy-http] conn#%d: write to upstream failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Read SOCKS5 response
	resp := make([]byte, 10)
	if _, err := io.ReadFull(upstreamConn, resp); err != nil {
		log.Printf("[proxy-http] conn#%d: read from upstream failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	if resp[1] != 0x00 {
		log.Printf("[proxy-http] conn#%d: SOCKS5 connect failed: %d", connID, resp[1])
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Success! Send HTTP 200 to client
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	inst.IncrConns()
	defer inst.DecrConns()

	log.Printf("[proxy-http] conn#%d: CONNECT %s via instance %d", connID, targetHost, inst.ID())

	// Relay data between client and upstream
	h.relay(clientConn, upstreamConn, inst, nil, connID)
}

// handleHTTP handles direct HTTP requests (GET, POST, etc.)
func (h *HTTPServer) handleHTTP(clientConn net.Conn, req *http.Request, reader *bufio.Reader, connID uint64) {
	// For HTTP (not HTTPS), we can also tunnel through SOCKS5
	// But most clients use CONNECT for HTTPS anyway
	// This is a fallback for plain HTTP

	if req.URL.Scheme == "" || req.URL.Host == "" {
		clientConn.Write([]byte("HTTP/1.0 400 Bad Request\r\n\r\n"))
		return
	}

	targetHost := req.URL.Host
	if req.URL.Port() == "" {
		if req.URL.Scheme == "https" {
			targetHost = req.URL.Host + ":443"
		} else {
			targetHost = req.URL.Host + ":80"
		}
	}

	// Pick upstream instance
	healthy := h.manager.HealthyInstances()
	socksHealthy := make([]*engine.Instance, 0, len(healthy))
	for _, inst := range healthy {
		if inst.Config.Mode != "ssh" {
			socksHealthy = append(socksHealthy, inst)
		}
	}
	if len(socksHealthy) == 0 {
		clientConn.Write([]byte("HTTP/1.0 503 Service Unavailable\r\n\r\n"))
		return
	}

	inst := h.balancer.Pick(socksHealthy)
	if inst == nil {
		clientConn.Write([]byte("HTTP/1.0 503 Service Unavailable\r\n\r\n"))
		return
	}

	upstreamConn, err := inst.Dial()
	if err != nil {
		log.Printf("[proxy-http] conn#%d: dial instance %d failed: %v", connID, inst.ID(), err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstreamConn.Close()

	// Send SOCKS5 request
	socksReq := h.buildSOCKS5Connect(targetHost)
	if _, err := upstreamConn.Write(socksReq); err != nil {
		log.Printf("[proxy-http] conn#%d: write to upstream failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	resp := make([]byte, 10)
	if _, err := io.ReadFull(upstreamConn, resp); err != nil {
		log.Printf("[proxy-http] conn#%d: read from upstream failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	if resp[1] != 0x00 {
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	inst.IncrConns()
	defer inst.DecrConns()

	log.Printf("[proxy-http] conn#%d: HTTP %s via instance %d", connID, targetHost, inst.ID())
	h.relay(clientConn, upstreamConn, inst, nil, connID)
}

// buildSOCKS5Connect builds a SOCKS5 CONNECT request for a target host:port
func (h *HTTPServer) buildSOCKS5Connect(targetHostPort string) []byte {
	// Parse host and port
	host, port, err := net.SplitHostPort(targetHostPort)
	if err != nil {
		// Assume HTTP (80) or HTTPS (443) if not specified
		host = targetHostPort
		port = "80"
	}

	var portNum uint16
	fmt.Sscanf(port, "%d", &portNum)

	// Build SOCKS5 request
	// VER=5, CMD=1 (CONNECT), RSV=0, ATYP (1=IPv4, 3=domain), ADDR, PORT
	req := []byte{0x05, 0x01, 0x00}

	// Try parsing as IPv4
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			req = append(req, 0x01)       // IPv4
			req = append(req, ipv4...)    // 4 bytes
		} else {
			req = append(req, 0x04)       // IPv6
			req = append(req, ip...)      // 16 bytes
		}
	} else {
		// Domain name
		req = append(req, 0x03)           // Domain
		req = append(req, byte(len(host)))
		req = append(req, []byte(host)...)
	}

	// Add port (big-endian)
	portBytes := [2]byte{byte(portNum >> 8), byte(portNum & 0xFF)}
	req = append(req, portBytes[:]...)

	return req
}

// relay bidirectional copy between client and upstream
func (h *HTTPServer) relay(clientConn, upstreamConn net.Conn, inst *engine.Instance, user *users.User, connID uint64) {
	var clientToUpstream, upstreamToClient int64
	done := make(chan struct{}, 2)

	go func() {
		var dst io.Writer = upstreamConn
		var src io.Reader = clientConn
		if user != nil {
			src = user.WrapReader(src)
		}
		bufPtr := h.bufPool.Get().(*[]byte)
		n, _ := io.CopyBuffer(dst, src, *bufPtr)
		h.bufPool.Put(bufPtr)
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
		bufPtr := h.bufPool.Get().(*[]byte)
		n, _ := io.CopyBuffer(dst, src, *bufPtr)
		h.bufPool.Put(bufPtr)
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
	log.Printf("[proxy-http] conn#%d: relay closed (tx=%d, rx=%d)", connID, clientToUpstream, upstreamToClient)
}
