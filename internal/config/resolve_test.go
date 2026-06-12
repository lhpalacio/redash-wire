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
