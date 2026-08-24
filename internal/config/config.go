package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Profile            string `yaml:"-"`
	PostgresListenAddr string `yaml:"postgres_listen_addr,omitempty"`
	MySQLListenAddr    string `yaml:"mysql_listen_addr,omitempty"`
	RedashURL          string `yaml:"redash_url,omitempty"`
	APIKey             string `yaml:"api_key,omitempty"`
	Username           string `yaml:"username,omitempty"`
	Password           string `yaml:"password,omitempty"`
	PollInterval       string `yaml:"poll_interval,omitempty"`
	PollTimeout        string `yaml:"poll_timeout,omitempty"`
}

type FileConfig struct {
	PostgresListenAddr string            `yaml:"postgres_listen_addr,omitempty"`
	MySQLListenAddr    string            `yaml:"mysql_listen_addr,omitempty"`
	Username           string            `yaml:"username,omitempty"`
	Password           string            `yaml:"password,omitempty"`
	PollInterval       string            `yaml:"poll_interval,omitempty"`
	PollTimeout        string            `yaml:"poll_timeout,omitempty"`
	DefaultProfile     string            `yaml:"default_profile"`
	Profiles           map[string]Config `yaml:"profiles"`
}

// DefaultUsername and DefaultPassword are the built-in fallback credentials used
// when a config sets none. They are well-known, so the server warns at startup
// if they are still in effect on a non-loopback listener (see cmd/redash-wire).
const (
	DefaultUsername = "redash-wire"
	DefaultPassword = "supersecret"
)

// Default listen addresses written by the setup wizard.
const (
	DefaultPostgresListenAddr = "127.0.0.1:15432"
	DefaultMySQLListenAddr    = "127.0.0.1:13306"
)

func (c *Config) GetPollInterval() time.Duration {
	d, err := time.ParseDuration(c.PollInterval)
	if err != nil {
		return 500 * time.Millisecond
	}
	return d
}

func (c *Config) GetPollTimeout() time.Duration {
	d, err := time.ParseDuration(c.PollTimeout)
	if err != nil {
		return 120 * time.Second
	}
	return d
}

func Load(path, profile string) (*Config, error) {
	fc, err := readFile(path)
	if err != nil {
		return nil, err
	}

	cfg, err := resolveProfile(fc, profile)
	if err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("profile %q: %w", cfg.Profile, err)
	}

	return cfg, nil
}

// ProfileNotFoundError reports a profile absent from the config file, so callers
// can branch on the condition without matching the message.
type ProfileNotFoundError struct {
	Name      string
	Available []string
}

func (e *ProfileNotFoundError) Error() string {
	return fmt.Sprintf("profile %q not found (available: %s)", e.Name, strings.Join(e.Available, ", "))
}

// ProfileSummary pairs a resolved profile with the error that would stop it
// starting. Config stays populated when Err is set.
type ProfileSummary struct {
	Name   string
	Config *Config
	Err    error
}

// Summary describes every profile in a config file.
type Summary struct {
	DefaultProfile string
	Profiles       []ProfileSummary
}

// LoadAll resolves every profile through the same rules as Load, reporting a
// validation failure per profile instead of failing outright.
func LoadAll(path string) (*Summary, error) {
	fc, err := readFile(path)
	if err != nil {
		return nil, err
	}

	if len(fc.Profiles) == 0 {
		return nil, fmt.Errorf("no profiles defined in config file")
	}

	sum := &Summary{DefaultProfile: fc.DefaultProfile}
	for _, name := range profileNames(fc.Profiles) {
		cfg, err := resolveProfile(fc, name)
		if err != nil {
			sum.Profiles = append(sum.Profiles, ProfileSummary{Name: name, Err: err})
			continue
		}
		sum.Profiles = append(sum.Profiles, ProfileSummary{Name: name, Config: cfg, Err: cfg.Validate()})
	}

	return sum, nil
}

func readFile(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var fc FileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown/misspelled keys instead of silently dropping them
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	return &fc, nil
}

