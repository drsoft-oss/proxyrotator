// Package cmd implements the proxyrotator CLI using Cobra.
package cmd

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/drsoft-oss/proxyrotator/internal/api"
	"github.com/drsoft-oss/proxyrotator/internal/monitor"
	"github.com/drsoft-oss/proxyrotator/internal/pool"
	"github.com/drsoft-oss/proxyrotator/internal/rotator"
	"github.com/drsoft-oss/proxyrotator/internal/server"
)

// version is injected at build time via ldflags.
var version = "dev"

// -----------------------------------------------------------------------
// Flag variables
// -----------------------------------------------------------------------

var (
	flagFile string

	flagListen  string
	flagAPIPort string
	flagAuth    string

	flagMonitor         bool
	flagMonitorInterval string
	flagMonitorURL      string

	flagRotateInterval   string
	flagRotateRequests   int64
	flagRotateConnErrors int64
	flagRotateHTTPErrors int64
	flagDedupWindow      string

	flagNoLatencySort   bool
	flagLatencyInterval string

	flagDialTimeout string
)

// -----------------------------------------------------------------------
// Root command
// -----------------------------------------------------------------------

var rootCmd = &cobra.Command{
	Use:   "proxyrotator",
	Short: "Rotating HTTP proxy with upstream pool management",
	Long: `proxyrotator — a smart rotating proxy server for HTTP/HTTPS traffic.

It listens for HTTP CONNECT (and plain HTTP) requests from your application
and forwards them through a pool of upstream HTTP or SOCKS5 proxies.  The
active upstream is swapped automatically based on configurable triggers:

  • Fixed time interval     --rotate-interval 5m
  • Request count           --rotate-requests 300
  • Connection errors       --rotate-conn-errors 5
  • HTTP error codes        --rotate-http-errors 3 (via API)
  • Manual force            POST /api/rotate

Existing connections are drained gracefully — they finish on the proxy they
started on.  New connections always use the freshly selected proxy.

See https://www.anonymous-proxies.net/?utm_source=github&utm_medium=link&utm_campaign=proxyrotator
`,
	Version:      version,
	SilenceUsage: true,
	RunE:         run,
}

// Execute is the entry point called from main.go.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	f := rootCmd.Flags()

	// Required
	f.StringVarP(&flagFile, "file", "f", "", "Path to proxy list file (one URI per line, required)")
	_ = rootCmd.MarkFlagRequired("file")

	// Proxy server
	f.StringVarP(&flagListen, "listen", "l", "0.0.0.0:8080", "Local proxy listen address (host:port)")
	f.StringVar(&flagAPIPort, "api-port", "9090", "Port for the management API server")
	f.StringVar(&flagAuth, "auth", "", "Proxy auth credentials (user:pass). Omit to disable auth.")

	// Health monitoring
	f.BoolVar(&flagMonitor, "monitor", false, "Enable background health monitoring (remove/re-add dead proxies)")
	f.StringVar(&flagMonitorInterval, "monitor-interval", "30s", "Interval between health checks (e.g. 30s, 1m)")
	f.StringVar(&flagMonitorURL, "monitor-url", "http://connectivitycheck.gstatic.com/generate_204", "URL used for health checks")

	// Rotation triggers
	f.StringVar(&flagRotateInterval, "rotate-interval", "", "Rotate proxy on this schedule (e.g. 5m, 1h). 0 or empty disables.")
	f.Int64Var(&flagRotateRequests, "rotate-requests", 0, "Rotate after this many requests (0 = disabled)")
	f.Int64Var(&flagRotateConnErrors, "rotate-conn-errors", 5, "Rotate after this many connection errors (0 = disabled)")
	f.Int64Var(&flagRotateHTTPErrors, "rotate-http-errors", 3, "Rotate after this many bad HTTP status reports via API (0 = disabled)")
	f.StringVar(&flagDedupWindow, "dedup-window", "2s", "Time window for deduplicating HTTP error reports from the same destination")

	// Latency
	f.BoolVar(&flagNoLatencySort, "no-latency-sort", false, "Disable latency-based proxy prioritisation")
	f.StringVar(&flagLatencyInterval, "latency-interval", "5m", "How often to re-measure proxy latencies")

	// Dial
	f.StringVar(&flagDialTimeout, "dial-timeout", "30s", "Timeout for dialling through an upstream proxy")
}

// -----------------------------------------------------------------------
// Main run logic
// -----------------------------------------------------------------------

