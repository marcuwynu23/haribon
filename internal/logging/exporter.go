// Package logging delivers structured request log entries to one or more
// destinations.
//
// Problem:  Operators need request logs on stdout, in a file, and shipped to a
//
//	log store — without the proxy blocking on a slow or dead sink.
//
// Choice:   every remote exporter owns a bounded queue drained by a background
//
//	goroutine that batches. Write never blocks the caller: if the queue
//	is full the entry is dropped and counted, because stalling a proxy
//	request to deliver a log line is the wrong trade.
//
// Failure:  a failed flush is retried implicitly (the next batch carries on);
//
//	the drop counter is the signal that a sink is not keeping up.
package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogEntry is the structured log line written for every proxied request.
// Fields are additive-only — never remove or rename (Loki/Promtail contracts).
type LogEntry struct {
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Backend    string `json:"backend"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Retries    int    `json:"retries,omitempty"`
	Level      string `json:"level"`
}

// Record converts the entry to a generic map, used by exporters that need
// arbitrary key/value pairs (Fluent Bit) rather than a fixed struct.
func (e LogEntry) Record() map[string]interface{} {
	m := map[string]interface{}{
		"time":        e.Time,
		"method":      e.Method,
		"path":        e.Path,
		"backend":     e.Backend,
		"status":      e.Status,
		"duration_ms": e.DurationMS,
		"level":       e.Level,
	}
	if e.Retries > 0 {
		m["retries"] = e.Retries
	}
	return m
}

// FormatText renders the entry as a single human-readable line
// (log_format: text).
func FormatText(e LogEntry) string {
	ts := e.Time
	if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
		ts = t.UTC().Format("2006-01-02T15:04:05Z")
	}
	backend := e.Backend
	if backend == "" {
		backend = "-"
	}
	line := fmt.Sprintf("%s %-5s %s %s -> %s %d %dms",
		ts, strings.ToUpper(e.Level), e.Method, e.Path, backend, e.Status, e.DurationMS)
	if e.Retries > 0 {
		line += fmt.Sprintf(" retries=%d", e.Retries)
	}
	return line
}

// Exporter sends log entries to a single destination.
type Exporter interface {
	// Write hands a log entry to the exporter. It must not block.
	Write(entry LogEntry)
	// Close flushes anything buffered and releases resources. Idempotent.
	Close()
	// Name returns the exporter name for logging/metrics.
	Name() string
	// Dropped returns how many entries were discarded because the exporter
	// could not keep up.
	Dropped() int64
}

// ────────────────────────────────────────────────
// stdout / file
// ────────────────────────────────────────────────

// StdoutExporter writes log entries to stdout.
// format is "json" (default) or "text".
type StdoutExporter struct {
	Format string
	Out    *os.File // nil → os.Stdout
}

func (e *StdoutExporter) Write(entry LogEntry) {
	out := e.Out
	if out == nil {
		out = os.Stdout
	}
	if e.Format == "text" {
		_, _ = fmt.Fprintln(out, FormatText(entry))
		return
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = out.Write(append(b, '\n'))
}

func (e *StdoutExporter) Close()         {}
func (e *StdoutExporter) Name() string   { return "stdout" }
func (e *StdoutExporter) Dropped() int64 { return 0 }

// FileExporter writes log entries to a file.
type FileExporter struct {
	File   *os.File
	Format string
	mu     sync.Mutex
}

func (e *FileExporter) Write(entry LogEntry) {
	if e.File == nil {
		return
	}
	var line []byte
	if e.Format == "text" {
		line = []byte(FormatText(entry))
	} else {
		b, err := json.Marshal(entry)
		if err != nil {
			return
		}
		line = b
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.File.Write(append(line, '\n'))
}

func (e *FileExporter) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.File != nil {
		_ = e.File.Sync()
		_ = e.File.Close()
		e.File = nil
	}
}

func (e *FileExporter) Name() string   { return "file" }
func (e *FileExporter) Dropped() int64 { return 0 }

// ────────────────────────────────────────────────
// Batched remote exporters
// ────────────────────────────────────────────────

const (
	defaultQueueDepth = 4096
	defaultMaxBatch   = 256
	defaultFlushEvery = 2 * time.Second
)

// batch carries the behaviour shared by every remote exporter: a bounded queue,
// a background drain loop that flushes on size or time, and a drop counter.
type batch struct {
	name      string
	queue     chan LogEntry
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	dropped   atomic.Int64

	maxBatch   int
	flushEvery time.Duration
	send       func([]LogEntry)
}

func newBatch(name string, send func([]LogEntry)) *batch {
	b := &batch{
		name:       name,
		queue:      make(chan LogEntry, defaultQueueDepth),
		done:       make(chan struct{}),
		maxBatch:   defaultMaxBatch,
		flushEvery: defaultFlushEvery,
		send:       send,
	}
	b.wg.Add(1)
	go b.run()
	return b
}

func (b *batch) Write(entry LogEntry) {
	select {
	case b.queue <- entry:
	default:
		// Sink is behind. Dropping is deliberate: a proxy must never block a
		// user request to deliver a log line.
		b.dropped.Add(1)
	}
}

func (b *batch) Close() {
	b.closeOnce.Do(func() {
		close(b.done)
		b.wg.Wait()
	})
}

func (b *batch) Name() string   { return b.name }
func (b *batch) Dropped() int64 { return b.dropped.Load() }

func (b *batch) run() {
	defer b.wg.Done()

	ticker := time.NewTicker(b.flushEvery)
	defer ticker.Stop()

	buf := make([]LogEntry, 0, b.maxBatch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		b.send(buf)
		buf = buf[:0]
	}

	for {
		select {
		case e := <-b.queue:
			buf = append(buf, e)
			if len(buf) >= b.maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-b.done:
			// Drain whatever is queued, then flush and return.
			for {
				select {
				case e := <-b.queue:
					buf = append(buf, e)
					if len(buf) >= b.maxBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// ────────────────────────────────────────────────
// Loki
// ────────────────────────────────────────────────

// LokiExporter pushes log entries to Loki's HTTP push API.
type LokiExporter struct {
	*batch
	url    string
	labels map[string]string
	client *http.Client
}

// NewLokiExporter creates a Loki push exporter.
// url is the Loki base URL, e.g. "http://localhost:3100".
// labels are extra stream labels; "job" is added when absent.
func NewLokiExporter(url string, labels map[string]string) *LokiExporter {
	if url == "" {
		url = "http://localhost:3100"
	}
	lbl := map[string]string{"job": "haribon"}
	for k, v := range labels {
		lbl[k] = v
	}
	e := &LokiExporter{
		url:    strings.TrimRight(url, "/"),
		labels: lbl,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	e.batch = newBatch("loki", e.push)
	return e
}

// push sends one batch as a single Loki stream.
func (e *LokiExporter) push(entries []LogEntry) {
	values := make([][2]string, 0, len(entries))
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Time)
		if err != nil {
			ts = time.Now()
		}
		values = append(values, [2]string{
			fmt.Sprintf("%d", ts.UnixNano()),
			string(line),
		})
	}
	if len(values) == 0 {
		return
	}

	body, err := json.Marshal(map[string]interface{}{
		"streams": []map[string]interface{}{
			{"stream": e.labels, "values": values},
		},
	})
	if err != nil {
		return
	}

	req, err := http.NewRequest(http.MethodPost, e.url+"/loki/api/v1/push", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
}

// ────────────────────────────────────────────────
// Elasticsearch
// ────────────────────────────────────────────────

// ElasticsearchExporter bulk-indexes log entries.
type ElasticsearchExporter struct {
	*batch
	url    string
	index  string
	client *http.Client
}

// NewElasticsearchExporter creates a bulk exporter.
// url is the Elasticsearch base URL, e.g. "http://localhost:9200".
func NewElasticsearchExporter(url, index string) *ElasticsearchExporter {
	if url == "" {
		url = "http://localhost:9200"
	}
	if index == "" {
		index = "haribon"
	}
	e := &ElasticsearchExporter{
		url:    strings.TrimRight(url, "/"),
		index:  index,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	e.batch = newBatch("elasticsearch", e.bulk)
	return e
}

// bulk posts an NDJSON _bulk body: an action line followed by the document.
func (e *ElasticsearchExporter) bulk(entries []LogEntry) {
	var buf bytes.Buffer
	action, _ := json.Marshal(map[string]interface{}{
		"index": map[string]string{"_index": e.index},
	})
	for _, entry := range entries {
		doc, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(doc)
		buf.WriteByte('\n')
	}
	if buf.Len() == 0 {
		return
	}

	req, err := http.NewRequest(http.MethodPost, e.url+"/_bulk", bytes.NewReader(buf.Bytes()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := e.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
}

// ────────────────────────────────────────────────
// Fluent Bit (forward protocol)
// ────────────────────────────────────────────────

// FluentbitExporter ships entries over Fluent Bit's forward protocol (TCP).
// Records are sent in Message mode — the msgpack array [tag, time, record].
// Chunk/ack framing is not used, so a dropped TCP connection loses the batch
// rather than being retried; that is acceptable for access logs.
type FluentbitExporter struct {
	*batch
	addr   string
	tag    string
	dialer *net.Dialer
}

// NewFluentbitExporter creates a forward-protocol exporter.
// addr is the Fluent Bit forward input, e.g. "localhost:24224".
func NewFluentbitExporter(addr string) *FluentbitExporter {
	if addr == "" {
		addr = "localhost:24224"
	}
	e := &FluentbitExporter{
		addr:   addr,
		tag:    "haribon",
		dialer: &net.Dialer{Timeout: 5 * time.Second},
	}
	e.batch = newBatch("fluentbit", e.forward)
	return e
}

func (e *FluentbitExporter) forward(entries []LogEntry) {
	conn, err := e.dialer.Dial("tcp", e.addr)
	if err != nil {
		return
	}
	defer conn.Close()

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

	var buf bytes.Buffer
	for _, entry := range entries {
		t, err := time.Parse(time.RFC3339Nano, entry.Time)
		if err != nil {
			t = time.Now()
		}
		packForwardMessage(&buf, e.tag, t.Unix(), entry.Record())
	}
	if buf.Len() == 0 {
		return
	}
	_, _ = conn.Write(buf.Bytes())
}

// ────────────────────────────────────────────────
// Fan-out
// ────────────────────────────────────────────────

// Multi fans a single entry out to several exporters.
type Multi struct {
	exporters []Exporter
}

// NewMulti builds a fan-out exporter, skipping nil members.
func NewMulti(exporters ...Exporter) *Multi {
	m := &Multi{}
	for _, e := range exporters {
		if e != nil {
			m.exporters = append(m.exporters, e)
		}
	}
	return m
}

// Write sends the entry to every member. A member whose queue is full drops it
// independently; one slow sink never blocks the others.
func (m *Multi) Write(entry LogEntry) {
	for _, e := range m.exporters {
		e.Write(entry)
	}
}

// Close closes every member.
func (m *Multi) Close() {
	for _, e := range m.exporters {
		e.Close()
	}
}

// Name lists the configured destinations, for the startup log line.
func (m *Multi) Name() string {
	names := make([]string, 0, len(m.exporters))
	for _, e := range m.exporters {
		names = append(names, e.Name())
	}
	return strings.Join(names, "+")
}

// Dropped sums the drop counters of every member.
func (m *Multi) Dropped() int64 {
	var total int64
	for _, e := range m.exporters {
		total += e.Dropped()
	}
	return total
}
