// Package server implements the local HTTP/HTTPS forward-proxy that clients
// connect to. It speaks HTTP/1.1 and supports:
//
//   - CONNECT tunnelling (used by HTTPS and any TCP tunnel)
//   - Plain HTTP forwarding (GET/POST/… for http:// targets)
//   - Optional Proxy-Authorization basic auth
//   - Drain-on-rotate: existing connections finish on the proxy they started
//     on; new connections always pick the current rotator proxy.
package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/romeomihailus/proxyrotator/internal/rotator"
	"github.com/romeomihailus/proxyrotator/internal/upstream"
)

// Config holds proxy server settings.
type Config struct {
	// ListenAddr is the address for the proxy to bind on (e.g. "0.0.0.0:8080").
	ListenAddr string

	// Username and Password for Proxy-Authorization. Both must be non-empty
	// to enable authentication.
	Username string
	Password string

	// DialTimeout is the maximum time to dial through the upstream proxy.
	DialTimeout time.Duration
}

// Server is the local HTTP proxy server.
type Server struct {
	cfg     Config
	rotator *rotator.Rotator
	ln      net.Listener
}

// New creates a Server. Call Start to begin accepting connections.
func New(cfg Config, r *rotator.Rotator) *Server {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 30 * time.Second
	}
	return &Server{cfg: cfg, rotator: r}
}

// Start begins listening and serving. Blocks until the listener is closed.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.ln = ln
	log.Printf("[server] proxy listening on %s", s.cfg.ListenAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed — normal shutdown
			return err
		}
		go s.handleConn(conn)
	}
}

// Stop closes the listener.
func (s *Server) Stop() error {
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

// -----------------------------------------------------------------------
// Connection handling
// -----------------------------------------------------------------------

func (s *Server) handleConn(clientConn net.Conn) {
	defer clientConn.Close()

	br := bufio.NewReader(clientConn)
	req, err := http.ReadRequest(br)
	if err != nil {
		if err != io.EOF {
			log.Printf("[server] read request: %v", err)
		}
		return
	}

	// Check auth before doing anything else
	if s.authRequired() && !s.checkAuth(req) {
		resp := &http.Response{
			StatusCode: http.StatusProxyAuthRequired,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
		}
		resp.Header.Set("Proxy-Authenticate", `Basic realm="proxyrotator"`)
		resp.Header.Set("Content-Length", "0")
		_ = resp.Write(clientConn)
		return
	}

	if req.Method == http.MethodConnect {
		s.handleCONNECT(clientConn, req)
	} else {
		s.handleHTTP(clientConn, br, req)
	}
}

// handleCONNECT tunnels a raw TCP connection through the upstream proxy.
// This is used for HTTPS and anything that needs a transparent tunnel.
func (s *Server) handleCONNECT(clientConn net.Conn, req *http.Request) {
	destination := req.Host // "host:port"
	if !hasPort(destination) {
		destination += ":443"
	}

	// Select proxy for this destination (honours domain pinning)
	px := s.rotator.ProxyFor(destination)
	if px == nil {
		writeError(clientConn, http.StatusBadGateway, "no available upstream proxy")
		return
	}

	// Track active connection on this specific proxy instance.
	// Drain semantics: the rotator can switch "current" at any time; the
	// existing connection continues on the proxy it grabbed here.
	px.ActiveConns.Add(1)
	defer px.ActiveConns.Add(-1)

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.DialTimeout)
	defer cancel()

	upstreamConn, err := upstream.Dial(ctx, px.URL, destination)
	if err != nil {
		s.rotator.RecordConnError()
		log.Printf("[server] CONNECT upstream dial failed (proxy=%s dest=%s): %v", px.String(), destination, err)
		writeError(clientConn, http.StatusBadGateway, fmt.Sprintf("upstream dial: %v", err))
		return
	}
	defer upstreamConn.Close()

	// Acknowledge tunnel establishment
	_, _ = fmt.Fprintf(clientConn, "HTTP/1.1 200 Connection established\r\n\r\n")

	s.rotator.RecordRequest()
	s.tunnel(clientConn, upstreamConn)
}

// handleHTTP forwards a plain HTTP request through the upstream proxy.
// The upstream proxy handles all HTTP semantics; we just relay bytes.
func (s *Server) handleHTTP(clientConn net.Conn, br *bufio.Reader, req *http.Request) {
	destination := req.URL.Host
	if destination == "" {
		destination = req.Host
	}
	if !hasPort(destination) {
		destination += ":80"
	}

	px := s.rotator.ProxyFor(destination)
	if px == nil {
		writeError(clientConn, http.StatusBadGateway, "no available upstream proxy")
		return
	}

	px.ActiveConns.Add(1)
	defer px.ActiveConns.Add(-1)

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.DialTimeout)
	defer cancel()

	upstreamConn, err := upstream.Dial(ctx, px.URL, destination)
	if err != nil {
		s.rotator.RecordConnError()
		log.Printf("[server] HTTP upstream dial failed (proxy=%s dest=%s): %v", px.String(), destination, err)
		writeError(clientConn, http.StatusBadGateway, fmt.Sprintf("upstream dial: %v", err))
		return
	}
	defer upstreamConn.Close()

	// Remove proxy-specific headers before forwarding
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")

	if err := req.Write(upstreamConn); err != nil {
		s.rotator.RecordConnError()
		log.Printf("[server] write HTTP request to upstream: %v", err)
		return
	}

	s.rotator.RecordRequest()
	s.tunnel(clientConn, upstreamConn)
}

// tunnel performs a bidirectional copy between two connections until
// either side closes.
func (s *Server) tunnel(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copy := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Half-close to unblock the other goroutine
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go copy(a, b)
	go copy(b, a)
	<-done
	<-done
}

// -----------------------------------------------------------------------
// Auth helpers
// -----------------------------------------------------------------------

func (s *Server) authRequired() bool {
	return s.cfg.Username != "" && s.cfg.Password != ""
}

func (s *Server) checkAuth(req *http.Request) bool {
	auth := req.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return false
	}
	return parts[0] == s.cfg.Username && parts[1] == s.cfg.Password
}

// -----------------------------------------------------------------------
// Misc helpers
// -----------------------------------------------------------------------

func writeError(conn net.Conn, code int, msg string) {
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
		code, http.StatusText(code))
	_, _ = fmt.Fprintf(conn, "%s", resp)
	log.Printf("[server] error %d: %s", code, msg)
}

func hasPort(host string) bool {
	_, _, err := net.SplitHostPort(host)
	return err == nil
}
