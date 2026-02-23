// Package rotator manages the active proxy selection and all rotation triggers.
//
// Rotation sources:
//   - Time interval  (--rotate-interval)
//   - Request count  (--rotate-requests)
//   - Conn errors    (--rotate-conn-errors) — ECONNRESET / handshake failures
//   - HTTP errors    (--rotate-http-errors) — non-2xx/3xx codes reported via API
//   - Manual         (POST /api/rotate)
//
// On rotation the old proxy is drained (new connections go to the new proxy;
// existing connections on the old proxy are allowed to finish naturally).
package rotator

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/romeomihailus/proxyrotator/internal/pool"
)

// Config holds all rotation thresholds.
type Config struct {
	// RotateInterval rotates the proxy on a fixed wall-clock schedule.
	// Zero disables interval-based rotation.
	RotateInterval time.Duration

	// RotateRequests rotates after this many requests have been served.
	// Zero disables request-count rotation.
	RotateRequests int64

	// RotateConnErrors rotates after this many connection-level errors
	// (e.g. ECONNRESET, TLS handshake failure) on the current proxy.
	// Zero disables.
	RotateConnErrors int64

	// RotateHTTPErrors rotates after this many non-2xx/3xx HTTP status codes
	// are reported through the API for the current proxy.
	// Zero disables.
	RotateHTTPErrors int64

	// HTTPErrorDedupWindow is the duration within which identical
	// destination errors are counted only once (prevents request-queue
	// flooding from triggering multiple rotations for the same event).
	// Defaults to 2 seconds when zero.
	HTTPErrorDedupWindow time.Duration
}

// Rotator selects and rotates the active upstream proxy.
type Rotator struct {
	pool *pool.Pool
	cfg  Config

	mu          sync.RWMutex
	current     *pool.Proxy // currently active proxy
	poolIndex   int         // index into pool.Alive() slice
	generation  int64       // increments on every rotation
	rotatedAt   time.Time   // wall-clock time of last rotation

	// Domain pinning: domain → pinned proxy (session-scoped).
	// Cleared automatically when the pinned proxy is rotated out.
	pins   map[string]*pool.Proxy
	pinsMu sync.RWMutex

	// HTTP error deduplication: tracks recently-seen (destination) entries.
	recentHTTPErrors   map[string]time.Time
	recentHTTPErrorsMu sync.Mutex

	// Channel used internally to trigger a rotation from any goroutine.
	rotateCh chan string // value = reason string (for logging)

	stop chan struct{}
	wg   sync.WaitGroup
}

// New creates a Rotator and immediately picks the first proxy.
func New(p *pool.Pool, cfg Config) (*Rotator, error) {
	if cfg.HTTPErrorDedupWindow == 0 {
		cfg.HTTPErrorDedupWindow = 2 * time.Second
	}

	r := &Rotator{
		pool:             p,
		cfg:              cfg,
		pins:             make(map[string]*pool.Proxy),
		recentHTTPErrors: make(map[string]time.Time),
		rotateCh:         make(chan string, 16),
		stop:             make(chan struct{}),
	}

	if err := r.pickNext("startup"); err != nil {
		return nil, fmt.Errorf("no alive proxies in pool: %w", err)
	}
	return r, nil
}

// Current returns the currently active proxy.
func (r *Rotator) Current() *pool.Proxy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// Generation returns the rotation generation counter.
// Callers can use this to detect whether the active proxy changed between
// two points in time without holding the lock.
func (r *Rotator) Generation() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generation
}

// ProxyFor returns the proxy that should be used for a given destination
// hostname. If the domain is pinned to a still-alive proxy, that proxy is
// returned. Otherwise the current global proxy is returned (and the domain
// is pinned to it for the rest of the session).
func (r *Rotator) ProxyFor(destination string) *pool.Proxy {
	domain := extractDomain(destination)

	r.pinsMu.Lock()
	defer r.pinsMu.Unlock()

	if px, ok := r.pins[domain]; ok && px.IsAlive() {
		return px
	}

	// No valid pin — use (and pin) the current proxy.
	cur := r.Current()
	if cur != nil {
		r.pins[domain] = cur
	}
	return cur
}

// ForceRotate queues a manual rotation.
func (r *Rotator) ForceRotate() {
	r.rotateCh <- "manual"
}

// RecordRequest increments the request counter for the current proxy
// and triggers a rotation if the request threshold is reached.
func (r *Rotator) RecordRequest() {
	r.mu.RLock()
	cur := r.current
	r.mu.RUnlock()
	if cur == nil {
		return
	}
	n := cur.ReqCount.Add(1)
	if r.cfg.RotateRequests > 0 && n >= r.cfg.RotateRequests {
		r.rotateCh <- fmt.Sprintf("request-count=%d", n)
	}
}

// RecordConnError increments the connection error counter for the current
// proxy and triggers rotation when the threshold is exceeded.
func (r *Rotator) RecordConnError() {
	r.mu.RLock()
	cur := r.current
	r.mu.RUnlock()
	if cur == nil {
		return
	}
	n := cur.ConnErrors.Add(1)
	if r.cfg.RotateConnErrors > 0 && n >= r.cfg.RotateConnErrors {
		r.rotateCh <- fmt.Sprintf("conn-errors=%d", n)
	}
}