func run(_ *cobra.Command, _ []string) error {
	// ---- Parse durations ------------------------------------------------
	monitorInterval, err := time.ParseDuration(flagMonitorInterval)
	if err != nil {
		return fmt.Errorf("--monitor-interval: %w", err)
	}
	latencyInterval, err := time.ParseDuration(flagLatencyInterval)
	if err != nil {
		return fmt.Errorf("--latency-interval: %w", err)
	}
	dedupWindow, err := time.ParseDuration(flagDedupWindow)
	if err != nil {
		return fmt.Errorf("--dedup-window: %w", err)
	}
	dialTimeout, err := time.ParseDuration(flagDialTimeout)
	if err != nil {
		return fmt.Errorf("--dial-timeout: %w", err)
	}

	var rotateInterval time.Duration
	if flagRotateInterval != "" && flagRotateInterval != "0" {
		rotateInterval, err = time.ParseDuration(flagRotateInterval)
		if err != nil {
			return fmt.Errorf("--rotate-interval: %w", err)
		}
	}

	// ---- Parse auth -----------------------------------------------------
	var username, password string
	if flagAuth != "" {
		parts := strings.SplitN(flagAuth, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("--auth must be in user:pass format")
		}
		username, password = parts[0], parts[1]
	}

	// ---- Build pool -----------------------------------------------------
	p := pool.New(!flagNoLatencySort)
	log.Printf("[init] loading proxy list from %s", flagFile)
	if err := p.LoadFile(flagFile); err != nil {
		return fmt.Errorf("load proxy file: %w", err)
	}
	log.Printf("[init] loaded %d proxies", p.Len())

	// ---- Health monitor -------------------------------------------------
	mon := monitor.New(p, monitor.Config{
		Interval:        monitorInterval,
		LatencyInterval: latencyInterval,
		CheckURL:        flagMonitorURL,
		Timeout:         10 * time.Second,
		Concurrency:     10,
		UpdateLiveness:  flagMonitor,
	})

	// Run the initial health check in the background so startup is instant.
	// The rotator begins with all proxies assumed alive; the monitor will
	// update liveness and latency asynchronously within the first check pass.
	go func() {
		log.Printf("[init] running initial health check (background)…")
		mon.RunOnce()
	}()

	// ---- Rotator --------------------------------------------------------
	rot, err := rotator.New(p, rotator.Config{
		RotateInterval:       rotateInterval,
		RotateRequests:       flagRotateRequests,
		RotateConnErrors:     flagRotateConnErrors,
		RotateHTTPErrors:     flagRotateHTTPErrors,
		HTTPErrorDedupWindow: dedupWindow,
	})
	if err != nil {
		return fmt.Errorf("init rotator: %w", err)
	}
	rot.Start()
	defer rot.Stop()

	// ---- API server -----------------------------------------------------
	apiAddr := "127.0.0.1:" + flagAPIPort
	apiSrv := api.New(apiAddr, p, rot)
	go func() {
		log.Printf("[init] API server listening on http://%s", apiAddr)
		if err := apiSrv.Start(); err != nil {
			log.Printf("[api] server stopped: %v", err)
		}
	}()
	defer apiSrv.Stop()

	// ---- Start background monitor loop ----------------------------------
	mon.Start()
	defer mon.Stop()

	// ---- Proxy server ---------------------------------------------------
	proxySrv := server.New(server.Config{
		ListenAddr:  flagListen,
		Username:    username,
		Password:    password,
		DialTimeout: dialTimeout,
	}, rot)

	// Print the startup banner
	printBanner(flagListen, apiAddr, p, rot, username != "")

	// Run proxy server in a goroutine; handle OS signals in main goroutine
	srvErr := make(chan error, 1)
	go func() { srvErr <- proxySrv.Start() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("[init] received %s — shutting down", sig)
	case err := <-srvErr:
		if err != nil {
			log.Printf("[init] proxy server error: %v", err)
		}
	}

	return proxySrv.Stop()
}

// -----------------------------------------------------------------------
// Startup banner
// -----------------------------------------------------------------------

func printBanner(proxyAddr, apiAddr string, p *pool.Pool, rot *rotator.Rotator, authEnabled bool) {
	cur := rot.Current()
	curStr := "<none>"
	if cur != nil {
		curStr = cur.String()
	}

	authStr := "disabled"
	if authEnabled {
		authStr = "enabled"
	}

	fmt.Printf(`
╔══════════════════════════════════════════════════════════════╗
║                     proxyrotator %s
╠══════════════════════════════════════════════════════════════╣
║  Proxy server : %s
║  API server   : http://%s
║  Auth         : %s
║  Pool         : %d proxies (%d alive)
║  Active proxy : %s
╠══════════════════════════════════════════════════════════════╣
║  API endpoints:
║    GET  http://%s/api/current
║    GET  http://%s/api/pool
║    POST http://%s/api/rotate
║    POST http://%s/api/status
╚══════════════════════════════════════════════════════════════╝

`, padRight(version, 44),
		padRight(proxyAddr, 46),
		padRight(apiAddr, 44),
		padRight(authStr, 46),
		p.Len(), p.AliveLen(),
		padRight(curStr, 46),
		apiAddr, apiAddr, apiAddr, apiAddr,
	)
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
