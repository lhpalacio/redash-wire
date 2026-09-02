package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lhpalacio/redash-wire/internal/config"
)

// The README reserves io_error for a file that could not be read; anything the
// file itself gets wrong is invalid_config. Driven through config.Load so the
// test sees the errors that package really returns.
func TestClassifyLoadError(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, perm os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		return p
	}
	valid := "postgres_listen_addr: 127.0.0.1:15432\ndefault_profile: dev\nprofiles:\n  dev:\n    redash_url: https://x\n    api_key: k\n"

	type loadCase struct {
		name, path, profile, want string
	}
	tests := []loadCase{
		{"directory", dir, "", codeIOError},
		{"missing file", filepath.Join(dir, "absent.yaml"), "", codeIOError},
		{"bad yaml", write("bad.yaml", "profiles: [", 0o600), "", codeInvalidConfig},
		{"unknown key", write("unknown.yaml", "nope: 1\n"+valid, 0o600), "", codeInvalidConfig},
		{"profile fails validation", write("nokey.yaml", "default_profile: dev\nprofiles:\n  dev:\n    redash_url: https://x\n", 0o600), "", codeInvalidConfig},
		{"profile not found", write("valid.yaml", valid, 0o600), "prod", codeProfileNotFound},
	}
	if os.Geteuid() != 0 { // root reads anything
		tests = append(tests, loadCase{"permission denied", write("locked.yaml", valid, 0o000), "", codeIOError})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(tt.path, tt.profile)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := classifyLoadError(err).Code; got != tt.want {
				t.Errorf("classified as %q, want %q (error: %v)", got, tt.want, err)
			}
		})
	}
}
