package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfig_CreatesDirectoriesAndFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "config.yaml")

	fc := NewFileConfig("test", "http://redash.example.com", "mykey123", "redash-wire", "pw", true, false)
	if err := WriteConfig(path, fc); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("config file is empty")
	}
}

func TestWriteConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	fc := NewFileConfig("myprofile", "http://redash.test.com", "secret-key", "redash-wire", "pw", true, false)
	if err := WriteConfig(path, fc); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, "myprofile")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.RedashURL != "http://redash.test.com" {
		t.Errorf("RedashURL = %q, want %q", cfg.RedashURL, "http://redash.test.com")
	}
	if cfg.APIKey != "secret-key" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "secret-key")
	}
	if cfg.Profile != "myprofile" {
		t.Errorf("Profile = %q, want %q", cfg.Profile, "myprofile")
	}
	if cfg.PostgresListenAddr != "127.0.0.1:15432" {
		t.Errorf("PostgresListenAddr = %q, want default", cfg.PostgresListenAddr)
	}
}

// TestWriteConfig_RoundTrip_MySQLOnly guards the regression where a MySQL-only
// wizard config silently started the PostgreSQL listener anyway, because Load
// re-defaulted the omitted address.
func TestWriteConfig_RoundTrip_MySQLOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	fc := NewFileConfig("myprofile", "http://redash.test.com", "secret-key", "redash-wire", "pw", false, true)
	if err := WriteConfig(path, fc); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# postgres_listen_addr: "+DefaultPostgresListenAddr) {
		t.Errorf("MySQL-only config should contain postgres_listen_addr as a comment:\n%s", data)
	}
	if strings.Contains(string(data), "\npostgres_listen_addr") {
		t.Errorf("MySQL-only config should not contain an active postgres_listen_addr:\n%s", data)
	}

	cfg, err := Load(path, "myprofile")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.PostgresListenAddr != "" {
		t.Errorf("PostgresListenAddr = %q, want empty (disabled)", cfg.PostgresListenAddr)
	}
	if cfg.MySQLListenAddr != "127.0.0.1:13306" {
		t.Errorf("MySQLListenAddr = %q, want default", cfg.MySQLListenAddr)
	}
}

// TestWriteConfig_PostgresOnly_CommentsOutMySQL checks that the listener the
// wizard left disabled still shows up in the file as a commented-out line.
func TestWriteConfig_PostgresOnly_CommentsOutMySQL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	fc := NewFileConfig("myprofile", "http://redash.test.com", "secret-key", "redash-wire", "pw", true, false)
	if err := WriteConfig(path, fc); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# mysql_listen_addr: "+DefaultMySQLListenAddr) {
		t.Errorf("Postgres-only config should contain mysql_listen_addr as a comment:\n%s", data)
	}

	cfg, err := Load(path, "myprofile")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.MySQLListenAddr != "" {
		t.Errorf("MySQLListenAddr = %q, want empty (disabled)", cfg.MySQLListenAddr)
	}
	if cfg.PostgresListenAddr != DefaultPostgresListenAddr {
		t.Errorf("PostgresListenAddr = %q, want default", cfg.PostgresListenAddr)
	}
}

func TestNewFileConfig_NoListenersForcesPostgres(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	fc := NewFileConfig("myprofile", "http://redash.test.com", "secret-key", "redash-wire", "pw", false, false)
	if err := WriteConfig(path, fc); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, "myprofile")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.PostgresListenAddr != "127.0.0.1:15432" {
		t.Errorf("PostgresListenAddr = %q, want forced default", cfg.PostgresListenAddr)
	}
}
