package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lhpalacio/redash-wire/internal/config"
)

// -show-secrets has to mean the same thing in both output modes.
func TestPrintConfigShowSecrets(t *testing.T) {
	path, sum := writeTestConfig(t)
	res := config.ResolveResult{Path: path, Found: true}

	var hidden bytes.Buffer
	printConfig(&hidden, buildConfigPayload(res, "explicit", sum, false))
	if strings.Contains(hidden.String(), testAPIKey) {
		t.Errorf("API key printed without -show-secrets:\n%s", hidden.String())
	}
	if !strings.Contains(hidden.String(), "api key:     set") {
		t.Errorf("expected the key reported as set:\n%s", hidden.String())
	}

	var shown bytes.Buffer
	printConfig(&shown, buildConfigPayload(res, "explicit", sum, true))
	if !strings.Contains(shown.String(), "api key:     "+testAPIKey) {
		t.Errorf("-show-secrets must print the key in text mode:\n%s", shown.String())
	}
	// The broken profile has no key, so there is nothing to show for it.
	if !strings.Contains(shown.String(), "api key:     missing") {
		t.Errorf("a missing key must still read as missing under -show-secrets:\n%s", shown.String())
	}
}

// The reason a profile is invalid is part of the report, not a log line.
func TestPrintConfigInvalidReasonInReport(t *testing.T) {
	path, sum := writeTestConfig(t)
	res := config.ResolveResult{Path: path, Found: true}

	var out bytes.Buffer
	printConfig(&out, buildConfigPayload(res, "explicit", sum, false))
	if !strings.Contains(out.String(), "invalid:     api_key is required") {
		t.Errorf("invalid reason missing from the report:\n%s", out.String())
	}
}