// RecordHTTPError is called by the API when the crawler reports a non-2xx/3xx
// response for a given destination. It deduplicates within the configured
// window to handle queued requests all using the same (soon-to-be-rotated)
// proxy.
func (r *Rotator) RecordHTTPError(destination string) {
	if r.cfg.RotateHTTPErrors <= 0 {
		return
	}

	domain := extractDomain(destination)
	window := r.cfg.HTTPErrorDedupWindow

	r.recentHTTPErrorsMu.Lock()
	last, seen := r.recentHTTPErrors[domain]
	if seen && time.Since(last) < window {
		// Already counted this destination within the dedup window — skip.
		r.recentHTTPErrorsMu.Unlock()
		return
	}
	r.recentHTTPErrors[domain] = time.Now()
	r.recentHTTPErrorsMu.Unlock()

	// Check if we rotated recently (grace period = dedup window).
	// If so, the error almost certainly belongs to the old proxy.
	// We skip the grace period on the very first proxy selection (rotatedAt
	// is zero, meaning no rotation has actually happened yet).
	r.mu.RLock()
	rotatedAt := r.rotatedAt
	cur := r.current
	r.mu.RUnlock()

	if !rotatedAt.IsZero() && time.Since(rotatedAt) < window {
		return
	}
	if cur == nil {
		return
	}

	n := cur.HTTPErrors.Add(1)
	if n >= r.cfg.RotateHTTPErrors {
		r.rotateCh <- fmt.Sprintf("http-errors=%d destination=%s", n, domain)
	}
}

// Start launches background goroutines for interval rotation.
// Call Stop to shut them down.
func (r *Rotator) Start() {
	if r.cfg.RotateInterval > 0 {
		r.wg.Add(1)
		go r.intervalLoop()
	}
	r.wg.Add(1)
	go r.rotationLoop()
}

// Stop shuts down background goroutines.
func (r *Rotator) Stop() {
	close(r.stop)
	r.wg.Wait()
}

// -----------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------

// rotationLoop drains the rotateCh and performs the actual rotation.
// Coalesces rapid back-to-back rotation requests.
func (r *Rotator) rotationLoop() {
	defer r.wg.Done()
	for {
		select {
		case reason := <-r.rotateCh:
			// Drain any additional pending requests — if multiple triggers
			// fired at once, we only need one rotation.
		drain:
			for {
				select {
				case extra := <-r.rotateCh:
					reason += "+" + extra
				default:
					break drain
				}
			}
			if err := r.pickNext(reason); err != nil {
				log.Printf("[rotator] rotation failed (%s): %v", reason, err)
			}
		case <-r.stop:
			return
		}
	}
}

func (r *Rotator) intervalLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.RotateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.rotateCh <- "interval"
		case <-r.stop:
			return
		}
	}
}

// pickNext selects the next proxy from the alive pool (round-robin) and
// updates the current proxy without killing in-flight connections.
func (r *Rotator) pickNext(reason string) error {
	alive := r.pool.Alive()
	if len(alive) == 0 {
		return fmt.Errorf("no alive proxies")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Move to next index (wrapping)
	if r.current == nil {
		r.poolIndex = 0
	} else {
		// Find current proxy in alive list to keep position meaningful
		cur := r.current
		found := -1
		for i, px := range alive {
			if px == cur {
				found = i
				break
			}
		}
		if found >= 0 {
			r.poolIndex = (found + 1) % len(alive)
		} else {
			// Current proxy not alive anymore — start from index 0
			r.poolIndex = 0
		}
	}

	prev := r.current
	r.current = alive[r.poolIndex]
	r.generation++
	// Only stamp the rotation time when we're actually switching away from a
	// previous proxy. On the very first call (startup) prev is nil and no
	// grace period should apply to incoming error reports.
	if prev != nil {
		r.rotatedAt = time.Now()
	}

	// Reset error counters on the newly activated proxy
	r.current.ResetErrorCounters()

	// Invalidate any domain pins that pointed to the old proxy
	if prev != nil && prev != r.current {
		r.pinsMu.Lock()
		for domain, px := range r.pins {
			if px == prev {
				delete(r.pins, domain)
			}
		}
		r.pinsMu.Unlock()
	}

	prevStr := "<none>"
	if prev != nil {
		prevStr = prev.String()
	}
	log.Printf("[rotator] rotation #%d (%s): %s → %s (active_conns_old=%d)",
		r.generation, reason, prevStr, r.current.String(),
		func() int64 {
			if prev != nil {
				return prev.ActiveConns.Load()
			}
			return 0
		}(),
	)
	return nil
}

// extractDomain strips the port from a host:port destination string.
func extractDomain(destination string) string {
	// destination may be "example.com:443" or just "example.com"
	idx := strings.LastIndex(destination, ":")
	if idx < 0 {
		return strings.ToLower(destination)
	}
	return strings.ToLower(destination[:idx])
}
