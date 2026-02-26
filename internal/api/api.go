// Package api exposes a lightweight HTTP API for external integrations.
//
// Endpoints
//
//	POST /api/rotate          Force an immediate proxy rotation.
//	POST /api/status          Report an HTTP status code from the crawler.
//	GET  /api/pool            List all proxies and their current state.
//	GET  /api/current         Return the currently active proxy.
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/drsoft-oss/proxyrotator/internal/pool"
	"github.com/drsoft-oss/proxyrotator/internal/rotator"
)

// Server is the API HTTP server.
type Server struct {
	pool    *pool.Pool
	rotator *rotator.Rotator
	server  *http.Server
}

// New creates and configures the API server.
func New(addr string, p *pool.Pool, r *rotator.Rotator) *Server {
	s := &Server{pool: p, rotator: r}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/rotate", s.handleRotate)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/pool", s.handlePool)
	mux.HandleFunc("/api/current", s.handleCurrent)

	s.server = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	return s
}

// Start begins listening. Blocks until the server stops.
func (s *Server) Start() error {
	return s.server.ListenAndServe()
}

// Stop shuts down the server gracefully.
func (s *Server) Stop() error {
	return s.server.Close()
}

// -----------------------------------------------------------------------
// Request / Response types
// -----------------------------------------------------------------------

// StatusRequest is the payload for POST /api/status.
type StatusRequest struct {
	// Status is the HTTP status code received by the crawler.
	Status int `json:"status"`
	// Destination is the target domain (host or host:port).
	Destination string `json:"destination"`
}

// ProxyInfo is a serialisable snapshot of a single proxy's state.
type ProxyInfo struct {
	ID          int64         `json:"id"`
	Address     string        `json:"address"`
	Scheme      string        `json:"scheme"`
	Alive       bool          `json:"alive"`
	Latency     string        `json:"latency_ms"`
	ActiveConns int64         `json:"active_conns"`
	ReqCount    int64         `json:"req_count"`
	ConnErrors  int64         `json:"conn_errors"`
	HTTPErrors  int64         `json:"http_errors"`
}

// -----------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------

// handleRotate triggers an immediate rotation.
//
//	POST /api/rotate
//	Response: {"ok": true, "proxy": "<new proxy address>"}
func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.rotator.ForceRotate()
	// Give the rotation goroutine a moment to complete before reading current
	time.Sleep(50 * time.Millisecond)
	cur := s.rotator.Current()
	addr := ""
	if cur != nil {
		addr = cur.String()
	}
	log.Printf("[api] manual rotation triggered; new proxy: %s", addr)
	jsonOK(w, map[string]any{"ok": true, "proxy": addr})
}

// handleStatus receives an HTTP status code report from the crawler.
//
//	POST /api/status
//	Body: {"status": 403, "destination": "example.com"}
//	Response: {"ok": true, "rotated": false}
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req StatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	if req.Destination == "" {
		http.Error(w, "destination is required", http.StatusBadRequest)
		return
	}

	// 2xx and 3xx are healthy — ignore
	if req.Status >= 200 && req.Status < 400 {
		jsonOK(w, map[string]any{"ok": true, "rotated": false})
		return
	}

	genBefore := s.rotator.Generation()
	s.rotator.RecordHTTPError(req.Destination)
	rotated := s.rotator.Generation() != genBefore

	log.Printf("[api] status report: %d for %s (rotated=%v)", req.Status, req.Destination, rotated)
	jsonOK(w, map[string]any{"ok": true, "rotated": rotated})
}

// handlePool returns the full proxy pool state.
//
//	GET /api/pool
func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	proxies := s.pool.All()
	cur := s.rotator.Current()
	var infos []ProxyInfo
	for _, px := range proxies {
		info := proxyToInfo(px)
		if cur != nil && px.ID == cur.ID {
			info.Address = "[ACTIVE] " + info.Address
		}
		infos = append(infos, info)
	}
	jsonOK(w, infos)
}

// handleCurrent returns the currently active proxy.
//
//	GET /api/current
func (s *Server) handleCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cur := s.rotator.Current()
	if cur == nil {
		http.Error(w, "no active proxy", http.StatusServiceUnavailable)
		return
	}
	jsonOK(w, proxyToInfo(cur))
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[api] encode response: %v", err)
	}
}

func proxyToInfo(px *pool.Proxy) ProxyInfo {
	lat := px.Latency()
	latStr := "0"
	if lat > 0 {
		latStr = fmt.Sprintf("%d", lat.Milliseconds())
	}
	return ProxyInfo{
		ID:          px.ID,
		Address:     px.String(),
		Scheme:      px.Scheme,
		Alive:       px.IsAlive(),
		Latency:     latStr,
		ActiveConns: px.ActiveConns.Load(),
		ReqCount:    px.ReqCount.Load(),
		ConnErrors:  px.ConnErrors.Load(),
		HTTPErrors:  px.HTTPErrors.Load(),
	}
}
