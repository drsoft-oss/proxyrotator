// Package upstream handles dialing through HTTP and SOCKS5 upstream proxies.
package upstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"golang.org/x/net/proxy"
)

// Dial opens a TCP connection to destination through the upstream proxy.
// destination must be in "host:port" format.
// The returned conn is a raw TCP pipe ready for bidirectional tunneling.
func Dial(ctx context.Context, upstream *url.URL, destination string) (net.Conn, error) {
	switch upstream.Scheme {
	case "http", "https":
		return dialHTTP(ctx, upstream, destination)
	case "socks5":
		return dialSOCKS5(ctx, upstream, destination)
	default:
		return nil, fmt.Errorf("unsupported upstream scheme: %s", upstream.Scheme)
	}
}

// dialHTTP sends an HTTP CONNECT request to the upstream proxy and returns
// the connection after the tunnel is established.
func dialHTTP(ctx context.Context, upstream *url.URL, destination string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Host)
	if err != nil {
		return nil, fmt.Errorf("dial upstream proxy %s: %w", upstream.Host, err)
	}

	// Build CONNECT request
	req, err := http.NewRequestWithContext(ctx, http.MethodConnect, "//"+destination, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("build CONNECT request: %w", err)
	}
	req.Host = destination

	// Inject proxy auth header if credentials are present
	if upstream.User != nil {
		user := upstream.User.Username()
		pass, _ := upstream.User.Password()
		creds := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req.Header.Set("Proxy-Authorization", "Basic "+creds)
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}

	// Read the proxy's response
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT failed: %s", resp.Status)
	}

	// If the bufio reader consumed bytes beyond the response, wrap conn to
	// replay them. In practice this doesn't happen on a clean CONNECT tunnel.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// dialSOCKS5 dials through a SOCKS5 upstream proxy.
func dialSOCKS5(ctx context.Context, upstream *url.URL, destination string) (net.Conn, error) {
	var auth *proxy.Auth
	if upstream.User != nil {
		user := upstream.User.Username()
		pass, _ := upstream.User.Password()
		auth = &proxy.Auth{User: user, Password: pass}
	}

	dialer, err := proxy.SOCKS5("tcp", upstream.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("create socks5 dialer: %w", err)
	}

	// Use the context-aware interface if available (golang.org/x/net/proxy
	// implements it since Go 1.15).
	type contextDialer interface {
		DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	}
	if cd, ok := dialer.(contextDialer); ok {
		conn, err := cd.DialContext(ctx, "tcp", destination)
		if err != nil {
			return nil, fmt.Errorf("socks5 dial %s: %w", destination, err)
		}
		return conn, nil
	}

	conn, err := dialer.Dial("tcp", destination)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial %s: %w", destination, err)
	}
	return conn, nil
}

// bufferedConn wraps a net.Conn and prepends already-buffered bytes to the
// read stream. Used when bufio.Reader consumed extra bytes from a CONNECT
// response.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}
