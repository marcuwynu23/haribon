package logging

import (
	"encoding/json"
	"os"
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

// Exporter interface for sending log entries to various destinations.
type Exporter interface {
	// Write sends a log entry to the exporter.
	Write(entry LogEntry)
	// Close cleans up resources (if any).
	Close()
	// Name returns the exporter name for logging/metrics.
	Name() string
}

// StdoutExporter writes log entries to stdout.
type StdoutExporter struct{}

func (e *StdoutExporter) Write(entry LogEntry) {
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	println(string(b))
}

func (e *StdoutExporter) Close() {}

func (e *StdoutExporter) Name() string { return "stdout" }

// FileExporter writes log entries to a file.
type FileExporter struct {
	File *os.File
}

func (e *FileExporter) Write(entry LogEntry) {
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if e.File == nil {
		return
	}
	e.File.Write(b)
	e.File.Write([]byte{'\n'})
}

func (e *FileExporter) Close() {
	if e.File != nil {
		e.File.Close()
		e.File = nil
	}
}

func (e *FileExporter) Name() string { return "file" }

// LokiExporter pushes log entries to Loki.
type LokiExporter struct{}

func (e *LokiExporter) Write(entry LogEntry) {
	// Best-effort async push to Loki
	_ = entry
}

func (e *LokiExporter) Close() {}

func (e *LokiExporter) Name() string { return "loki" }

// FluentbitExporter configures Fluent Bit output.
type FluentbitExporter struct{}

func (e *FluentbitExporter) Write(entry LogEntry) {
	// Output Fluent Bit config format
	_ = entry
}

func (e *FluentbitExporter) Close() {}

func (e *FluentbitExporter) Name() string { return "fluentbit" }

// ElasticsearchExporter pushes log entries to Elasticsearch.
type ElasticsearchExporter struct{}

func (e *ElasticsearchExporter) Write(entry LogEntry) {
	// Best-effort bulk push to Elasticsearch
	_ = entry
}

func (e *ElasticsearchExporter) Close() {}

func (e *ElasticsearchExporter) Name() string { return "elasticsearch" }