// Package pool manages the set of upstream proxies.
// It tracks liveness, latency, and connection counts.
package pool

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy represents one upstream proxy endpoint.
type Proxy struct {
	URL *url.URL // original parsed URL

	// Identity (immutable after creation)
	ID     int64
	Scheme string // "http", "https", "socks5"
	Host   string // host:port

	// Liveness (protected by mu)
	mu      sync.RWMutex
	alive   bool
	latency time.Duration

	// Atomic counters — hot path, no lock needed
	ActiveConns  atomic.Int64 // currently tunneling connections
	ReqCount     atomic.Int64 // total requests served by this proxy
	ConnErrors   atomic.Int64 // ECONNRESET / handshake failures
	HTTPErrors   atomic.Int64 // non-2xx/3xx responses reported via API
}

// IsAlive returns whether the proxy is considered healthy.
func (p *Proxy) IsAlive() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.alive
}

// SetAlive updates the liveness flag.
func (p *Proxy) SetAlive(v bool) {
	p.mu.Lock()
	p.alive = v
	p.mu.Unlock()
}

// Latency returns the last measured latency.
func (p *Proxy) Latency() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.latency
}

// SetLatency updates the measured latency.
func (p *Proxy) SetLatency(d time.Duration) {
	p.mu.Lock()
	p.latency = d
	p.mu.Unlock()
}

// ResetErrorCounters zeros out per-rotation error counters.
func (p *Proxy) ResetErrorCounters() {
	p.ConnErrors.Store(0)
	p.HTTPErrors.Store(0)
	p.ReqCount.Store(0)
}

// String returns a human-readable representation.
func (p *Proxy) String() string {
	u := *p.URL
	if u.User != nil {
		u.User = url.UserPassword("***", "***")
	}
	return u.String()
}

// Pool holds all known upstream proxies and keeps them sorted by latency.
type Pool struct {
	mu      sync.RWMutex
	proxies []*Proxy
	nextID  atomic.Int64

	latencySort bool // if false, keep original file order
}

// New creates an empty pool.
func New(latencySort bool) *Pool {
	return &Pool{latencySort: latencySort}
}

// LoadFile parses a proxy list file (one URI per line) and populates the pool.
// Lines starting with '#' or empty lines are ignored.
// Supported schemes: http://, https://, socks5://
func (p *Pool) LoadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open proxy file: %w", err)
	}
	defer f.Close()

	var proxies []*Proxy
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxy, err := parseProxy(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: skip invalid proxy %q: %v\n", line, err)
			continue
		}
		proxy.ID = p.nextID.Add(1)
		proxy.alive = true // assume alive initially; monitor will correct
		proxies = append(proxies, proxy)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read proxy file: %w", err)
	}
	if len(proxies) == 0 {
		return fmt.Errorf("proxy file contains no valid entries")
	}

	p.mu.Lock()
	p.proxies = proxies
	p.mu.Unlock()
	return nil
}

// parseProxy parses a single proxy URI line.
func parseProxy(raw string) (*Proxy, error) {
	// Allow bare host:port → assume http
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5":
	default:
		return nil, fmt.Errorf("unsupported scheme %q (use http, https, socks5)", scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host")
	}
	return &Proxy{
		URL:    u,
		Scheme: scheme,
		Host:   u.Host,
	}, nil
}

// All returns a snapshot of all proxies (alive or not).
func (p *Pool) All() []*Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Proxy, len(p.proxies))
	copy(out, p.proxies)
	return out
}

// Alive returns alive proxies. If latencySort is enabled, sorted by latency
// ascending (fastest first, zeros last so unprobed proxies don't front the queue).
func (p *Pool) Alive() []*Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var out []*Proxy
	for _, px := range p.proxies {
		if px.IsAlive() {
			out = append(out, px)
		}
	}
	if p.latencySort && len(out) > 1 {
		sort.Slice(out, func(i, j int) bool {
			li := out[i].Latency()
			lj := out[j].Latency()
			// Push un-probed (zero latency) to the back
			if li == 0 {
				return false
			}
			if lj == 0 {
				return true
			}
			return li < lj
		})
	}
	return out
}

// Len returns the total number of proxies in the pool.
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

// AliveLen returns the number of alive proxies.
func (p *Pool) AliveLen() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, px := range p.proxies {
		if px.IsAlive() {
			count++
		}
	}
	return count
}
