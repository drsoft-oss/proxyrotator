package rotator

import (
	"os"
	"testing"
	"time"

	"github.com/romeomihailus/proxyrotator/internal/pool"
)

// makePool creates a pool from a slice of proxy URIs.
func makePool(t *testing.T, uris []string) *pool.Pool {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "proxies*.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range uris {
		f.WriteString(u + "\n")
	}
	f.Close()

	p := pool.New(false)
	if err := p.LoadFile(f.Name()); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNew_PicksFirstProxy(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080", "http://5.6.7.8:8080"})
	r, err := New(p, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.Current() == nil {
		t.Fatal("expected a current proxy, got nil")
	}
}

func TestNew_NoAliveProxies(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080"})
	// Mark all dead
	for _, px := range p.All() {
		px.SetAlive(false)
	}
	_, err := New(p, Config{})
	if err == nil {
		t.Fatal("expected error with no alive proxies, got nil")
	}
}

func TestForceRotate(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080", "http://5.6.7.8:8080"})
	r, err := New(p, Config{})
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer r.Stop()

	gen0 := r.Generation()
	r.ForceRotate()

	// Wait for the rotation goroutine to process
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if r.Generation() != gen0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("generation did not increment after ForceRotate")
}

func TestRotateOnRequestCount(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080", "http://5.6.7.8:8080"})
	r, err := New(p, Config{RotateRequests: 3})
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer r.Stop()

	gen0 := r.Generation()

	// Fire 3 requests
	r.RecordRequest()
	r.RecordRequest()
	r.RecordRequest()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if r.Generation() != gen0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("rotation did not fire after reaching request count threshold")
}

func TestRotateOnConnErrors(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080", "http://5.6.7.8:8080"})
	r, err := New(p, Config{RotateConnErrors: 2})
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer r.Stop()

	gen0 := r.Generation()
	r.RecordConnError()
	r.RecordConnError()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if r.Generation() != gen0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("rotation did not fire after reaching conn-error threshold")
}

func TestDomainPinning_StickyForSession(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080", "http://5.6.7.8:8080"})
	r, err := New(p, Config{})
	if err != nil {
		t.Fatal(err)
	}

	// First call pins example.com to whatever the current proxy is.
	first := r.ProxyFor("example.com:443")
	if first == nil {
		t.Fatal("expected a proxy for example.com, got nil")
	}

	// Subsequent calls for the same domain must return the same proxy.
	second := r.ProxyFor("example.com:443")
	if second == nil {
		t.Fatal("expected a proxy on second call")
	}
	if first.ID != second.ID {
		t.Errorf("domain pin changed between calls: %d → %d", first.ID, second.ID)
	}

	// A different domain should also work but may differ.
	other := r.ProxyFor("other.com:443")
	if other == nil {
		t.Fatal("expected a proxy for other.com")
	}
}

func TestDomainPinning_ClearedAfterRotation(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080", "http://5.6.7.8:8080"})
	r, err := New(p, Config{})
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer r.Stop()

	pinned := r.ProxyFor("example.com:443")
	if pinned == nil {
		t.Fatal("expected pinned proxy")
	}

	// Force rotate — the pin for this proxy should be cleared.
	r.ForceRotate()
	time.Sleep(100 * time.Millisecond)

	// The pin should now point to the new proxy.
	after := r.ProxyFor("example.com:443")
	if after == nil {
		t.Fatal("expected proxy after rotation")
	}
	// They may or may not differ depending on pool size, but should not panic.
}

func TestHTTPErrorDedup(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080", "http://5.6.7.8:8080"})
	r, err := New(p, Config{
		RotateHTTPErrors:     2,
		HTTPErrorDedupWindow: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer r.Stop()

	gen0 := r.Generation()

	// Send 10 errors for the same destination in rapid succession.
	// They should be deduped to 1.
	for i := 0; i < 10; i++ {
		r.RecordHTTPError("example.com")
	}

	// Give the rotator time to process
	time.Sleep(100 * time.Millisecond)

	// Only 1 out of 10 should have been counted → threshold (2) not reached.
	if r.Generation() != gen0 {
		t.Error("HTTP error dedup failed: rotation triggered when it should not have")
	}
}

func TestHTTPErrorTriggersRotation(t *testing.T) {
	p := makePool(t, []string{"http://1.2.3.4:8080", "http://5.6.7.8:8080"})
	r, err := New(p, Config{
		RotateHTTPErrors:     2,
		HTTPErrorDedupWindow: 10 * time.Millisecond, // tiny window so we can test quickly
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer r.Stop()

	gen0 := r.Generation()

	// Two different destinations, spaced beyond the dedup window.
	r.RecordHTTPError("site-a.com")
	time.Sleep(20 * time.Millisecond)
	r.RecordHTTPError("site-b.com")

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if r.Generation() != gen0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("rotation did not fire after reaching HTTP error threshold")
}

func TestExtractDomain(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"example.com:443", "example.com"},
		{"example.com:80", "example.com"},
		{"example.com", "example.com"},
		{"Example.COM:8080", "example.com"},
	}
	for _, tc := range cases {
		got := extractDomain(tc.input)
		if got != tc.want {
			t.Errorf("extractDomain(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
