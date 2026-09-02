package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	ReadOnly        bool   `json:"read_only"`
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
	readOnly := fs.Bool("read-only", false, "write read_only: true, so the proxy refuses writes on this profile")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if *redashURL == "" {
		return reportError(failf(codeUsage, "-url is required"), *asJSON)
	}
	url, cerr := initURL(*redashURL)
	if cerr != nil {
		return reportError(cerr, *asJSON)
	}
	if *profile == "" {
		*profile = "default"
	}

	apiKey, cerr := readAPIKey()
	if cerr != nil {
		return reportError(cerr, *asJSON)
	}

	path, cerr := initTarget(*configPath)
	if cerr != nil {
		return reportError(cerr, *asJSON)
	}

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

	fc := config.NewFileConfig(*profile, url, apiKey, *username, *password, hasPostgres, hasMySQL, *readOnly)
	if err := config.WriteConfig(path, fc); err != nil {
		// A file that appeared since initTarget looked, e.g. a second setup
		// racing this one.
		if errors.Is(err, config.ErrExists) {
			return reportError(configExistsError(path), *asJSON)
		}
		return reportError(fail(codeIOError, err), *asJSON)
	}

	payload := buildInitPayload(path, *profile, url, session, sources, hasPostgres, hasMySQL, *readOnly)

	if *asJSON {
		if cerr := emitJSON(payload); cerr != nil {
			return reportError(cerr, false)
		}
		return exitOK
	}

	printInit(payload)
	return exitOK
}

// initURL normalizes -url the way the wizard does and rejects what the wizard
// would, so a bare host is a usage mistake rather than a failed connection.
func initURL(raw string) (string, *cliError) {
	url := strings.TrimRight(raw, "/")
	if err := setup.ValidateURL(url); err != nil {
		return "", failf(codeUsage, "invalid -url %q: %w", raw, err)
	}
	return url, nil
}

// initTarget picks the file to write. Without -config that is the documented
// default, never ./config.yaml: an unrelated file in the working directory must
// not block a first setup. The path has to be free, since an existing file
// holds credentials.
func initTarget(flagPath string) (string, *cliError) {
	path := flagPath
	if path == "" {
		var err error
		if path, err = config.DefaultConfigPath(); err != nil {
			return "", fail(codeIOError, err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fail(codeIOError, err)
	}

	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return abs, nil
	case err != nil:
		return "", fail(codeIOError, err)
	case info.IsDir():
		return "", failf(codeIOError, "%s is a directory, not a config file", shortenHome(abs))
	default:
		return "", configExistsError(abs)
	}
}

func configExistsError(path string) *cliError {
	return failf(codeConfigExists,
		"config already exists at %s; edit it directly, or pass -config to write elsewhere",
		shortenHome(path))
}

func buildInitPayload(path, profile, url string, session *redash.SessionInfo, sources []redash.DataSource, hasPostgres, hasMySQL, readOnly bool) initPayload {
	payload := initPayload{
		ConfigPath:      path,
		Profile:         profile,
		RedashURL:       url,
		PostgresEnabled: hasPostgres,
		MySQLEnabled:    hasMySQL,
		ReadOnly:        readOnly,
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
	if p.ReadOnly {
		fmt.Printf("mode:         read-only\n")
	}
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
