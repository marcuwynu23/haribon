package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Provider supplies backend addresses dynamically.
// Implementations may poll DNS, watch files, or use other discovery mechanisms.
type Provider interface {
	// Discover returns the current list of backend addresses.
	Discover() ([]string, error)
	// Close releases any resources held by the provider.
	Close() error
	// Watch polls for backend changes until stopCh is closed, calling onChange
	// with the new list whenever it differs from the last one delivered.
	// onChange is never called concurrently and never called with an unchanged
	// list; a poll that errors is skipped and retried on the next tick.
	Watch(stopCh <-chan struct{}, onChange func([]string))
}

// SameList reports whether two backend lists contain the same URLs in the same
// order. Used to suppress no-op rebuilds when a poll returns unchanged data.
func SameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// watchLoop is the shared polling body used by the DNS and file providers.
func watchLoop(interval time.Duration, stopCh <-chan struct{}, discover func() ([]string, error), onChange func([]string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var last []string
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			got, err := discover()
			if err != nil || len(got) == 0 {
				continue // transient failure: keep the previous pool
			}
			if SameList(got, last) {
				continue
			}
			last = append([]string(nil), got...)
			if onChange != nil {
				onChange(got)
			}
		}
	}
}

// StaticProvider returns a fixed list of backends (no dynamic discovery).
// It satisfies the Provider interface for backward compatibility.
type StaticProvider struct {
	backends []string
	mu       sync.RWMutex
}

// NewStaticProvider creates a provider that always returns the given backends.
func NewStaticProvider(backends []string) *StaticProvider {
	cp := make([]string, len(backends))
	copy(cp, backends)
	return &StaticProvider{backends: cp}
}

// Discover returns the static backend list.
func (p *StaticProvider) Discover() ([]string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.backends))
	copy(out, p.backends)
	return out, nil
}

// Close is a no-op for StaticProvider.
func (p *StaticProvider) Close() error { return nil }

// Watch is a no-op for StaticProvider (no dynamic discovery).
func (p *StaticProvider) Watch(stopCh <-chan struct{}, _ func([]string)) {
	<-stopCh
}

// Update replaces the static backend list (thread-safe).
func (p *StaticProvider) Update(backends []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = make([]string, len(backends))
	copy(p.backends, backends)
}

// DNSProvider polls a DNS name for A/AAAA records at a configured interval.
// It implements Provider.
//
// Note: SRV lookups are not implemented; A/AAAA records are used and the port
// is taken from dns_port (default 80) so the result is a usable backend URL.
type DNSProvider struct {
	name     string
	port     int
	interval time.Duration
	resolver *net.Resolver
	stopCh   <-chan struct{}
	backends []string
	mu       sync.RWMutex
}

// NewDNSProvider creates a DNS discovery provider.
// name is the DNS hostname (e.g. "api.internal").
// port is stamped onto every resolved address; values <= 0 mean 80.
// interval is the poll duration.
// stopCh is used to stop the polling goroutine.
func NewDNSProvider(name string, port int, interval time.Duration, stopCh <-chan struct{}) *DNSProvider {
	if port <= 0 {
		port = 80
	}
	return &DNSProvider{
		name:     name,
		port:     port,
		interval: interval,
		resolver: &net.Resolver{PreferGo: true},
		stopCh:   stopCh,
	}
}

// Discover resolves the DNS name and returns backend URLs.
// net.Resolver.LookupHost returns bare addresses ("10.0.0.1"), so each one is
// formatted into an http:// URL using the configured port — a bare address is
// not usable as a proxy target.
//
// net.JoinHostPort is used rather than fmt.Sprintf("%s:%d") because an AAAA
// record ("2001:db8::1") must be bracketed: "http://2001:db8::1:80" is not a
// parseable URL, and would fail at request time rather than at startup.
func (p *DNSProvider) Discover() ([]string, error) {
	addrs, err := p.resolver.LookupHost(context.Background(), p.name)
	if err != nil {
		return nil, fmt.Errorf("dns lookup %q: %w", p.name, err)
	}
	urls := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		urls = append(urls, "http://"+net.JoinHostPort(addr, strconv.Itoa(p.port)))
	}
	sort.Strings(urls)
	p.mu.Lock()
	p.backends = urls
	p.mu.Unlock()
	return urls, nil
}

// Close is a no-op for DNSProvider (polling is stopped externally via stopCh).
func (p *DNSProvider) Close() error { return nil }

// Watch polls the DNS name at the configured interval until stopCh closes.
func (p *DNSProvider) Watch(stopCh <-chan struct{}, onChange func([]string)) {
	watchLoop(p.interval, stopCh, p.Discover, onChange)
}

// FileProvider watches a JSON file for backend list changes.
// The file must contain a JSON array of strings (backend URLs).
type FileProvider struct {
	path     string
	interval time.Duration
	stopCh   <-chan struct{}
	backends []string
	mu       sync.RWMutex
}

// NewFileProvider creates a file-based discovery provider.
// path is the JSON file path containing an array of backend URLs.
// interval is the poll duration in seconds.
func NewFileProvider(path string, interval time.Duration, stopCh <-chan struct{}) *FileProvider {
	return &FileProvider{
		path:     path,
		interval: interval,
		stopCh:   stopCh,
	}
}

// Discover reads the JSON file and returns the backend list.
func (p *FileProvider) Discover() ([]string, error) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return nil, fmt.Errorf("read discovery file %q: %w", p.path, err)
	}
	var urls []string
	if err := json.Unmarshal(data, &urls); err != nil {
		return nil, fmt.Errorf("parse discovery file %q: %w", p.path, err)
	}
	for _, u := range urls {
		if u == "" {
			return nil, fmt.Errorf("discovery file %q: empty backend url", p.path)
		}
	}
	p.mu.Lock()
	p.backends = urls
	p.mu.Unlock()
	return urls, nil
}

// Close is a no-op for FileProvider.
func (p *FileProvider) Close() error { return nil }

// Watch polls the file at the configured interval until stopCh closes.
func (p *FileProvider) Watch(stopCh <-chan struct{}, onChange func([]string)) {
	watchLoop(p.interval, stopCh, p.Discover, onChange)
}

// Config holds discovery configuration.
type Config struct {
	Provider   string // static | dns | file
	DNSName    string
	DNSPort    int // port stamped onto resolved DNS addresses (default 80)
	FilePath   string
	RefreshSec int
}

// NewProvider creates a Provider from the discovery config.
// Returns a StaticProvider if no discovery is configured.
func NewProvider(disc Config, stopCh <-chan struct{}) (Provider, error) {
	switch disc.Provider {
	case "dns":
		if disc.DNSName == "" {
			return nil, fmt.Errorf("dns provider requires dns_name")
		}
		interval := time.Duration(disc.RefreshSec) * time.Second
		if interval <= 0 {
			interval = 30 * time.Second
		}
		p := NewDNSProvider(disc.DNSName, disc.DNSPort, interval, stopCh)
		return p, nil
	case "file":
		if disc.FilePath == "" {
			return nil, fmt.Errorf("file provider requires file_path")
		}
		interval := time.Duration(disc.RefreshSec) * time.Second
		if interval <= 0 {
			interval = 30 * time.Second
		}
		return NewFileProvider(disc.FilePath, interval, stopCh), nil
	default:
		return NewStaticProvider(nil), nil
	}
}
