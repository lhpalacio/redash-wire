package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// The app parses this stream line by line, so every record has to be valid JSON
// on its own and carry a timestamp it can order events by.
func TestNewLoggerJSON(t *testing.T) {
	var buf bytes.Buffer
	logger, err := newLogger(&buf, formatJSON, false)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}

	logger.Info("listening (postgres)", "addr", "127.0.0.1:15432")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, buf.String())
	}

	if rec["msg"] != "listening (postgres)" {
		t.Errorf("msg = %v, want %q", rec["msg"], "listening (postgres)")
	}
	if rec["addr"] != "127.0.0.1:15432" {
		t.Errorf("addr = %v, want the resolved listen address", rec["addr"])
	}
	if rec["level"] != "info" {
		t.Errorf("level = %v, want info", rec["level"])
	}

	ts, ok := rec["time"].(string)
	if !ok {
		t.Fatalf("no time field in %v", rec)
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("time %q is not RFC3339: %v", ts, err)
	}
}

// Debug events have to reach the stream too, or the app's log window shows
// nothing useful when someone turns debugging on.
func TestNewLoggerJSONDebugLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, err := newLogger(&buf, formatJSON, true)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}

	logger.Debug("query dispatched", "data_source_id", 3)

	if !strings.Contains(buf.String(), "query dispatched") {
		t.Errorf("debug event missing with debug enabled: %q", buf.String())
	}
}

// The human format must stay exactly as it was; it is what the terminal shows.
func TestNewLoggerTextIsNotJSON(t *testing.T) {
	var buf bytes.Buffer
	logger, err := newLogger(&buf, formatText, false)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}

	logger.Info("listening (postgres)", "addr", "127.0.0.1:15432")

	var rec map[string]any
	if json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec) == nil {
		t.Errorf("text format produced JSON: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "listening (postgres)") {
		t.Errorf("text output missing the message: %q", buf.String())
	}
}

func TestNewLoggerRejectsUnknownFormat(t *testing.T) {
	if _, err := newLogger(io.Discard, "ndjson", false); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadAPIKeyFrom(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		want     string
		wantCode string
	}{
		{name: "plain key", input: "rdsh_abc123", want: "rdsh_abc123"},
		{name: "trailing newline from echo", input: "rdsh_abc123\n", want: "rdsh_abc123"},
		{name: "surrounding whitespace", input: "  rdsh_abc123 \r\n", want: "rdsh_abc123"},
		{name: "empty", input: "", wantCode: codeUsage},
		{name: "whitespace only", input: " \n\t ", wantCode: codeUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cerr := readAPIKeyFrom(strings.NewReader(tt.input))
			if tt.wantCode != "" {
				if cerr == nil {
					t.Fatalf("expected error %q, got key %q", tt.wantCode, got)
				}
				if cerr.Code != tt.wantCode {
					t.Errorf("code = %q, want %q", cerr.Code, tt.wantCode)
				}
				return
			}
			if cerr != nil {
				t.Fatalf("unexpected error: %v", cerr)
			}
			if got != tt.want {
				t.Errorf("key = %q, want %q", got, tt.want)
			}
		})
	}
}

// A mistakenly piped file is truncated rather than read into memory whole.
func TestReadAPIKeyFromCapsInput(t *testing.T) {
	got, cerr := readAPIKeyFrom(strings.NewReader(strings.Repeat("k", maxAPIKeyBytes*3)))
	if cerr != nil {
		t.Fatalf("unexpected error: %v", cerr)
	}
	if len(got) != maxAPIKeyBytes {
		t.Errorf("read %d bytes, want the %d-byte cap", len(got), maxAPIKeyBytes)
	}
}

func TestReadAPIKeyFromReadError(t *testing.T) {
	_, cerr := readAPIKeyFrom(failingReader{})
	if cerr == nil {
		t.Fatal("expected an error")
	}
	if cerr.Code != codeIOError {
		t.Errorf("code = %q, want %q", cerr.Code, codeIOError)
	}
}

// Only a path inside the home directory gets the ~: a sibling that merely
// shares the prefix, like /Users/x/homebrew next to a home of /Users/x/home,
// does not.
func TestShortenHome(t *testing.T) {
	t.Setenv("HOME", "/Users/x/home")
	tests := map[string]string{
		"/Users/x/home/.redash-wire/config.yaml": "~/.redash-wire/config.yaml",
		"/Users/x/home":                          "~",
		"/Users/x/homebrew/config.yaml":          "/Users/x/homebrew/config.yaml",
		"/Users/x/config.yaml":                   "/Users/x/config.yaml",
		"/opt/config.yaml":                       "/opt/config.yaml",
	}
	for path, want := range tests {
		if got := shortenHome(path); got != want {
			t.Errorf("shortenHome(%q) = %q, want %q", path, got, want)
		}
	}
}
