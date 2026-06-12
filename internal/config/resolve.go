package config

import (
	"os"
	"path/filepath"
)

type ResolveResult struct {
	Path  string
	Found bool
	// FromCwd is true when the config was auto-discovered as ./config.yaml in the
	// working directory (rather than via -config or the home-dir default). Callers
	// should warn before using it, since running in an untrusted directory could
	// load an attacker-supplied config that exfiltrates env-expanded secrets.
	FromCwd bool
}

func Resolve(flagPath string, flagExplicit bool) (ResolveResult, error) {
	if flagExplicit {
		abs, err := filepath.Abs(flagPath)
		if err != nil {
			return ResolveResult{}, err
		}
		_, err = os.Stat(abs)
		return ResolveResult{Path: abs, Found: err == nil}, nil
	}

	localPath, err := filepath.Abs("config.yaml")
	if err == nil {
		if _, err := os.Stat(localPath); err == nil {
			return ResolveResult{Path: localPath, Found: true, FromCwd: true}, nil
		}
	}

	defaultPath, err := DefaultConfigPath()
	if err != nil {
		return ResolveResult{}, err
	}
	if _, err := os.Stat(defaultPath); err == nil {
		return ResolveResult{Path: defaultPath, Found: true}, nil
	}

	return ResolveResult{Path: defaultPath, Found: false}, nil
}

func DefaultConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".redash-wire"), nil
}

func DefaultConfigPath() (string, error) {
	dir, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}
