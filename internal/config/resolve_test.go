package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve_FlagExplicit_FileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "my.yaml")
	os.WriteFile(path, []byte("profiles:\n  x:\n    redash_url: http://x\n    api_key: k\n"), 0o644)

	result, err := Resolve(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found {
		t.Error("expected Found=true")
	}
	if result.Path != path {
		t.Errorf("got path %q, want %q", result.Path, path)
	}
}

func TestResolve_FlagExplicit_FileMissing(t *testing.T) {
	result, err := Resolve("/nonexistent/config.yaml", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Found {
		t.Error("expected Found=false")
	}
}

func TestResolve_NoFlag_LocalExists(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("test"), 0o644)

	result, err := Resolve("config.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found {
		t.Error("expected Found=true")
	}
	if filepath.Base(result.Path) != "config.yaml" {
		t.Errorf("unexpected path: %s", result.Path)
	}
}

func TestResolve_NoFlag_NoneFound(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)

	result, err := Resolve("config.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Found {
		t.Error("expected Found=false")
	}
	if filepath.Base(result.Path) != "config.yaml" {
		t.Errorf("expected default path, got %s", result.Path)
	}
}

// An explicit -config must never fall back to ./config.yaml. The macOS app relies
// on this: it runs with a working directory it does not control, and picking up a
// stray config there would silently point the proxy at someone else's Redash.
func TestResolve_FlagExplicit_IgnoresCwdConfig(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	cwdConfig := filepath.Join(cwd, "config.yaml")
	if err := os.WriteFile(cwdConfig, []byte("profiles:\n  cwd:\n    redash_url: http://attacker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	explicit := filepath.Join(t.TempDir(), "chosen.yaml")
	if err := os.WriteFile(explicit, []byte("profiles:\n  x:\n    redash_url: http://x\n    api_key: k\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Resolve(explicit, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != explicit {
		t.Errorf("got path %q, want the explicit path %q", result.Path, explicit)
	}
	if result.Path == cwdConfig {
		t.Error("explicit path resolved to the working directory config")
	}
	if result.FromCwd {
		t.Error("expected FromCwd=false for an explicit path")
	}
}

// A missing explicit path must report itself missing rather than quietly falling
// back to a config that happens to sit in the working directory.
func TestResolve_FlagExplicit_MissingDoesNotFallBackToCwd(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, "config.yaml"), []byte("profiles:\n  cwd: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(t.TempDir(), "absent.yaml")

	result, err := Resolve(missing, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Found {
		t.Error("expected Found=false; an explicit path must not fall back to the working directory")
	}
	if result.FromCwd {
		t.Error("expected FromCwd=false")
	}
	if result.Path != missing {
		t.Errorf("got path %q, want %q", result.Path, missing)
	}
}
