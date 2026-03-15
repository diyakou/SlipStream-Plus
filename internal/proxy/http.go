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
)

// HTTPServer is an HTTP CONNECT proxy that forwards connections via SOCKS5 to slipstream instances.
type HTTPServer struct {
	listenAddr     string
	bufferSize     int
	maxConnections int
	socksAddr      string // Address of SOCKS5 server (for balancing)
	activeConns    atomic.Int64
	bufPool        sync.Pool
	connID         atomic.Uint64
}

func NewHTTPServer(listenAddr string, socksAddr string, bufferSize int, maxConns int) *HTTPServer {
	return &HTTPServer{
		listenAddr:     listenAddr,
		socksAddr:      socksAddr,
		bufferSize:     bufferSize,
		maxConnections: maxConns,
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

// handleConnect implements HTTP CONNECT tunneling via SOCKS5
func (h *HTTPServer) handleConnect(clientConn net.Conn, req *http.Request, connID uint64) {
	targetHost := req.RequestURI

	// Connect to SOCKS5 server (which handles balancing internally)
	upstreamConn, err := net.DialTimeout("tcp", h.socksAddr, 5*time.Second)
	if err != nil {
		log.Printf("[proxy-http] conn#%d: dial SOCKS5 server %s failed: %v", connID, h.socksAddr, err)
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

	// Use single handshake buffer to minimize allocations
	handshakeBuf := make([]byte, 1024)

	// Step 1: Send SOCKS5 greeting
	greetReq := []byte{0x05, 0x01, 0x00} // VER=5, NMETHODS=1, METHOD=0x00 (no auth)
	if _, err := upstreamConn.Write(greetReq); err != nil {
		log.Printf("[proxy-http] conn#%d: write greeting to SOCKS5 failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Step 2: Read greeting response (2 bytes)
	greetResp := handshakeBuf[:2]
	if _, err := io.ReadFull(upstreamConn, greetResp); err != nil {
		log.Printf("[proxy-http] conn#%d: read greeting response failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}
	if greetResp[0] != 0x05 || greetResp[1] != 0x00 {
		log.Printf("[proxy-http] conn#%d: SOCKS5 greeting failed: %x %x", connID, greetResp[0], greetResp[1])
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Step 3: Send CONNECT request to SOCKS5
	connectReq := h.buildSOCKS5Connect(targetHost)
	if _, err := upstreamConn.Write(connectReq); err != nil {
		log.Printf("[proxy-http] conn#%d: write CONNECT to SOCKS5 failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Step 4: Read CONNECT response header (4 bytes: VER + REP + RSV + ATYP)
	respHeader := handshakeBuf[2:6]
	if _, err := io.ReadFull(upstreamConn, respHeader); err != nil {
		log.Printf("[proxy-http] conn#%d: read CONNECT response header failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	if respHeader[1] != 0x00 {
		log.Printf("[proxy-http] conn#%d: SOCKS5 CONNECT failed with status: %d", connID, respHeader[1])
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Step 5: Drain bind address + port based on ATYP
	atyp := respHeader[3]
	switch atyp {
	case 0x01: // IPv4
		io.ReadFull(upstreamConn, handshakeBuf[:6]) // 4 bytes IPv4 + 2 bytes port
	case 0x03: // Domain
		lenBuf := handshakeBuf[:1]
		io.ReadFull(upstreamConn, lenBuf)
		io.ReadFull(upstreamConn, handshakeBuf[:int(lenBuf[0])+2]) // domain + port
	case 0x04: // IPv6
		io.ReadFull(upstreamConn, handshakeBuf[:18]) // 16 bytes IPv6 + 2 bytes port
	default:
		io.ReadFull(upstreamConn, handshakeBuf[:6])
	}

	// Success! Send HTTP 200 to client
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Relay data between client and upstream SOCKS5
	h.relay(clientConn, upstreamConn, connID)
}

// handleHTTP handles direct HTTP requests (GET, POST, etc.)
func (h *HTTPServer) handleHTTP(clientConn net.Conn, req *http.Request, reader *bufio.Reader, connID uint64) {
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

	// Connect to SOCKS5 server
	upstreamConn, err := net.DialTimeout("tcp", h.socksAddr, 5*time.Second)
	if err != nil {
		log.Printf("[proxy-http] conn#%d: dial SOCKS5 server %s failed: %v", connID, h.socksAddr, err)
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

	// Use single handshake buffer to minimize allocations
	handshakeBuf := make([]byte, 1024)

	// Step 1: Send SOCKS5 greeting
	greetReq := []byte{0x05, 0x01, 0x00} // VER=5, NMETHODS=1, METHOD=0x00 (no auth)
	if _, err := upstreamConn.Write(greetReq); err != nil {
		log.Printf("[proxy-http] conn#%d: write greeting to SOCKS5 failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Step 2: Read greeting response (2 bytes)
	greetResp := handshakeBuf[:2]
	if _, err := io.ReadFull(upstreamConn, greetResp); err != nil {
		log.Printf("[proxy-http] conn#%d: read greeting response failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}
	if greetResp[0] != 0x05 || greetResp[1] != 0x00 {
		log.Printf("[proxy-http] conn#%d: SOCKS5 greeting failed: %x %x", connID, greetResp[0], greetResp[1])
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Step 3: Send SOCKS5 CONNECT request
	socksReq := h.buildSOCKS5Connect(targetHost)
	if _, err := upstreamConn.Write(socksReq); err != nil {
		log.Printf("[proxy-http] conn#%d: write CONNECT to SOCKS5 failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Step 4: Read CONNECT response header
	respHeader := handshakeBuf[2:6]
	if _, err := io.ReadFull(upstreamConn, respHeader); err != nil {
		log.Printf("[proxy-http] conn#%d: read CONNECT response header failed: %v", connID, err)
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	if respHeader[1] != 0x00 {
		log.Printf("[proxy-http] conn#%d: SOCKS5 CONNECT failed with status: %d", connID, respHeader[1])
		clientConn.Write([]byte("HTTP/1.0 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Step 5: Drain bind address + port
	atyp := respHeader[3]
	switch atyp {
	case 0x01: // IPv4
		io.ReadFull(upstreamConn, handshakeBuf[:6])
	case 0x03: // Domain
		lenBuf := handshakeBuf[:1]
		io.ReadFull(upstreamConn, lenBuf)
		io.ReadFull(upstreamConn, handshakeBuf[:int(lenBuf[0])+2])
	case 0x04: // IPv6
		io.ReadFull(upstreamConn, handshakeBuf[:18])
	default:
		io.ReadFull(upstreamConn, handshakeBuf[:6])
	}

	h.relay(clientConn, upstreamConn, connID)
}

// buildSOCKS5Connect builds SOCKS5 CONNECT request for target
// Returns: VER(1) CMD(1) RSV(1) ATYP(1) ADDR PORT
func (h *HTTPServer) buildSOCKS5Connect(targetHostPort string) []byte {
	// Parse host and port
	host, port, err := net.SplitHostPort(targetHostPort)
	if err != nil {
		// Assume HTTPS (443) if not specified
		host = targetHostPort
		port = "443"
	}

	var portNum uint16
	fmt.Sscanf(port, "%d", &portNum)

	// SOCKS5 CONNECT request
	req := []byte{0x05, 0x01, 0x00} // VER=5, CMD=1 (CONNECT), RSV=0

	// Try parsing as IPv4
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			req = append(req, 0x01)       // ATYP=1 (IPv4)
			req = append(req, ipv4...)    // 4 bytes
		} else {
			req = append(req, 0x04)       // ATYP=4 (IPv6)
			req = append(req, ip...)      // 16 bytes
		}
	} else {
		// Domain name
		req = append(req, 0x03)           // ATYP=3 (Domain)
		req = append(req, byte(len(host)))
		req = append(req, []byte(host)...)
	}

	// Add port (big-endian)
	portBytes := [2]byte{byte(portNum >> 8), byte(portNum & 0xFF)}
	req = append(req, portBytes[:]...)

	return req
}

// relay bidirectional copy between client and upstream
func (h *HTTPServer) relay(clientConn, upstreamConn net.Conn, connID uint64) {
	var clientToUpstream, upstreamToClient int64
	done := make(chan struct{}, 2)

	go func() {
		bufPtr := h.bufPool.Get().(*[]byte)
		n, _ := io.CopyBuffer(upstreamConn, clientConn, *bufPtr)
		h.bufPool.Put(bufPtr)
		clientToUpstream = n
		if tc, ok := upstreamConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	go func() {
		bufPtr := h.bufPool.Get().(*[]byte)
		n, _ := io.CopyBuffer(clientConn, upstreamConn, *bufPtr)
		h.bufPool.Put(bufPtr)
		upstreamToClient = n
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	<-done
	<-done

	// Only log at debug level to reduce hot path overhead
	if clientToUpstream > 0 || upstreamToClient > 0 {
		log.Printf("[proxy-http] conn#%d: relay closed (tx=%d, rx=%d)", connID, clientToUpstream, upstreamToClient)
	}
}
