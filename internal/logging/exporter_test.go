package logging

// exporter_test.go — internal tests.
//
// Internal (not logging_test) so the shared batching behaviour can be exercised
// with an injected sink. The interesting properties here are the failure modes:
// a stalled sink must never block the caller, and Close must not lose what is
// already queued.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleEntry() LogEntry {
	return LogEntry{
		Time:       time.Now().UTC().Format(time.RFC3339Nano),
		Method:     "GET",
		Path:       "/health",
		Backend:    "http://localhost:4441",
		Status:     200,
		DurationMS: 3,
		Level:      "info",
	}
}

// ---------- batch ----------

// TestBatch_WriteNeverBlocksOnStalledSink pins the central design promise: a
// remote log sink that has stopped responding must not stall the proxy. The
// sink here blocks forever; Write must still return, counting drops.
func TestBatch_WriteNeverBlocksOnStalledSink(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	b := newBatch("test", func([]LogEntry) { <-release })

	// Comfortably more than one queue's worth, so the drop path must trigger.
	for i := 0; i < defaultQueueDepth+2*defaultMaxBatch; i++ {
		b.Write(sampleEntry())
	}

	if got := b.Dropped(); got == 0 {
		t.Fatal("expected drops once the sink stopped draining the queue")
	}

	releaseOnce.Do(func() { close(release) })
	b.Close()
}

func TestBatch_CloseDrainsQueuedEntries(t *testing.T) {
	var mu sync.Mutex
	var got []LogEntry
	b := newBatch("test", func(entries []LogEntry) {
		mu.Lock()
		got = append(got, entries...)
		mu.Unlock()
	})

	const n = 50
	for i := 0; i < n; i++ {
		b.Write(LogEntry{Method: "GET", Path: fmt.Sprintf("/%d", i), Level: "info"})
	}
	b.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Fatalf("Close must flush everything queued: wrote %d, delivered %d", n, len(got))
	}
}

func TestBatch_CloseIsIdempotent(t *testing.T) {
	b := newBatch("test", func([]LogEntry) {})
	b.Close()
	b.Close() // must not panic on a double close
}

func TestBatch_FlushesAtMaxBatch(t *testing.T) {
	flushed := make(chan int, 8)
	b := newBatch("test", func(entries []LogEntry) { flushed <- len(entries) })

	for i := 0; i < defaultMaxBatch; i++ {
		b.Write(sampleEntry())
	}

	select {
	case n := <-flushed:
		if n != defaultMaxBatch {
			t.Fatalf("expected a full batch of %d, got %d", defaultMaxBatch, n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch was not flushed once it reached maxBatch")
	}
	b.Close()
}

// ---------- formatting ----------

func TestFormatText(t *testing.T) {
	line := FormatText(LogEntry{
		Time:       "2026-01-02T03:04:05Z",
		Method:     "POST",
		Path:       "/api",
		Backend:    "http://localhost:4441",
		Status:     201,
		DurationMS: 12,
		Retries:    2,
		Level:      "info",
	})
	for _, want := range []string{"2026-01-02T03:04:05Z", "POST", "/api", "http://localhost:4441", "201", "12ms", "retries=2"} {
		if !strings.Contains(line, want) {
			t.Fatalf("text line %q is missing %q", line, want)
		}
	}
}

func TestFormatText_UnknownTimeAndBackend(t *testing.T) {
	// A breaker entry has no backend and no parseable timestamp; neither may
	// produce an empty field that shifts the columns.
	line := FormatText(LogEntry{Time: "not-a-time", Level: "warn", Method: "BREAKER", Path: "circuit-breaker"})
	if !strings.Contains(line, "not-a-time") {
		t.Fatalf("unparseable timestamp should be passed through, got %q", line)
	}
	if !strings.Contains(line, " - ") {
		t.Fatalf("missing backend should render as -, got %q", line)
	}
}

func TestStdoutExporter_JSON(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "out.json"))
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer f.Close()

	e := &StdoutExporter{Format: "json", Out: f}
	e.Write(sampleEntry())

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatal("each entry must be newline-terminated")
	}
	var got LogEntry
	if err := json.Unmarshal(bytes.TrimSpace(data), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, data)
	}
	if got.Path != "/health" || got.Status != 200 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if e.Name() != "stdout" || e.Dropped() != 0 {
		t.Fatal("stdout exporter must report its name and never drop")
	}
}

