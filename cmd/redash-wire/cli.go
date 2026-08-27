package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/lhpalacio/redash-wire/internal/config"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

// Stable contract: adding a code is safe, renaming or repurposing one is not.
const (
	codeUsage                = "usage"
	codeNotConfigured        = "not_configured"
	codeInvalidConfig        = "invalid_config"
	codeProfileNotFound      = "profile_not_found"
	codeConnectionFailed     = "connection_failed"
	codeAuthenticationFailed = "authentication_failed"
	codeConfigExists         = "config_exists"
	codeIOError              = "io_error"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

type cliError struct {
	Code string
	Err  error
}

func (e *cliError) Error() string { return e.Err.Error() }
func (e *cliError) Unwrap() error { return e.Err }

func fail(code string, err error) *cliError { return &cliError{Code: code, Err: err} }

func failf(code, format string, args ...any) *cliError {
	return &cliError{Code: code, Err: fmt.Errorf(format, args...)}
}

type errorPayload struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// stdout carries command results and nothing else, and stays empty while
// serving. The log stream always goes to stderr.
func emitJSON(v any) *cliError {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(codeIOError, err)
	}
	return nil
}

// In JSON mode the error goes to stdout, so a caller reads one stream whatever
// the outcome.
func reportError(cerr *cliError, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(errorPayload{Error: errorBody{Code: cerr.Code, Message: cerr.Error()}})
	} else {
		fmt.Fprintln(os.Stderr, "error:", cerr.Error())
	}
	if cerr.Code == codeUsage {
		return exitUsage
	}
	return exitError
}

func dispatch(name string, args []string) int {
	switch name {
	case "config":
		return cmdConfig(args)
	case "datasources":
		return cmdDataSources(args)
	case "init":
		return cmdInit(args)
	case "help":
		printUsage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", name)
		printUsage(os.Stderr)
		return exitUsage
	}
}

func printUsage(w *os.File) {
	fmt.Fprint(w, strings.TrimLeft(`
redash-wire — query Redash from any PostgreSQL or MySQL client

Usage:
  redash-wire [flags]                    start the proxy
  redash-wire config [flags]             show the resolved configuration
  redash-wire datasources [flags]        list the Redash data sources
  redash-wire init -url <url> [flags]    write a config (API key on stdin)
  redash-wire help                       show this help

Serve flags:
  -config <path>    config file to use
  -profile <name>   profile to load
  -debug            enable debug logging
  -wait-for-redash  bind and wait when Redash is unreachable, instead of exiting
  -version          print version and exit

Run "redash-wire <command> -h" for a command's flags.
`, "\n"))
}

func resolveConfigPath(flagPath string) (config.ResolveResult, string, *cliError) {
	explicit := flagPath != ""
	path := flagPath
	if !explicit {
		path = "config.yaml"
	}

	res, err := config.Resolve(path, explicit)
	if err != nil {
		return res, "", fail(codeIOError, err)
	}

	source := "home"
	switch {
	case explicit:
		source = "explicit"
	case res.FromCwd:
		source = "cwd"
	}
	return res, source, nil
}

func loadProfile(flagPath, profile string) (*config.Config, *cliError) {
	res, _, cerr := resolveConfigPath(flagPath)
	if cerr != nil {
		return nil, cerr
	}
	if !res.Found {
		return nil, failf(codeNotConfigured, "no config file found at %s", shortenHome(res.Path))
	}

	cfg, err := config.Load(res.Path, profile)
	if err != nil {
		var notFound *config.ProfileNotFoundError
		if errors.As(err, &notFound) {
			return nil, fail(codeProfileNotFound, err)
		}
		return nil, fail(codeInvalidConfig, err)
	}
	return cfg, nil
}

// Redash answers a rejected API key with 404, not 401. A 404 is ambiguous, since
// a URL pointing at some other web server looks the same, so the message names
// both causes. Any HTTP response means the connection worked.
func classifyAPIError(err error) *cliError {
	status, ok := redash.HTTPStatus(err)
	if !ok {
		return fail(codeConnectionFailed, err)
	}

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return failf(codeAuthenticationFailed,
			"%w (check the API key, and that the URL points at a Redash instance)", err)
	default:
		return fail(codeConnectionFailed, err)
	}
}

// Asking for help with -h is not a usage mistake, so it exits 0.
func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return exitOK, true
	case errors.Is(err, flag.ErrHelp):
		return exitOK, false
	default:
		return exitUsage, false
	}
}
