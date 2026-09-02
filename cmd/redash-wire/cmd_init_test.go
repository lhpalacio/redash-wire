package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Without -config, init writes the documented default. It must not go through
// the serve lookup, where a stray ./config.yaml would make it refuse.
func TestInitTargetIgnoresCwdConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, "config.yaml"), []byte("profiles: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, cerr := initTarget("")
	if cerr != nil {
		t.Fatalf("initTarget: %v (%s)", cerr, cerr.Code)
	}
	if want := filepath.Join(home, ".redash-wire", "config.yaml"); got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

func TestInitTarget(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.yaml")
	if err := os.WriteFile(existing, []byte("profiles: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		flagPath string
		wantCode string
		wantMsg  string
	}{
		{name: "free path", flagPath: filepath.Join(dir, "new", "config.yaml")},
		{name: "existing file", flagPath: existing, wantCode: codeConfigExists},
		{name: "directory", flagPath: dir, wantCode: codeIOError, wantMsg: "is a directory"},
		{name: "parent is a file", flagPath: filepath.Join(existing, "config.yaml"), wantCode: codeIOError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cerr := initTarget(tt.flagPath)
			if tt.wantCode == "" {
				if cerr != nil {
					t.Fatalf("unexpected error: %v", cerr)
				}
				if got != tt.flagPath {
					t.Errorf("target = %q, want %q", got, tt.flagPath)
				}
				return
			}
			if cerr == nil {
				t.Fatalf("expected %s, got path %q", tt.wantCode, got)
			}
			if cerr.Code != tt.wantCode {
				t.Errorf("code = %q, want %q (%v)", cerr.Code, tt.wantCode, cerr)
			}
			if !strings.Contains(cerr.Error(), tt.wantMsg) {
				t.Errorf("message %q does not mention %q", cerr.Error(), tt.wantMsg)
			}
		})
	}
}

// A bare host is a usage mistake, as it is in the wizard; it must not go out on
// the network and come back as connection_failed.
func TestInitURL(t *testing.T) {
	tests := []struct {
		raw      string
		want     string
		wantCode string
	}{
		{raw: "https://redash.example.com", want: "https://redash.example.com"},
		{raw: "https://redash.example.com/", want: "https://redash.example.com"},
		{raw: "http://localhost:5000", want: "http://localhost:5000"},
		{raw: "redash.example.com", wantCode: codeUsage},
		{raw: "ftp://redash.example.com", wantCode: codeUsage},
		{raw: "https://", wantCode: codeUsage},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, cerr := initURL(tt.raw)
			if tt.wantCode != "" {
				if cerr == nil {
					t.Fatalf("expected %s, got %q", tt.wantCode, got)
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
				t.Errorf("url = %q, want %q", got, tt.want)
			}
		})
	}
}
