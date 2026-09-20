package discover_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcuwynu23/haribon/internal/discover"
)

// ---------- StaticProvider ----------

func TestStaticProvider_Discover(t *testing.T) {
	p := discover.NewStaticProvider([]string{"http://a", "http://b"})
	defer p.Close()

	got, err := p.Discover()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "http://a" || got[1] != "http://b" {
		t.Fatalf("unexpected backends: %v", got)
	}
}

func TestStaticProvider_Update(t *testing.T) {
	p := discover.NewStaticProvider([]string{"http://a"})
	defer p.Close()

	p.Update([]string{"http://b", "http://c"})
	got, _ := p.Discover()
	if len(got) != 2 || got[0] != "http://b" {
		t.Fatalf("update failed: %v", got)
	}
}

// ---------- FileProvider ----------

func TestFileProvider_Discover_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backends.json")
	data, _ := json.Marshal([]string{"http://a:4441", "http://b:4442"})
	os.WriteFile(path, data, 0644)

	stopCh := make(chan struct{})
	defer close(stopCh)

	p := discover.NewFileProvider(path, 1*time.Second, stopCh)
	defer p.Close()

	got, err := p.Discover()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(got))
	}
}

func TestFileProvider_Discover_MissingFile(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	p := discover.NewFileProvider("/no/such/file.json", 1*time.Second, stopCh)
	defer p.Close()

	_, err := p.Discover()
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFileProvider_Discover_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("{invalid"), 0644)

	stopCh := make(chan struct{})
	defer close(stopCh)

	p := discover.NewFileProvider(path, 1*time.Second, stopCh)
	defer p.Close()

	_, err := p.Discover()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ---------- Config ----------

func TestNewProvider_Static(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	p, err := discover.NewProvider(discover.Config{Provider: "static"}, stopCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer p.Close()

	got, err := p.Discover()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty static backends, got %v", got)
	}
}

func TestNewProvider_DNS_MissingName(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	_, err := discover.NewProvider(discover.Config{Provider: "dns"}, stopCh)
	if err == nil {
		t.Fatal("expected error for dns without name")
	}
}

func TestNewProvider_File_MissingPath(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	_, err := discover.NewProvider(discover.Config{Provider: "file"}, stopCh)
	if err == nil {
		t.Fatal("expected error for file without path")
	}
}
