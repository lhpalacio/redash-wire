package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func NewFileConfig(profileName, redashURL, apiKey, username, password string, enablePostgres, enableMySQL bool) *FileConfig {
	// A written config must always pass the at-least-one-listener validation in
	// Load, regardless of caller bugs.
	if !enablePostgres && !enableMySQL {
		enablePostgres = true
	}
	fc := &FileConfig{
		Username:       username,
		Password:       password,
		PollInterval:   "500ms",
		PollTimeout:    "120s",
		DefaultProfile: profileName,
		Profiles: map[string]Config{
			profileName: {
				RedashURL: redashURL,
				APIKey:    apiKey,
			},
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

// WriteConfig writes the config atomically with owner-only permissions, since it
// holds the Redash API key and proxy password in cleartext. It writes to a temp
// file in the same directory and renames into place so a crash or disk-full mid
// write can never leave a truncated config behind.
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
	defer os.Remove(tmpName) // no-op once the rename succeeds

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

	if err := os.Rename(tmpName, path); err != nil {
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
