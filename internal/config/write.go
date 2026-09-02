package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ErrExists reports that WriteConfig found a file already at the path and left
// it alone. Callers test for it with errors.Is.
var ErrExists = errors.New("config file already exists")

// NewFileConfig builds a one-profile config the way the wizard and init write
// it. readOnly is recorded on the profile, and only when set, so a writable
// config carries no read_only line to puzzle over.
func NewFileConfig(profileName, redashURL, apiKey, username, password string, enablePostgres, enableMySQL, readOnly bool) *FileConfig {
	// A written config must always pass the at-least-one-listener validation in
	// Load, regardless of caller bugs.
	if !enablePostgres && !enableMySQL {
		enablePostgres = true
	}
	profile := Config{
		RedashURL: redashURL,
		APIKey:    apiKey,
	}
	if readOnly {
		profile.ReadOnly = &readOnly
	}
	fc := &FileConfig{
		Username:       username,
		Password:       password,
		PollInterval:   "500ms",
		PollTimeout:    "120s",
		DefaultProfile: profileName,
		Profiles: map[string]Config{
			profileName: profile,
		},
	}
	if enablePostgres {
		fc.PostgresListenAddr = DefaultPostgresListenAddr
	}
	if enableMySQL {
		fc.MySQLListenAddr = DefaultMySQLListenAddr
	}
	return fc
}

// WriteConfig creates the config atomically with owner-only permissions, since
// it holds the Redash API key and proxy password in cleartext. It writes to a
// temp file in the same directory and links it into place so a crash or
// disk-full mid write can never leave a truncated config behind. It never
// replaces an existing file: the link fails with ErrExists instead, and because
// that check is the create itself, two setups racing for the same path cannot
// both succeed.
func WriteConfig(path string, fc *FileConfig) error {
	var root yaml.Node
	if err := root.Encode(fc); err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	commentDisabledListener(&root, fc)

	data, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // after a successful link this only drops the temp name

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing config: %w", err)
	}

	// A hard link, unlike a rename, refuses an existing target.
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w at %s", ErrExists, path)
		}
		return fmt.Errorf("finalizing config file: %w", err)
	}

	return nil
}

// commentDisabledListener renders a listener that was left disabled as a
// commented-out line next to the active one, so enabling it later is an
// uncomment instead of a docs lookup.
func commentDisabledListener(root *yaml.Node, fc *FileConfig) {
	if root.Kind != yaml.MappingNode {
		return
	}
	switch {
	case fc.PostgresListenAddr == "" && fc.MySQLListenAddr != "":
		setHeadComment(root, "mysql_listen_addr", "postgres_listen_addr: "+DefaultPostgresListenAddr)
	case fc.MySQLListenAddr == "" && fc.PostgresListenAddr != "":
		// Attach to the first key after the listener block so the comment
		// lands right below the active postgres line.
		for i := 0; i+1 < len(root.Content); i += 2 {
			if key := root.Content[i].Value; key != "postgres_listen_addr" && key != "mysql_listen_addr" {
				setHeadComment(root, key, "mysql_listen_addr: "+DefaultMySQLListenAddr)
				return
			}
		}
	}
}

func setHeadComment(root *yaml.Node, key, comment string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i].HeadComment = comment
			return
		}
	}
}
