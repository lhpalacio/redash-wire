package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lhpalacio/redash-wire/internal/config"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/setup"
	"golang.org/x/term"
)

const maxAPIKeyBytes = 4096

type initPayload struct {
	ConfigPath      string `json:"config_path"`
	Profile         string `json:"profile"`
	RedashURL       string `json:"redash_url"`
	PostgresEnabled bool   `json:"postgres_enabled"`
	MySQLEnabled    bool   `json:"mysql_enabled"`
	DataSources     int    `json:"data_sources"`
	UserName        string `json:"user_name"`
	UserEmail       string `json:"user_email"`
	RedashVersion   string `json:"redash_version"`
}

// The setup wizard without a terminal: same validation, same writer.
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file to write (default: ~/.redash-wire/config.yaml)")
	profile := fs.String("profile", "default", "profile name to create")
	redashURL := fs.String("url", "", "Redash base URL (required)")
	username := fs.String("username", config.DefaultUsername, "proxy login username")
	password := fs.String("password", config.DefaultPassword, "proxy login password")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if *redashURL == "" {
		return reportError(failf(codeUsage, "-url is required"), *asJSON)
	}
	if *profile == "" {
		*profile = "default"
	}

	apiKey, cerr := readAPIKey()
	if cerr != nil {
		return reportError(cerr, *asJSON)
	}

	res, _, cerr := resolveConfigPath(*configPath)
	if cerr != nil {
		return reportError(cerr, *asJSON)
	}
	// Never overwrite: the existing file holds credentials.
	if res.Found {
		return reportError(failf(codeConfigExists,
			"config already exists at %s; edit it directly, or pass -config to write elsewhere",
			shortenHome(res.Path)), *asJSON)
	}

	url := strings.TrimRight(*redashURL, "/")
	session, sources, err := setup.ValidateConnection(url, apiKey)
	if err != nil {
		return reportError(classifyAPIError(err), *asJSON)
	}

	hasPostgres := len(redash.FilterByType(sources, redash.IsPostgresCompatible)) > 0
	hasMySQL := len(redash.FilterByType(sources, redash.IsMySQLCompatible)) > 0
	// Matches the wizard: with nothing compatible found, enable both so the
	// written config still validates.
	if !hasPostgres && !hasMySQL {
		hasPostgres, hasMySQL = true, true
	}

	fc := config.NewFileConfig(*profile, url, apiKey, *username, *password, hasPostgres, hasMySQL)
	if err := config.WriteConfig(res.Path, fc); err != nil {
		return reportError(fail(codeIOError, err), *asJSON)
	}

	payload := buildInitPayload(res.Path, *profile, url, session, sources, hasPostgres, hasMySQL)

	if *asJSON {
		if cerr := emitJSON(payload); cerr != nil {
			return reportError(cerr, false)
		}
		return exitOK
	}

	printInit(payload)
	return exitOK
}

func buildInitPayload(path, profile, url string, session *redash.SessionInfo, sources []redash.DataSource, hasPostgres, hasMySQL bool) initPayload {
	payload := initPayload{
		ConfigPath:      path,
		Profile:         profile,
		RedashURL:       url,
		PostgresEnabled: hasPostgres,
		MySQLEnabled:    hasMySQL,
		DataSources:     len(sources),
	}
	if session != nil {
		payload.UserName = session.User.Name
		payload.UserEmail = session.User.Email
		payload.RedashVersion = session.ClientConfig.Version
	}
	return payload
}

// The key comes from stdin so it never reaches argv, which any local process can
// read through `ps`.
func readAPIKey() (string, *cliError) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return "", failf(codeUsage,
			"the API key is read from stdin: pipe it in, e.g. `pbpaste | redash-wire init -url https://redash.example.com`")
	}
	return readAPIKeyFrom(os.Stdin)
}

// Trims whitespace so a trailing newline does not become part of the key.
func readAPIKeyFrom(r io.Reader) (string, *cliError) {
	data, err := io.ReadAll(io.LimitReader(r, maxAPIKeyBytes))
	if err != nil {
		return "", fail(codeIOError, fmt.Errorf("reading API key from stdin: %w", err))
	}

	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", failf(codeUsage, "no API key on stdin")
	}
	return key, nil
}

func printInit(p initPayload) {
	fmt.Printf("wrote %s\n", shortenHome(p.ConfigPath))
	fmt.Printf("profile:      %s\n", p.Profile)
	if p.UserName != "" {
		fmt.Printf("redash user:  %s (%s)\n", p.UserName, p.UserEmail)
	}
	fmt.Printf("data sources: %d\n", p.DataSources)
	fmt.Printf("listeners:    %s\n", strings.Join(enabledListeners(p), ", "))
}

func enabledListeners(p initPayload) []string {
	var out []string
	if p.PostgresEnabled {
		out = append(out, "postgres "+config.DefaultPostgresListenAddr)
	}
	if p.MySQLEnabled {
		out = append(out, "mysql "+config.DefaultMySQLListenAddr)
	}
	return out
}
