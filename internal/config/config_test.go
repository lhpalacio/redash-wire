package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return p
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name       string
		yaml       string
		profile    string
		envSetup   func(t *testing.T)
		wantErr    string
		wantConfig *Config
	}{
		{
			name: "valid config with default profile",
			yaml: `
postgres_listen_addr: 127.0.0.1:15432
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
    api_key: secret123
`,
			profile: "",
			wantConfig: &Config{
				PostgresListenAddr: "127.0.0.1:15432",
				RedashURL:          "https://redash.example.com",
				APIKey:             "secret123",
				PollInterval:       "500ms",
				PollTimeout:        "120s",
			},
		},
		{
			name: "profile flag overrides default_profile",
			yaml: `
postgres_listen_addr: 127.0.0.1:15432
default_profile: dev
profiles:
  dev:
    redash_url: https://dev.example.com
    api_key: dev-key
  staging:
    redash_url: https://staging.example.com
    api_key: staging-key
`,
			profile: "staging",
			wantConfig: &Config{
				PostgresListenAddr: "127.0.0.1:15432",
				RedashURL:          "https://staging.example.com",
				APIKey:             "staging-key",
				PollInterval:       "500ms",
				PollTimeout:        "120s",
			},
		},
		{
			name: "env var expansion in api_key and redash_url",
			yaml: `
postgres_listen_addr: 127.0.0.1:15432
default_profile: prod
profiles:
  prod:
    redash_url: $REDASH_TEST_URL
    api_key: ${REDASH_TEST_KEY}
`,
			profile: "",
			envSetup: func(t *testing.T) {
				t.Setenv("REDASH_TEST_URL", "https://env.example.com")
				t.Setenv("REDASH_TEST_KEY", "env-secret")
			},
			wantConfig: &Config{
				PostgresListenAddr: "127.0.0.1:15432",
				RedashURL:          "https://env.example.com",
				APIKey:             "env-secret",
				PollInterval:       "500ms",
				PollTimeout:        "120s",
			},
		},
		{
			name: "missing redash_url",
			yaml: `
default_profile: dev
profiles:
  dev:
    api_key: secret
`,
			profile: "",
			wantErr: "redash_url is required",
		},
		{
			name: "missing api_key",
			yaml: `
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
`,
			profile: "",
			wantErr: "api_key is required",
		},
		{
			name: "invalid poll_interval duration",
			yaml: `
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
    api_key: secret
    poll_interval: not-a-duration
`,
			profile: "",
			wantErr: "invalid poll_interval",
		},
		{
			name: "invalid poll_timeout duration",
			yaml: `
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
    api_key: secret
    poll_timeout: garbage
`,
			profile: "",
			wantErr: "invalid poll_timeout",
		},
		{
			name: "profile not found",
			yaml: `
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
    api_key: secret
`,
			profile: "prod",
			wantErr: `profile "prod" not found`,
		},
		{
			name: "no profiles defined",
			yaml: `
default_profile: dev
`,
			profile: "",
			wantErr: "no profiles defined",
		},
		{
			name: "no default_profile and no flag",
			yaml: `
profiles:
  alpha:
    redash_url: https://alpha.example.com
    api_key: key
  beta:
    redash_url: https://beta.example.com
    api_key: key
`,
			profile: "",
			wantErr: "no profile specified and no default_profile set",
		},
		{
			name: "file-level defaults override hardcoded",
			yaml: `
postgres_listen_addr: 0.0.0.0:9999
mysql_listen_addr: 0.0.0.0:3307
poll_interval: 1s
poll_timeout: 60s
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
    api_key: secret
`,
			profile: "",
			wantConfig: &Config{
				PostgresListenAddr: "0.0.0.0:9999",
				MySQLListenAddr:    "0.0.0.0:3307",
				RedashURL:          "https://redash.example.com",
				APIKey:             "secret",
				PollInterval:       "1s",
				PollTimeout:        "60s",
			},
		},
		{
			name: "profile overrides file-level defaults",
			yaml: `
postgres_listen_addr: 0.0.0.0:9999
poll_interval: 1s
poll_timeout: 60s
default_profile: custom
profiles:
  custom:
    postgres_listen_addr: 10.0.0.1:5555
    redash_url: https://redash.example.com
    api_key: secret
    poll_interval: 2s
    poll_timeout: 30s
`,
			profile: "",
			wantConfig: &Config{
				PostgresListenAddr: "10.0.0.1:5555",
				RedashURL:          "https://redash.example.com",
				APIKey:             "secret",
				PollInterval:       "2s",
				PollTimeout:        "30s",
			},
		},
		{
			name: "mysql-only config disables postgres",
			yaml: `
mysql_listen_addr: 127.0.0.1:13306
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
    api_key: secret
`,
			profile: "",
			wantConfig: &Config{
				PostgresListenAddr: "",
				MySQLListenAddr:    "127.0.0.1:13306",
				RedashURL:          "https://redash.example.com",
				APIKey:             "secret",
				PollInterval:       "500ms",
				PollTimeout:        "120s",
			},
		},
		{
			name: "no listeners configured",
			yaml: `
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
    api_key: secret
`,
			profile: "",
			wantErr: "no listeners enabled",
		},
		{
			name: "misspelled key is rejected",
			yaml: `
postgres_listen_adr: 127.0.0.1:15432
default_profile: dev
profiles:
  dev:
    redash_url: https://redash.example.com
    api_key: secret
`,
			profile: "",
			wantErr: "field postgres_listen_adr not found",
		},
		{
			name: "profile enables postgres over file-level mysql-only",
			yaml: `
mysql_listen_addr: 127.0.0.1:13306
default_profile: dev
profiles:
  dev:
    postgres_listen_addr: 127.0.0.1:25432
    redash_url: https://redash.example.com
    api_key: secret
`,
			profile: "",
			wantConfig: &Config{
				PostgresListenAddr: "127.0.0.1:25432",
				MySQLListenAddr:    "127.0.0.1:13306",
				RedashURL:          "https://redash.example.com",
				APIKey:             "secret",
				PollInterval:       "500ms",
				PollTimeout:        "120s",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSetup != nil {
				tt.envSetup(t)
			}

			path := writeYAML(t, tt.yaml)
			cfg, err := Load(path, tt.profile)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if cfg.PostgresListenAddr != tt.wantConfig.PostgresListenAddr {
				t.Errorf("PostgresListenAddr = %q, want %q", cfg.PostgresListenAddr, tt.wantConfig.PostgresListenAddr)
			}
			if cfg.MySQLListenAddr != tt.wantConfig.MySQLListenAddr {
				t.Errorf("MySQLListenAddr = %q, want %q", cfg.MySQLListenAddr, tt.wantConfig.MySQLListenAddr)
			}
			if cfg.RedashURL != tt.wantConfig.RedashURL {
				t.Errorf("RedashURL = %q, want %q", cfg.RedashURL, tt.wantConfig.RedashURL)
			}
			if cfg.APIKey != tt.wantConfig.APIKey {
				t.Errorf("APIKey = %q, want %q", cfg.APIKey, tt.wantConfig.APIKey)
			}
			if cfg.PollInterval != tt.wantConfig.PollInterval {
				t.Errorf("PollInterval = %q, want %q", cfg.PollInterval, tt.wantConfig.PollInterval)
			}
			if cfg.PollTimeout != tt.wantConfig.PollTimeout {
				t.Errorf("PollTimeout = %q, want %q", cfg.PollTimeout, tt.wantConfig.PollTimeout)
			}
		})
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml", "")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "reading config file") {
		t.Fatalf("error %q does not mention reading config file", err)
	}
}