// Layers defaults, then file-level keys, then the profile's own. Callers that act
// on the config validate it; callers that only report it do not.
func resolveProfile(fc *FileConfig, profile string) (*Config, error) {
	if len(fc.Profiles) == 0 {
		return nil, fmt.Errorf("no profiles defined in config file")
	}

	if profile == "" {
		profile = fc.DefaultProfile
	}
	if profile == "" {
		names := profileNames(fc.Profiles)
		return nil, fmt.Errorf("no profile specified and no default_profile set (available: %s)", strings.Join(names, ", "))
	}

	p, ok := fc.Profiles[profile]
	if !ok {
		return nil, &ProfileNotFoundError{Name: profile, Available: profileNames(fc.Profiles)}
	}

	cfg := &Config{
		Username:     DefaultUsername,
		Password:     DefaultPassword,
		PollInterval: "500ms",
		PollTimeout:  "120s",
	}

	overlay(&cfg.PostgresListenAddr, fc.PostgresListenAddr)
	overlay(&cfg.MySQLListenAddr, fc.MySQLListenAddr)
	overlay(&cfg.Username, fc.Username)
	overlay(&cfg.Password, fc.Password)
	overlay(&cfg.PollInterval, fc.PollInterval)
	overlay(&cfg.PollTimeout, fc.PollTimeout)

	overlay(&cfg.PostgresListenAddr, p.PostgresListenAddr)
	overlay(&cfg.MySQLListenAddr, p.MySQLListenAddr)
	overlay(&cfg.RedashURL, p.RedashURL)
	overlay(&cfg.APIKey, p.APIKey)
	overlay(&cfg.Username, p.Username)
	overlay(&cfg.Password, p.Password)
	overlay(&cfg.PollInterval, p.PollInterval)
	overlay(&cfg.PollTimeout, p.PollTimeout)

	// Only the URL and API key support ${ENV_VAR} expansion (as documented in
	// config.example.yaml). Username/password are taken literally so a literal
	// '$' is never mangled and an unset var can't silently blank the password.
	cfg.APIKey = os.ExpandEnv(cfg.APIKey)
	cfg.RedashURL = os.ExpandEnv(cfg.RedashURL)

	cfg.Profile = profile

	return cfg, nil
}

func (c *Config) Validate() error {
	var errs []error
	if c.PostgresListenAddr == "" && c.MySQLListenAddr == "" {
		errs = append(errs, fmt.Errorf(`no listeners enabled: set postgres_listen_addr: "127.0.0.1:15432" and/or mysql_listen_addr: "127.0.0.1:13306"`))
	}
	if c.RedashURL == "" {
		errs = append(errs, fmt.Errorf("redash_url is required"))
	}
	if c.APIKey == "" {
		errs = append(errs, fmt.Errorf("api_key is required (check that any ${ENV_VAR} in api_key is set)"))
	}
	if c.Username == "" {
		errs = append(errs, fmt.Errorf("username must not be empty"))
	}
	if c.Password == "" {
		errs = append(errs, fmt.Errorf("password must not be empty"))
	}
	if d, err := time.ParseDuration(c.PollInterval); err != nil {
		errs = append(errs, fmt.Errorf("invalid poll_interval %q: %w", c.PollInterval, err))
	} else if d <= 0 {
		errs = append(errs, fmt.Errorf("poll_interval must be positive, got %q", c.PollInterval))
	}
	if d, err := time.ParseDuration(c.PollTimeout); err != nil {
		errs = append(errs, fmt.Errorf("invalid poll_timeout %q: %w", c.PollTimeout, err))
	} else if d <= 0 {
		errs = append(errs, fmt.Errorf("poll_timeout must be positive, got %q", c.PollTimeout))
	}
	return errors.Join(errs...)
}

// UsesDefaultCredentials reports whether the effective username/password are
// still the built-in defaults, so callers can warn on non-loopback listeners.
func (c *Config) UsesDefaultCredentials() bool {
	return c.Username == DefaultUsername && c.Password == DefaultPassword
}

// overlay implements the "later layer wins unless unset" precedence used
// across config layers.
func overlay(dst *string, src string) {
	if src != "" {
		*dst = src
	}
}

func profileNames(profiles map[string]Config) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
