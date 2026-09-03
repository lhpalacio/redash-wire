package main

import (
	"bytes"
	"regexp"
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
	if !regexp.MustCompile(`api key:\s+set\b`).MatchString(hidden.String()) {
		t.Errorf("expected the key reported as set:\n%s", hidden.String())
	}

	var shown bytes.Buffer
	printConfig(&shown, buildConfigPayload(res, "explicit", sum, true))
	if !regexp.MustCompile(`api key:\s+` + regexp.QuoteMeta(testAPIKey)).MatchString(shown.String()) {
		t.Errorf("-show-secrets must print the key in text mode:\n%s", shown.String())
	}
	// The broken profile has no key, so there is nothing to show for it.
	if !regexp.MustCompile(`api key:\s+missing\b`).MatchString(shown.String()) {
		t.Errorf("a missing key must still read as missing under -show-secrets:\n%s", shown.String())
	}
}

// The reason a profile is invalid is part of the report, not a log line.
func TestPrintConfigInvalidReasonInReport(t *testing.T) {
	path, sum := writeTestConfig(t)
	res := config.ResolveResult{Path: path, Found: true}

	payload := buildConfigPayload(res, "explicit", sum, false)
	var reason string
	for _, p := range payload.Profiles {
		if !p.Valid {
			reason = p.Error
		}
	}
	if reason == "" {
		t.Fatalf("fixture has no invalid profile with a reason: %+v", payload.Profiles)
	}

	var out bytes.Buffer
	printConfig(&out, payload)
	if !strings.Contains(out.String(), reason) {
		t.Errorf("invalid reason %q missing from the report:\n%s", reason, out.String())
	}
}
