<p align="center">
  <a href="https://www.anonymous-proxies.net?utm_source=github&utm_medium=banner&utm_campaign=proxyrotator" target="_blank" rel="noopener noreferrer">
    <img src="banner.svg" alt="proxyrotator banner" width="768" />
  </a>
</p>

# proxyrotator

> Smart rotating HTTP proxy — load-balance and auto-rotate a pool of upstream HTTP/SOCKS5 proxies with zero-drop connection draining, latency prioritisation, domain pinning, and a built-in management API.

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go)](https://go.dev)

---

## What It Does

**proxyrotator** sits between your application (crawler, scraper, automation tool) and your pool of upstream proxies.

```
Your App ──CONNECT──▶ proxyrotator :8080 ──▶ upstream proxy A (HTTP/SOCKS5)
                                         ──▶ upstream proxy B
                                         ──▶ upstream proxy C
```

Instead of hard-coding a single proxy and hoping it keeps working, proxyrotator:

- **Rotates** the active upstream on a schedule, after N requests, after too many errors, or on-demand.
- **Drains gracefully** — existing connections finish on the proxy they started on; new connections go to the new proxy immediately.
- **Pins domains** — once a session maps `example.com` to proxy B, it stays there for the lifetime of the session (prevents breaking multi-step flows).
- **Prioritises speed** — periodically measures latency and moves the fastest proxies to the front of the queue.
- **Monitors health** — optional background health checker removes dead proxies and re-adds them once they recover.
- **Exposes an API** — your crawler can report bad HTTP status codes (403, 429 …) and the rotator decides whether to rotate based on your thresholds.

---

## Installation

### Download a pre-built binary (recommended)

Download the latest release for your platform from the
[Releases page](https://github.com/romeomihailus/proxyrotator/releases/latest).

| Platform | File |
|---|---|
| Linux x86-64 | `proxyrotator_linux_amd64.tar.gz` |
| Linux ARM64 | `proxyrotator_linux_arm64.tar.gz` |
| macOS Intel | `proxyrotator_darwin_amd64.tar.gz` |
| macOS Apple Silicon | `proxyrotator_darwin_arm64.tar.gz` |
| Windows x86-64 | `proxyrotator_windows_amd64.zip` |

**Linux / macOS one-liner:**

```bash
curl -sSL https://github.com/romeomihailus/proxyrotator/releases/latest/download/proxyrotator_$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz \
  | tar -xz proxyrotator && sudo mv proxyrotator /usr/local/bin/
```

### Build from source

Requires Go 1.21+:

```bash
git clone https://github.com/romeomihailus/proxyrotator
cd proxyrotator
go build -o proxyrotator .
```

---

## Quick Start

**1. Create a proxy list file** (`proxies.txt`):

```
http://1.2.3.4:8080
https://user:pass@5.6.7.8:3128
socks5://user:pass@9.10.11.12:1080
```

**2. Start proxyrotator:**

```bash
proxyrotator --file proxies.txt
```

**3. Point your application at `http://localhost:8080`.**

That's it. The tool will print the proxy address and API URL on startup.

---

## Usage

```
proxyrotator [flags]
```

### All flags

| Flag | Default | Description |
|------|---------|-------------|
| `--file`, `-f` | _(required)_ | Path to the proxy list file |
| `--listen`, `-l` | `0.0.0.0:8080` | Local proxy listen address |
| `--api-port` | `9090` | Port for the management API (bound to `127.0.0.1`) |
| `--auth` | _(none)_ | Proxy auth credentials (`user:pass`). Omit to disable. |
| `--monitor` | `false` | Enable background health checks (marks/restores dead proxies) |
| `--monitor-interval` | `30s` | Interval between health check passes |
| `--monitor-url` | `http://connectivitycheck.gstatic.com/generate_204` | URL used for health probing |
| `--rotate-interval` | _(disabled)_ | Rotate on a fixed schedule (e.g. `5m`, `1h`) |
| `--rotate-requests` | `0` | Rotate after this many requests (`0` = off) |
| `--rotate-conn-errors` | `5` | Rotate after this many ECONNRESET / handshake errors (`0` = off) |
| `--rotate-http-errors` | `3` | Rotate after this many bad HTTP status reports via API (`0` = off) |
| `--dedup-window` | `2s` | Deduplication window for API error reports (see below) |
| `--no-latency-sort` | `false` | Disable latency-based proxy prioritisation |
| `--latency-interval` | `5m` | How often to re-measure proxy latencies |
| `--dial-timeout` | `30s` | Timeout when dialling through an upstream proxy |

### Common examples

```bash
# Rotate every 5 minutes, with monitoring enabled
proxyrotator -f proxies.txt --rotate-interval 5m --monitor

# Rotate after 300 requests served
proxyrotator -f proxies.txt --rotate-requests 300

# Rotate after 5 connection errors or 3 HTTP error reports
proxyrotator -f proxies.txt --rotate-conn-errors 5 --rotate-http-errors 3

# Require Proxy-Authorization from the client
proxyrotator -f proxies.txt --auth myuser:mypassword

# Disable latency sorting (preserve original file order)
proxyrotator -f proxies.txt --no-latency-sort

# Combine: rotate every 10 minutes OR after 500 requests, with auth and monitoring
proxyrotator -f proxies.txt \
  --rotate-interval 10m \
  --rotate-requests 500 \
  --auth scraper:secret \
  --monitor \
  --listen 0.0.0.0:8080 \
  --api-port 9090
```

---

## Proxy List Format

One proxy URI per line. Blank lines and lines starting with `#` are ignored.

```
# HTTP proxy (no auth)
http://1.2.3.4:8080

# HTTPS proxy with credentials
https://user:pass@5.6.7.8:3128

# SOCKS5 proxy with credentials
socks5://user:pass@9.10.11.12:1080

# Bare host:port — treated as HTTP
10.0.0.1:3128
```

Supported schemes: `http`, `https`, `socks5`.

---

## How Rotation Works

### Rotation triggers

Multiple triggers can be active simultaneously. Whichever fires first wins.

| Trigger | Flag | Notes |
|---------|------|-------|
| Time interval | `--rotate-interval` | Ticks on a wall-clock schedule |
| Request count | `--rotate-requests` | Counts requests served by the **current** proxy |
| Connection errors | `--rotate-conn-errors` | ECONNRESET, TLS handshake failure, upstream dial failure |
| HTTP errors (API) | `--rotate-http-errors` | Non-2xx/3xx codes reported by your crawler via `POST /api/status` |
| Manual | `POST /api/rotate` | Forced, immediate |

### Selection algorithm

Rotation picks the **next proxy** in round-robin order from the alive pool.
If `--no-latency-sort` is **not** set (default), the pool is sorted by latency (ascending) before the round-robin index advances, so the fastest proxies get the most traffic.

### Graceful drain (no dropped connections)

On rotation, **only new connections** are sent to the new proxy.  
Connections that are already tunnelling continue on the proxy they grabbed at connection time.  
The old proxy's `active_conns` counter in `/api/pool` will count down to zero naturally.

There is no hard timeout — connections finish in their own time.

---

## Domain Pinning

When your crawler targets a multi-step workflow on the same domain
(login → scrape → paginate), proxyrotator ensures that all requests to that
domain flow through the **same upstream proxy** for the duration of the
session.

- The first request to `example.com` pins it to the current active proxy.
- Subsequent requests to `example.com` always use the pinned proxy, even if
  the global active proxy rotates.
- If the pinned proxy dies (and monitoring is enabled), the pin is cleared
  and the next request picks (and pins) the new active proxy.
- All pins are **session-scoped** — they reset when proxyrotator restarts.

---

## Management API

The API server binds only to `127.0.0.1` (loopback) and runs on the port
specified by `--api-port` (default `9090`).

### `GET /api/current`

Returns the currently active upstream proxy.

```bash
curl http://127.0.0.1:9090/api/current
```

```json
{
  "id": 2,
  "address": "http://5.6.7.8:3128",
  "scheme": "http",
  "alive": true,
  "latency_ms": "87",
  "active_conns": 4,
  "req_count": 142,
  "conn_errors": 0,
  "http_errors": 1
}
```

---

### `GET /api/pool`

Returns the full proxy pool with per-proxy stats. The currently active proxy
has `[ACTIVE]` prepended to its address.

```bash
curl http://127.0.0.1:9090/api/pool
```

```json
[
  {
    "id": 1,
    "address": "[ACTIVE] http://1.2.3.4:8080",
    "scheme": "http",
    "alive": true,
    "latency_ms": "63",
    "active_conns": 12,
    "req_count": 300,
    "conn_errors": 2,
    "http_errors": 0
  },
  {
    "id": 2,
    "address": "socks5://9.10.11.12:1080",
    "scheme": "socks5",
    "alive": false,
    "latency_ms": "0",
    "active_conns": 0,
    "req_count": 0,
    "conn_errors": 0,
    "http_errors": 0
  }
]
```

---

### `POST /api/rotate`

Forces an immediate proxy rotation.

```bash
curl -s -X POST http://127.0.0.1:9090/api/rotate
```

```json
{"ok": true, "proxy": "http://5.6.7.8:3128"}
```

---

### `POST /api/status`

Reports a HTTP status code received by your crawler for a given destination.
The rotator counts errors per destination and rotates when the threshold
(`--rotate-http-errors`) is reached.

```bash
curl -s -X POST http://127.0.0.1:9090/api/status \
  -H "Content-Type: application/json" \
  -d '{"status": 403, "destination": "example.com"}'
```

```json
{"ok": true, "rotated": false}
```

**Deduplication:** If your crawler has many requests in flight to the same
destination when it gets banned, they will all report a 403. The rotator
deduplicates error reports for the same destination within a short window
(`--dedup-window`, default `2s`), so only one error is counted per
destination per window.  Additionally, if a rotation just happened
(within the same window), reports are discarded — they almost certainly
belong to the old proxy.

---

## Integration Examples

### Python (requests + proxies)

```python
import requests

session = requests.Session()
session.proxies = {
    "http":  "http://localhost:8080",
    "https": "http://localhost:8080",
}

# Optional auth
session.proxies = {
    "http":  "http://myuser:mypassword@localhost:8080",
    "https": "http://myuser:mypassword@localhost:8080",
}

resp = session.get("https://example.com")

# Report bad status to the rotator
if resp.status_code not in range(200, 400):
    requests.post("http://127.0.0.1:9090/api/status", json={
        "status": resp.status_code,
        "destination": "example.com"
    })
```

### curl

```bash
# Basic usage
curl -x http://localhost:8080 https://example.com

# With auth
curl -x http://myuser:mypassword@localhost:8080 https://example.com
```

### Playwright / Puppeteer

```js
const browser = await chromium.launch({
  proxy: { server: 'http://localhost:8080' }
});
```

---

## Architecture

```
proxyrotator/
├── main.go
├── cmd/
│   └── root.go          # Cobra CLI, flag parsing, wiring
└── internal/
    ├── pool/
    │   └── pool.go      # Proxy pool: parse, store, sort by latency
    ├── upstream/
    │   └── dialer.go    # Dial through HTTP or SOCKS5 upstream
    ├── rotator/
    │   └── rotator.go   # Rotation logic, domain pinning, error tracking
    ├── monitor/
    │   └── monitor.go   # Background health checks + latency probes
    ├── server/
    │   └── server.go    # HTTP CONNECT + plain HTTP proxy server
    └── api/
        └── api.go       # Management REST API
```

### How components interact

```
┌─────────────────────────────────────────────┐
│                  pool.Pool                  │
│  Stores all Proxy objects, sorts by latency │
└──────────────┬──────────────────────────────┘
               │ reads / marks alive
    ┌──────────┼──────────────┐
    │          │              │
┌───▼──────┐  │  ┌───────────▼─────────┐
│ monitor  │  │  │      rotator        │
│ (health) │  │  │ tracks current proxy │
└──────────┘  │  │ rotation triggers    │
              │  │ domain pin map       │
              │  └──────┬──────────────┘
              │         │ ProxyFor(destination)
              │  ┌──────▼──────────────┐
              │  │      server         │
              │  │ HTTP CONNECT + HTTP │
              │  │ drain on rotate     │
              │  └──────┬──────────────┘
              │         │ upstream.Dial(ctx, proxy.URL, dest)
              │  ┌──────▼──────────────┐
              │  │     upstream        │
              │  │ HTTP CONNECT tunnel │
              │  │ SOCKS5 dial         │
              │  └─────────────────────┘
              │
    ┌─────────▼───────────────┐
    │          api            │
    │  /rotate /status /pool  │
    └─────────────────────────┘
```

---

## Building Releases

proxyrotator uses [GoReleaser](https://goreleaser.com) for cross-platform builds.

```bash
# Install GoReleaser
brew install goreleaser  # macOS
# or: go install github.com/goreleaser/goreleaser/v2@latest

# Snapshot build (local, no tag required)
goreleaser release --snapshot --clean

# Production release (requires a git tag)
git tag v1.0.0
goreleaser release --clean
```

Binaries will be in `dist/`.

---

## License

MIT

---

<p align="center">
  <strong>proxyrotator</strong> is brought to you by <a href="https://www.anonymous-proxies.net/?utm_source=github&utm_medium=link&utm_campaign=proxyrotator"><strong>Anonymous-Proxies.net</strong></a> —
  a trusted proxy provider since 2008, delivering premium HTTP, SOCKS5, and residential proxies
  to thousands of clients worldwide.
  <br /><br />
  Need reliable proxies? <a href="https://www.anonymous-proxies.net/?utm_source=github&utm_medium=link&utm_campaign=proxyrotator">Browse our plans →</a>
</p>
