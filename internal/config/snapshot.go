package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// Snapshot is an immutable, atomically-swappable config handle.
// Production code reads via .Load() (atomic load) and swaps via .Reload()
// so in-flight requests always complete on the previous snapshot.
//
// Design: problem — reloads must not drop in-flight requests;
// options — (a) global mutex (blocks requests), (b) atomic.Pointer swap
// (in-flight finish on old snapshot); choice: (b) — zero-copy swap.
// Failure: reload failure keeps the old config, logs level:error,
// never crashes the process.
type Snapshot struct {
	cfg atomic.Pointer[Config]
}

// NewSnapshot creates a Snapshot initialized from the loaded config.
func NewSnapshot(cfg Config) *Snapshot {
	s := &Snapshot{}
	s.cfg.Store(&cfg)
	return s
}

// Load returns the current config snapshot (never nil).
func (s *Snapshot) Load() Config {
	return *s.cfg.Load()
}

// Reload re-reads the config file, validates, and atomically swaps.
// On failure the old config is preserved and the error is returned.
func (s *Snapshot) Reload(path string) error {
	newCfg, err := Load(path)
	if err != nil {
		return fmt.Errorf("reload load: %w", err)
	}
	ApplyEnvOverrides(&newCfg)
	if err := Validate(newCfg); err != nil {
		return fmt.Errorf("reload validate: %w", err)
	}
	Defaults(&newCfg)
	s.cfg.Store(&newCfg)
	return nil
}

// Watch watches the config file for changes via SIGHUP and optional
// polling (watchSecs > 0). It blocks until ctx is cancelled.
// Each SIGHUP or poll interval triggers a Reload.
func (s *Snapshot) Watch(ctx context.Context, path string, watchSecs int) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	var poll *time.Ticker
	var pollCh <-chan time.Time
	if watchSecs > 0 {
		poll = time.NewTicker(time.Duration(watchSecs) * time.Second)
		defer poll.Stop()
		pollCh = poll.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-sighup:
			if err := s.Reload(path); err != nil {
				log.Printf("config reload error: %v", err)
			} else {
				log.Printf("config reloaded: %s", path)
			}
		case <-pollCh:
			if watchSecs > 0 {
				if err := s.Reload(path); err != nil {
					log.Printf("config reload error: %v", err)
				} else {
					log.Printf("config reloaded: %s", path)
				}
			}
		}
	}
}

// FileHash returns the hex SHA-256 of a config file's contents, or "" when the
// file cannot be read. Used only for the cluster config-drift check, so a read
// failure is deliberately not fatal.
func FileHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// StartWatcherHook watches for SIGHUP signals and optionally polls the config
// file at the given interval. When onReload is non-nil it is called with the
// freshly-loaded config *after* the snapshot has been swapped, so the caller
// can rebuild live routing state (balancer, breaker, backend pool).
//
// Note: SIGHUP is a Unix concept. Go on Windows never delivers it, so
// --watch_config is the only working trigger there.
func StartWatcherHook(
	snapshot *Snapshot,
	path string,
	watchSecs int,
	stopCh <-chan struct{},
	onReload func(Config),
) {
	go func() {
		var poll *time.Ticker
		var pollCh <-chan time.Time
		if watchSecs > 0 {
			poll = time.NewTicker(time.Duration(watchSecs) * time.Second)
			defer poll.Stop()
			pollCh = poll.C
		}

		sighup := make(chan os.Signal, 1)
		signal.Notify(sighup, syscall.SIGHUP)
		defer signal.Stop(sighup)

		reload := func() {
			if err := snapshot.Reload(path); err != nil {
				log.Printf("config reload error: %v", err)
				return
			}
			log.Printf("config reloaded: %s", path)
			if onReload != nil {
				onReload(snapshot.Load())
			}
		}

		for {
			select {
			case <-stopCh:
				return
			case <-sighup:
				reload()
			case <-pollCh:
				reload()
			}
		}
	}()
}

// StartWatcher starts a goroutine that watches for SIGHUP signals and
// optionally polls the config file. It swaps the snapshot but does not notify
// a caller — see StartWatcherHook when live state must be rebuilt too.
func StartWatcher(snapshot *Snapshot, path string, watchSecs int, stopCh <-chan struct{}) {
	StartWatcherHook(snapshot, path, watchSecs, stopCh, nil)
}

// BackendsFromConfig extracts host strings from the config.
func BackendsFromConfig(cfg Config) []string {
	hosts := make([]string, len(cfg.Backends))
	for i, b := range cfg.Backends {
		hosts[i] = b.Host
	}
	return hosts
}

// BackendWeight holds a backend URL and its weight.
type BackendWeight struct {
	URL    string
	Weight int
}

// BackendEntriesFromConfig extracts backend URLs and weights from the config.
func BackendEntriesFromConfig(cfg Config) []BackendWeight {
	entries := make([]BackendWeight, len(cfg.Backends))
	for i, b := range cfg.Backends {
		entries[i] = BackendWeight{URL: b.Host, Weight: b.Weight}
	}
	return entries
}