func TestGetPollInterval(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "valid duration", value: "2s", want: 2 * time.Second},
		{name: "valid milliseconds", value: "250ms", want: 250 * time.Millisecond},
		{name: "empty falls back to default", value: "", want: 500 * time.Millisecond},
		{name: "invalid falls back to default", value: "not-a-duration", want: 500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{PollInterval: tt.value}
			got := c.GetPollInterval()
			if got != tt.want {
				t.Errorf("GetPollInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetPollTimeout(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "valid duration", value: "60s", want: 60 * time.Second},
		{name: "valid minutes", value: "5m", want: 5 * time.Minute},
		{name: "empty falls back to default", value: "", want: 120 * time.Second},
		{name: "invalid falls back to default", value: "bad", want: 120 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{PollTimeout: tt.value}
			got := c.GetPollTimeout()
			if got != tt.want {
				t.Errorf("GetPollTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A default_profile naming a missing profile stops serve, so the report for the
// same file has to say so instead of calling every profile valid.
func TestLoadAll_DefaultProfileMissing(t *testing.T) {
	path := writeYAML(t, `
postgres_listen_addr: 127.0.0.1:15432
default_profile: prod
profiles:
  dev:
    redash_url: https://dev.example.com
    api_key: dev-key
`)

	_, err := LoadAll(path)
	if err == nil || !strings.Contains(err.Error(), `default_profile "prod" not found`) {
		t.Fatalf("LoadAll: got %v, want a default_profile error", err)
	}

	// Serve keeps its own error on this file, and -profile still works.
	var notFound *ProfileNotFoundError
	if _, err := Load(path, ""); !errors.As(err, &notFound) {
		t.Errorf("Load without a profile: got %v, want ProfileNotFoundError", err)
	}
	if _, err := Load(path, "dev"); err != nil {
		t.Errorf("Load with an explicit profile: %v", err)
	}
}

// No default_profile at all is fine for the report: -profile picks one.
func TestLoadAll_NoDefaultProfile(t *testing.T) {
	path := writeYAML(t, `
postgres_listen_addr: 127.0.0.1:15432
profiles:
  dev:
    redash_url: https://dev.example.com
    api_key: dev-key
`)

	sum, err := LoadAll(path)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(sum.Profiles) != 1 || sum.Profiles[0].Err != nil {
		t.Errorf("unexpected summary: %+v", sum.Profiles)
	}
}
