package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/lhpalacio/redash-wire/internal/config"
)

type configPayload struct {
	ConfigPath string `json:"config_path"`
	// Absence is data, not an error: a caller uses this command to find out
	// whether setup is needed, so first run must differ from a broken config.
	Exists bool `json:"exists"`
	// "explicit" (-config), "cwd" (./config.yaml) or "home".
	Source         string           `json:"source"`
	DefaultProfile string           `json:"default_profile"`
	Profiles       []profilePayload `json:"profiles"`
}

type profilePayload struct {
	Name      string `json:"name"`
	RedashURL string `json:"redash_url"`
	APIKeySet bool   `json:"api_key_set"`
	// Omitted unless -show-secrets. The key's only consumer is this binary.
	APIKey *string `json:"api_key,omitempty"`
	// An empty listen address means that listener is disabled for this profile.
	PostgresListenAddr string `json:"postgres_listen_addr"`
	MySQLListenAddr    string `json:"mysql_listen_addr"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	DefaultCredentials bool   `json:"default_credentials"`
	PollInterval       string `json:"poll_interval"`
	PollTimeout        string `json:"poll_timeout"`
	// An invalid profile is still listed with its values, so a caller can show
	// what is wrong.
	Valid bool   `json:"valid"`
	Error string `json:"error"`
}

func cmdConfig(args []string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file to use (default: ./config.yaml, then ~/.redash-wire/config.yaml)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON on stdout")
	showSecrets := fs.Bool("show-secrets", false, "include the Redash API key in the output")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	res, source, cerr := resolveConfigPath(*configPath)
	if cerr != nil {
		return reportError(cerr, *asJSON)
	}

	var sum *config.Summary
	if res.Found {
		var err error
		sum, err = config.LoadAll(res.Path)
		if err != nil {
			return reportError(fail(codeInvalidConfig, err), *asJSON)
		}
	}

	payload := buildConfigPayload(res, source, sum, *showSecrets)

	if *asJSON {
		if cerr := emitJSON(payload); cerr != nil {
			return reportError(cerr, false)
		}
		return exitOK
	}

	printConfig(payload)
	return exitOK
}

func buildConfigPayload(res config.ResolveResult, source string, sum *config.Summary, showSecrets bool) configPayload {
	payload := configPayload{
		ConfigPath: res.Path,
		Exists:     res.Found,
		Source:     source,
		Profiles:   []profilePayload{},
	}
	if sum == nil {
		return payload
	}

	payload.DefaultProfile = sum.DefaultProfile
	for _, p := range sum.Profiles {
		entry := profilePayload{Name: p.Name, Valid: p.Err == nil}
		if p.Err != nil {
			entry.Error = p.Err.Error()
		}
		if p.Config != nil {
			entry.RedashURL = p.Config.RedashURL
			entry.APIKeySet = p.Config.APIKey != ""
			entry.PostgresListenAddr = p.Config.PostgresListenAddr
			entry.MySQLListenAddr = p.Config.MySQLListenAddr
			entry.Username = p.Config.Username
			entry.Password = p.Config.Password
			entry.DefaultCredentials = p.Config.UsesDefaultCredentials()
			entry.PollInterval = p.Config.PollInterval
			entry.PollTimeout = p.Config.PollTimeout
			if showSecrets {
				key := p.Config.APIKey
				entry.APIKey = &key
			}
		}
		payload.Profiles = append(payload.Profiles, entry)
	}

	return payload
}

func printConfig(p configPayload) {
	fmt.Printf("config: %s (%s)\n", shortenHome(p.ConfigPath), p.Source)
	if !p.Exists {
		fmt.Println("status: not configured — run `redash-wire` to set up, or `redash-wire init -url <url>`")
		return
	}
	fmt.Printf("default profile: %s\n", p.DefaultProfile)
	for _, prof := range p.Profiles {
		fmt.Printf("\n[%s]\n", prof.Name)
		fmt.Printf("  redash url:  %s\n", prof.RedashURL)
		fmt.Printf("  api key:     %s\n", boolLabel(prof.APIKeySet, "set", "missing"))
		fmt.Printf("  postgres:    %s\n", addrLabel(prof.PostgresListenAddr))
		fmt.Printf("  mysql:       %s\n", addrLabel(prof.MySQLListenAddr))
		fmt.Printf("  username:    %s\n", prof.Username)
		fmt.Printf("  password:    %s\n", prof.Password)
		if prof.DefaultCredentials {
			fmt.Printf("               (built-in defaults)\n")
		}
		fmt.Printf("  poll:        every %s, timeout %s\n", prof.PollInterval, prof.PollTimeout)
		if !prof.Valid {
			fmt.Fprintf(os.Stderr, "  invalid:     %s\n", prof.Error)
		}
	}
}

func addrLabel(addr string) string {
	if addr == "" {
		return "disabled"
	}
	return addr
}

func boolLabel(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
