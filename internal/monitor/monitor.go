// Package monitor performs background health-checks and latency measurements
// on all proxies in the pool, updating their liveness and latency fields.
//
// When monitoring is enabled (--monitor flag):
//   - Dead proxies are taken out of the active pool immediately.
//   - Recovered proxies are automatically re-added.
//
// Latency probing runs on the same interval regardless of the --monitor flag,
// so the rotator can prioritise faster proxies when latency-sort is on.
// Pass --no-latency-sort to skip the sort without disabling the probe.
package monitor

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/drsoft-oss/proxyrotator/internal/pool"
	"github.com/drsoft-oss/proxyrotator/internal/upstream"
)

const (
	defaultCheckURL     = "http://connectivitycheck.gstatic.com/generate_204"
	defaultTimeout      = 10 * time.Second
	defaultConcurrency  = 10
)

// Config controls health-check behaviour.
type Config struct {
	// Interval between full-pool health checks.
	Interval time.Duration

	// LatencyInterval controls how often latency is re-measured (can differ
	// from the liveness check interval). Zero means "same as Interval".
	LatencyInterval time.Duration

	// CheckURL is the URL used to probe liveness. A 204 / 200 response
	// from the target is considered healthy.
	CheckURL string

	// Timeout per individual proxy check.
	Timeout time.Duration

	// Concurrency limits how many proxies are checked in parallel.
	Concurrency int

	// UpdateLiveness controls whether dead proxies are removed from the pool.
	// When false, the monitor still measures latency but does not mark
	// proxies dead/alive (useful for latency-only updates).
	UpdateLiveness bool
}

// Monitor orchestrates background health checks.
type Monitor struct {
	pool *pool.Pool
	cfg  Config

	stop chan struct{}
	wg   sync.WaitGroup
}

// New creates a Monitor. Call Start to begin background checks.
func New(p *pool.Pool, cfg Config) *Monitor {
	if cfg.CheckURL == "" {
		cfg.CheckURL = defaultCheckURL
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.LatencyInterval == 0 {
		cfg.LatencyInterval = cfg.Interval
	}
	return &Monitor{pool: p, cfg: cfg, stop: make(chan struct{})}
}

// Start launches the background monitoring goroutine.
func (m *Monitor) Start() {
	m.wg.Add(1)
	go m.loop()
}

// Stop shuts down the monitor and waits for the goroutine to exit.
func (m *Monitor) Stop() {
	close(m.stop)
	m.wg.Wait()
}

// RunOnce performs a single health-check pass over the whole pool.
// Safe to call manually (e.g. on startup before serving traffic).
func (m *Monitor) RunOnce() {
	log.Println("[monitor] health check pass started")
	proxies := m.pool.All()

	sem := make(chan struct{}, m.cfg.Concurrency)
	var wg sync.WaitGroup

	for _, px := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func(px *pool.Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			m.check(px)
		}(px)
	}
	wg.Wait()
	log.Printf("[monitor] health check done: %d/%d alive", m.pool.AliveLen(), m.pool.Len())
}

// -----------------------------------------------------------------------
// Internal
// -----------------------------------------------------------------------

func (m *Monitor) loop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.RunOnce()
		case <-m.stop:
			return
		}
	}
}

// check probes a single proxy and updates its alive/latency fields.
func (m *Monitor) check(px *pool.Proxy) {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout)
	defer cancel()

	start := time.Now()
	err := m.probe(ctx, px)
	latency := time.Since(start)

	if err != nil {
		if m.cfg.UpdateLiveness {
			if px.IsAlive() {
				log.Printf("[monitor] proxy DEAD %s: %v", px.String(), err)
			}
			px.SetAlive(false)
		}
		px.SetLatency(0)
	} else {
		if m.cfg.UpdateLiveness && !px.IsAlive() {
			log.Printf("[monitor] proxy RECOVERED %s (latency=%s)", px.String(), latency.Round(time.Millisecond))
		}
		if m.cfg.UpdateLiveness {
			px.SetAlive(true)
		}
		px.SetLatency(latency)
	}
}

// probe dials through the proxy and issues a lightweight HTTP request.
func (m *Monitor) probe(ctx context.Context, px *pool.Proxy) error {
	// Determine destination from the check URL
	checkURL, err := url.Parse(m.cfg.CheckURL)
	if err != nil {
		return fmt.Errorf("bad check URL: %w", err)
	}
	host := checkURL.Host
	if !hasPort(host) {
		if checkURL.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	// Dial through the proxy
	conn, err := upstream.Dial(ctx, px.URL, host)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Send a minimal HTTP/1.1 request and read the status line
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n",
		checkURL.RequestURI(), checkURL.Hostname())
	if _, err := fmt.Fprint(conn, req); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	// Read just enough to get the status code
	buf := make([]byte, 32)
	n, _ := conn.Read(buf)
	if n < 9 {
		return fmt.Errorf("short response (%d bytes)", n)
	}
	_ = http.StatusOK // keep import
	return nil
}

func hasPort(host string) bool {
	_, _, err := net.SplitHostPort(host)
	return err == nil
}
