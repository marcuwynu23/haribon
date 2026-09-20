package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
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
	// Watch starts polling for backend changes until stopCh is closed.
	Watch(stopCh <-chan struct{})
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
func (p *StaticProvider) Watch(stopCh <-chan struct{}) {
	<-stopCh
}

// Update replaces the static backend list (thread-safe).
func (p *StaticProvider) Update(backends []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = make([]string, len(backends))
	copy(p.backends, backends)
}

// DNSProvider polls a DNS name for A/SRV records at a configured interval.
// It implements Provider.
type DNSProvider struct {
	name     string
	interval time.Duration
	resolver *net.Resolver
	stopCh   <-chan struct{}
	backends []string
	mu       sync.RWMutex
}

// NewDNSProvider creates a DNS discovery provider.
// name is the DNS hostname (e.g. "api.internal").
// interval is the poll duration.
// stopCh is used to stop the polling goroutine.
func NewDNSProvider(name string, interval time.Duration, stopCh <-chan struct{}) *DNSProvider {
	return &DNSProvider{
		name:     name,
		interval: interval,
		resolver: &net.Resolver{PreferGo: true},
		stopCh:   stopCh,
	}
}

// Discover resolves the DNS name and returns A record addresses.
func (p *DNSProvider) Discover() ([]string, error) {
	addrs, err := p.resolver.LookupHost(context.Background(), p.name)
	if err != nil {
		return nil, fmt.Errorf("dns lookup %q: %w", p.name, err)
	}
	p.mu.Lock()
	p.backends = addrs
	p.mu.Unlock()
	return addrs, nil
}

// Close is a no-op for DNSProvider (polling is stopped externally via stopCh).
func (p *DNSProvider) Close() error { return nil }

// Watch polls the DNS name at the configured interval until stopCh closes.
func (p *DNSProvider) Watch(stopCh <-chan struct{}) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			_, _ = p.Discover()
		}
	}
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
func (p *FileProvider) Watch(stopCh <-chan struct{}) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			_, _ = p.Discover()
		}
	}
}

// Config holds discovery configuration.
type Config struct {
	Provider   string // static | dns | file
	DNSName    string
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
		p := NewDNSProvider(disc.DNSName, interval, stopCh)
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