func TestFileExporter_TextThenClose(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "haribon.log")
	f, err := os.Create(name)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e := &FileExporter{File: f, Format: "text"}
	e.Write(sampleEntry())
	e.Close()

	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "/health") {
		t.Fatalf("expected the request line in the file, got %q", data)
	}
	// Close nils the handle, so a later write is a no-op rather than a panic.
	e.Write(sampleEntry())
}

// ---------- Loki ----------

func TestLokiExporter_PushBody(t *testing.T) {
	type lokiStream struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	type lokiBody struct {
		Streams []lokiStream `json:"streams"`
	}

	type capture struct {
		path string
		body lokiBody
	}
	got := make(chan capture, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b lokiBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		got <- capture{path: r.URL.Path, body: b}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Trailing slash exercises URL normalisation.
	ex := NewLokiExporter(srv.URL+"/", map[string]string{"env": "test"})
	entry := sampleEntry()
	ex.Write(entry)
	ex.Close()

	ts, err := time.Parse(time.RFC3339Nano, entry.Time)
	if err != nil {
		t.Fatalf("sample entry timestamp is not RFC3339Nano: %v", err)
	}

	select {
	case c := <-got:
		if c.path != "/loki/api/v1/push" {
			t.Fatalf("unexpected push path: %s", c.path)
		}
		if len(c.body.Streams) != 1 {
			t.Fatalf("expected 1 stream, got %d", len(c.body.Streams))
		}
		s := c.body.Streams[0]
		if s.Stream["job"] != "haribon" {
			t.Fatalf("the job label must always be set, got %q", s.Stream["job"])
		}
		if s.Stream["env"] != "test" {
			t.Fatalf("configured labels must be preserved, got %v", s.Stream)
		}
		if len(s.Values) != 1 {
			t.Fatalf("expected 1 value, got %d", len(s.Values))
		}
		// Loki addresses each line by unix nanoseconds as a string.
		if want := fmt.Sprintf("%d", ts.UnixNano()); s.Values[0][0] != want {
			t.Fatalf("timestamp: want %s, got %s", want, s.Values[0][0])
		}
		// The line itself is the JSON log record.
		var line map[string]interface{}
		if err := json.Unmarshal([]byte(s.Values[0][1]), &line); err != nil {
			t.Fatalf("log line is not JSON: %v (%s)", err, s.Values[0][1])
		}
		if line["path"] != "/health" {
			t.Fatalf("log line lost the request path: %v", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Loki exporter never pushed")
	}
}

// TestLokiExporter_UnreachableSinkDoesNotPanic covers the startup case where
// Loki is down: writes must be swallowed, not crash the proxy.
func TestLokiExporter_UnreachableSinkDoesNotPanic(t *testing.T) {
	ex := NewLokiExporter("http://127.0.0.1:1", nil)
	ex.Write(sampleEntry())
	ex.Close()
}

// ---------- Elasticsearch ----------

func TestElasticsearchExporter_BulkNDJSON(t *testing.T) {
	got := make(chan struct {
		path string
		ct   string
		body string
	}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		got <- struct {
			path string
			ct   string
			body string
		}{path: r.URL.Path, ct: r.Header.Get("Content-Type"), body: buf.String()}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ex := NewElasticsearchExporter(srv.URL, "haribon-test")
	ex.Write(sampleEntry())
	ex.Close()

	select {
	case c := <-got:
		if c.path != "/_bulk" {
			t.Fatalf("unexpected path: %s", c.path)
		}
		if !strings.Contains(c.ct, "ndjson") {
			t.Fatalf("bulk requires ndjson content type, got %q", c.ct)
		}
		lines := strings.Split(strings.TrimRight(c.body, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("one entry is an action line plus a doc line, got %d lines: %q", len(lines), c.body)
		}
		var action map[string]map[string]string
		if err := json.Unmarshal([]byte(lines[0]), &action); err != nil {
			t.Fatalf("action line is not JSON: %v (%s)", err, lines[0])
		}
		if action["index"]["_index"] != "haribon-test" {
			t.Fatalf("action line carries the wrong index: %s", lines[0])
		}
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(lines[1]), &doc); err != nil {
			t.Fatalf("doc line is not JSON: %v (%s)", err, lines[1])
		}
		if doc["path"] != "/health" {
			t.Fatalf("doc lost the request path: %v", doc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Elasticsearch exporter never posted")
	}
}

func TestElasticsearchExporter_DefaultIndex(t *testing.T) {
	ex := NewElasticsearchExporter("", "")
	if ex.index != "haribon" {
		t.Fatalf("expected default index haribon, got %q", ex.index)
	}
	if ex.url != "http://localhost:9200" {
		t.Fatalf("expected default url, got %q", ex.url)
	}
	ex.Close()
}

// ---------- Fluent Bit ----------

func TestFluentbitExporter_SendsForwardMessage(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	frame := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 8192)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		frame <- append([]byte(nil), buf[:n]...)
	}()

	ex := NewFluentbitExporter(ln.Addr().String())
	ex.Write(sampleEntry())
	ex.Close()

	select {
	case b := <-frame:
		if len(b) == 0 {
			t.Fatal("empty forward frame")
		}
		// Message mode is a 3-element msgpack array: [tag, time, record].
		if b[0] != 0x93 {
			t.Fatalf("expected msgpack fixarray of 3 (0x93), got 0x%02x", b[0])
		}
		if !bytes.Contains(b, []byte("haribon")) {
			t.Fatal("frame must carry the haribon tag")
		}
		if !bytes.Contains(b, []byte("/health")) {
			t.Fatal("frame must carry the record payload")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no forward message received")
	}
}

func TestFluentbitExporter_UnreachableSinkDoesNotPanic(t *testing.T) {
	ex := NewFluentbitExporter("127.0.0.1:1")
	ex.Write(sampleEntry())
	ex.Close()
}

// ---------- Multi ----------

func TestMulti_FansOutAndNames(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	rec := func(name string) Exporter {
		return &recordingExporter{name: name, onWrite: func() {
			mu.Lock()
			seen[name]++
			mu.Unlock()
		}}
	}

	m := NewMulti(rec("a"), nil, rec("b"))
	m.Write(sampleEntry())
	m.Close()

	mu.Lock()
	defer mu.Unlock()
	if seen["a"] != 1 || seen["b"] != 1 {
		t.Fatalf("every member must receive the entry, got %v", seen)
	}
	if m.Name() != "a+b" {
		t.Fatalf("name should list destinations without the nil member, got %q", m.Name())
	}
	if m.Dropped() != 0 {
		t.Fatalf("expected no drops, got %d", m.Dropped())
	}
}

// TestMulti_OneSlowSinkDoesNotBlockOthers is the reason every remote exporter
// owns its own queue: a sink that has stopped draining must drop its own
// entries and leave the co-exporters untouched.
func TestMulti_OneSlowSinkDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })

	slow := newBatch("slow", func([]LogEntry) { <-release }) // blocks forever
	fast := &recordingExporter{name: "fast"}

	m := NewMulti(slow, fast)
	for i := 0; i < defaultQueueDepth+defaultMaxBatch+10; i++ {
		m.Write(sampleEntry())
	}

	if fast.writes() == 0 {
		t.Fatal("a stalled sink must not starve the others")
	}
	if slow.Dropped() == 0 {
		t.Fatal("the stalled sink should have dropped entries rather than blocking the caller")
	}

	once.Do(func() { close(release) })
	m.Close()
}

// recordingExporter is a minimal Exporter for fan-out tests.
type recordingExporter struct {
	name    string
	onWrite func()
	mu      sync.Mutex
	n       int
}

func (e *recordingExporter) Write(LogEntry) {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	if e.onWrite != nil {
		e.onWrite()
	}
}

func (e *recordingExporter) writes() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

func (e *recordingExporter) Close()         {}
func (e *recordingExporter) Name() string   { return e.name }
func (e *recordingExporter) Dropped() int64 { return 0 }
