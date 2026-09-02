package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 0600", perm)
	}
}

// The file holds credentials, so a second setup must leave an existing one
// untouched and nothing behind from the attempt.
func TestWriteConfig_RefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("keep: me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := WriteConfig(path, NewFileConfig("p", "http://redash.example.com", "new-key", "u", "pw", true, false))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("WriteConfig over an existing file: got %v, want ErrExists", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep: me\n" {
		t.Errorf("existing config was replaced:\n%s", data)
	}
	assertOnlyConfig(t, dir)
}

// Two setups racing for the same path, each having seen it free: exactly one
// may win, and the file must hold that one's key.
func TestWriteConfig_ConcurrentWritersOnlyOneWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const writers = 8
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fc := NewFileConfig("p", "http://redash.example.com", fmt.Sprintf("key-%d", i), "u", "pw", true, false)
			errs[i] = WriteConfig(path, fc)
		}()
	}
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner == -1:
			winner = i
		case err == nil:
			t.Errorf("writers %d and %d both succeeded", winner, i)
		case !errors.Is(err, ErrExists):
			t.Errorf("writer %d: got %v, want ErrExists", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no writer succeeded")
	}

	cfg, err := Load(path, "p")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := fmt.Sprintf("key-%d", winner); cfg.APIKey != want {
		t.Errorf("APIKey = %q, want the winner's %q", cfg.APIKey, want)
	}
	assertOnlyConfig(t, dir)
}

func assertOnlyConfig(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" {
			t.Errorf("left behind: %s", e.Name())
		}
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
