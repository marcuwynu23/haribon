package discover_test

// discover_watch_test.go — tests for the delivery side of discovery.
//
// The original suite only covered Discover(). That is why a provider could be
// configured, poll successfully, and still have no effect: Watch discarded
// every result. These tests pin the contract that a change reaches onChange and
// that an unchanged or failed poll does not.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcuwynu23/haribon/internal/discover"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------- SameList ----------

func TestSameList(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, true},
		{"empty vs one", nil, []string{"a"}, false},
		{"identical", []string{"a", "b"}, []string{"a", "b"}, true},
		{"reordered", []string{"a", "b"}, []string{"b", "a"}, false},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"different content", []string{"a"}, []string{"c"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := discover.SameList(c.a, c.b); got != c.want {
				t.Fatalf("SameList(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// ---------- Watch delivery ----------

// TestFileProvider_WatchDeliversChanges is the regression test for the bug that
// made discovery inert: Watch polled, then discarded the result with `_, _ =`.
func TestFileProvider_WatchDeliversChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backends.json")
	writeFile(t, path, `["http://a:1"]`)

	stop := make(chan struct{})
	defer close(stop)

	p := discover.NewFileProvider(path, 20*time.Millisecond, stop)

	changes := make(chan []string, 4)
	go p.Watch(stop, func(urls []string) { changes <- urls })

	// Give the loop a tick to settle on the initial list.
	select {
	case first := <-changes:
		if len(first) != 1 || first[0] != "http://a:1" {
			t.Fatalf("unexpected initial list: %v", first)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch never delivered the initial backend list")
	}

	writeFile(t, path, `["http://a:1","http://b:2"]`)

	select {
	case got := <-changes:
		if len(got) != 2 || got[1] != "http://b:2" {
			t.Fatalf("expected the added backend, got %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch never delivered the changed backend list")
	}
}

// TestFileProvider_WatchSkipsUnchangedPolls keeps a steady pool from causing a
// rebuild (and a fresh balancer) on every single poll.
func TestFileProvider_WatchSkipsUnchangedPolls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backends.json")
	writeFile(t, path, `["http://a:1"]`)

	stop := make(chan struct{})
	defer close(stop)

	p := discover.NewFileProvider(path, 10*time.Millisecond, stop)

	changes := make(chan []string, 16)
	go p.Watch(stop, func(urls []string) { changes <- urls })

	select {
	case <-changes:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch never delivered the initial list")
	}

	// Many poll intervals pass with the file untouched.
	time.Sleep(300 * time.Millisecond)

	select {
	case extra := <-changes:
		t.Fatalf("an unchanged file must not trigger onChange, got %v", extra)
	default:
	}
}

// TestFileProvider_WatchSurvivesBadPolls: a malformed or missing file mid-run
// must not kill the watcher, because a single bad write should not end
// discovery for the lifetime of the process.
func TestFileProvider_WatchSurvivesBadPolls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backends.json")
	writeFile(t, path, `["http://a:1"]`)

	stop := make(chan struct{})
	defer close(stop)

	p := discover.NewFileProvider(path, 10*time.Millisecond, stop)

	changes := make(chan []string, 16)
	go p.Watch(stop, func(urls []string) { changes <- urls })

	select {
	case <-changes:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch never delivered the initial list")
	}

	writeFile(t, path, `{ this is not a JSON array }`)
	time.Sleep(100 * time.Millisecond)
	writeFile(t, path, `["http://a:1","http://recovered:80"]`)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-changes:
			if len(got) == 2 && got[1] == "http://recovered:80" {
				return // recovered
			}
		case <-deadline:
			t.Fatal("the watcher stopped delivering after a malformed file")
		}
	}
}

func TestFileProvider_WatchStopsOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backends.json")
	writeFile(t, path, `["http://a:1"]`)

	stop := make(chan struct{})
	p := discover.NewFileProvider(path, 10*time.Millisecond, stop)

	done := make(chan struct{})
	go func() {
		p.Watch(stop, func([]string) {})
		close(done)
	}()

	close(stop)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch must return once stopCh is closed")
	}
}

// TestStaticProvider_WatchBlocksUntilClose documents that a static provider has
// nothing to poll but must still honour the stop channel.
func TestStaticProvider_WatchBlocksUntilClose(t *testing.T) {
	stop := make(chan struct{})
	p := discover.NewStaticProvider([]string{"http://a:1"})

	done := make(chan struct{})
	go func() {
		p.Watch(stop, func([]string) { t.Error("a static provider must never report a change") })
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("static Watch must block until stopCh closes")
	case <-time.After(100 * time.Millisecond):
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("static Watch must return once stopCh is closed")
	}
}

// ---------- DNS ----------

// TestDNSProvider_ReturnsUsableURLs covers the fix that made DNS discovery
// usable: LookupHost returns bare addresses like "10.0.0.1", which are not valid
// proxy targets. Each must be stamped with the configured port.
func TestDNSProvider_ReturnsUsableURLs(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)

	// "localhost" resolves locally without any network access.
	p := discover.NewDNSProvider("localhost", 8080, time.Second, stop)

	urls, err := p.Discover()
	if err != nil {
		t.Skipf("localhost did not resolve in this environment: %v", err)
	}
	if len(urls) == 0 {
		t.Fatal("localhost resolved to no addresses")
	}
	for _, u := range urls {
		if u[:7] != "http://" {
			t.Fatalf("resolved address must be an http URL, got %q", u)
		}
		if u[len(u)-5:] != ":8080" {
			t.Fatalf("resolved address must carry dns_port, got %q", u)
		}
	}
}

func TestDNSProvider_DefaultPortIsEighty(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)

	p := discover.NewDNSProvider("localhost", 0, time.Second, stop)
	urls, err := p.Discover()
	if err != nil {
		t.Skipf("localhost did not resolve in this environment: %v", err)
	}
	for _, u := range urls {
		if u[len(u)-3:] != ":80" {
			t.Fatalf("port should default to 80, got %q", u)
		}
	}
}
