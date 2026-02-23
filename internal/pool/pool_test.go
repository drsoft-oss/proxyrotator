package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeProxyFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "proxies*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoadFile_ValidProxies(t *testing.T) {
	content := `
# comment line
http://1.2.3.4:8080
https://user:pass@5.6.7.8:3128
socks5://9.10.11.12:1080

# another comment
10.0.0.1:3128
`
	f := writeProxyFile(t, content)
	p := New(false)
	if err := p.LoadFile(f); err != nil {
		t.Fatalf("LoadFile error: %v", err)
	}
	if got := p.Len(); got != 4 {
		t.Errorf("expected 4 proxies, got %d", got)
	}
}

func TestLoadFile_EmptyFile(t *testing.T) {
	f := writeProxyFile(t, "# only comments\n\n")
	p := New(false)
	err := p.LoadFile(f)
	if err == nil {
		t.Fatal("expected error for empty proxy file, got nil")
	}
}

func TestLoadFile_MissingFile(t *testing.T) {
	p := New(false)
	err := p.LoadFile(filepath.Join(t.TempDir(), "nonexistent.txt"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadFile_InvalidScheme(t *testing.T) {
	// Invalid line should be skipped; valid ones should still load
	content := "trojan://bad:scheme@1.2.3.4:443\nhttp://1.2.3.4:8080\n"
	f := writeProxyFile(t, content)
	p := New(false)
	if err := p.LoadFile(f); err != nil {
		t.Fatalf("LoadFile error: %v", err)
	}
	if p.Len() != 1 {
		t.Errorf("expected 1 valid proxy, got %d", p.Len())
	}
}

func TestAlive_FiltersDead(t *testing.T) {
	content := "http://1.2.3.4:8080\nhttp://5.6.7.8:8080\nhttp://9.10.11.12:8080\n"
	f := writeProxyFile(t, content)
	p := New(false)
	if err := p.LoadFile(f); err != nil {
		t.Fatal(err)
	}

	// Mark the second proxy dead
	all := p.All()
	all[1].SetAlive(false)

	alive := p.Alive()
	if len(alive) != 2 {
		t.Errorf("expected 2 alive proxies, got %d", len(alive))
	}
}

func TestAlive_LatencySort(t *testing.T) {
	content := "http://1.2.3.4:8080\nhttp://5.6.7.8:8080\nhttp://9.10.11.12:8080\n"
	f := writeProxyFile(t, content)
	p := New(true) // latency sort enabled
	if err := p.LoadFile(f); err != nil {
		t.Fatal(err)
	}

	all := p.All()
	all[0].SetLatency(300 * time.Millisecond)
	all[1].SetLatency(50 * time.Millisecond)
	all[2].SetLatency(150 * time.Millisecond)

	alive := p.Alive()
	// Should be sorted: 50ms, 150ms, 300ms
	if alive[0].Latency() != 50*time.Millisecond {
		t.Errorf("expected fastest proxy first, got %s", alive[0].Latency())
	}
	if alive[1].Latency() != 150*time.Millisecond {
		t.Errorf("expected 150ms second, got %s", alive[1].Latency())
	}
}

func TestAlive_ZeroLatencyLast(t *testing.T) {
	content := "http://1.2.3.4:8080\nhttp://5.6.7.8:8080\nhttp://9.10.11.12:8080\n"
	f := writeProxyFile(t, content)
	p := New(true)
	if err := p.LoadFile(f); err != nil {
		t.Fatal(err)
	}

	all := p.All()
	all[0].SetLatency(0)              // unprobed
	all[1].SetLatency(200 * time.Millisecond)
	all[2].SetLatency(100 * time.Millisecond)

	alive := p.Alive()
	// Unprobed (zero) should be last
	last := alive[len(alive)-1]
	if last.Latency() != 0 {
		t.Errorf("expected zero-latency proxy last, got %s", last.Latency())
	}
}

func TestProxyString_RedactsPassword(t *testing.T) {
	content := "http://user:secret@1.2.3.4:8080\n"
	f := writeProxyFile(t, content)
	p := New(false)
	if err := p.LoadFile(f); err != nil {
		t.Fatal(err)
	}
	s := p.All()[0].String()
	if contains(s, "secret") {
		t.Errorf("proxy String() leaked password: %s", s)
	}
}

func TestProxyCounters(t *testing.T) {
	content := "http://1.2.3.4:8080\n"
	f := writeProxyFile(t, content)
	p := New(false)
	if err := p.LoadFile(f); err != nil {
		t.Fatal(err)
	}
	px := p.All()[0]
	px.ReqCount.Add(5)
	px.ConnErrors.Add(2)
	px.HTTPErrors.Add(1)

	px.ResetErrorCounters()
	if px.ReqCount.Load() != 0 {
		t.Error("ReqCount not reset")
	}
	if px.ConnErrors.Load() != 0 {
		t.Error("ConnErrors not reset")
	}
	if px.HTTPErrors.Load() != 0 {
		t.Error("HTTPErrors not reset")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 &&
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
