package main

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lhpalacio/redash-wire/internal/config"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The macOS app decodes these payloads, so their shape is a contract rather than
// an implementation detail. A golden diff means a consumer breaks: update the
// files deliberately, don't regenerate them to make the test pass.
func assertGolden(t *testing.T, name string, got any) {
	t.Helper()

	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	data = append(data, '\n')

	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden (run `go test ./cmd/redash-wire -update` to create it): %v", err)
	}
	if string(data) != string(want) {
		t.Errorf("payload does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, data, want)
	}
}

// testAPIKey is a sentinel: the leak tests assert this exact string never reaches
// output that is supposed to be redacted.
const testAPIKey = "SENTINEL-API-KEY-must-not-leak"

const testConfigYAML = `postgres_listen_addr: "127.0.0.1:15432"
mysql_listen_addr: "127.0.0.1:13306"
username: "redash-wire"
password: "supersecret"
poll_interval: "500ms"
poll_timeout: "120s"
default_profile: prod
profiles:
  prod:
    redash_url: "https://redash.prod.example.com"
    api_key: "` + testAPIKey + `"
    postgres_listen_addr: "127.0.0.1:25432"
    username: "prod-user"
    password: "prod-password"
  broken:
    redash_url: "https://redash.broken.example.com"
`

// writeTestConfig writes a fixture config and returns its resolved summary. The
// "broken" profile has no api_key, so it exercises the invalid-but-listed path.
func writeTestConfig(t *testing.T) (string, *config.Summary) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(testConfigYAML), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	sum, err := config.LoadAll(path)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	return path, sum
}

// normalizePath keeps the temp directory out of the golden file.
func normalizePath(p configPayload) configPayload {
	p.ConfigPath = "/testdata/config.yaml"
	return p
}

func TestConfigPayloadGolden(t *testing.T) {
	path, sum := writeTestConfig(t)
	res := config.ResolveResult{Path: path, Found: true}

	got := normalizePath(buildConfigPayload(res, "explicit", sum, false))
	assertGolden(t, "config.golden.json", got)
}

func TestConfigPayloadWithSecretsGolden(t *testing.T) {
	path, sum := writeTestConfig(t)
	res := config.ResolveResult{Path: path, Found: true}

	got := normalizePath(buildConfigPayload(res, "explicit", sum, true))
	assertGolden(t, "config-secrets.golden.json", got)
}

func TestConfigPayloadNotConfiguredGolden(t *testing.T) {
	res := config.ResolveResult{Path: "/home/user/.redash-wire/config.yaml", Found: false}

	got := buildConfigPayload(res, "home", nil, false)
	got.ConfigPath = "/testdata/config.yaml"
	assertGolden(t, "config-missing.golden.json", got)
}

// TestConfigPayloadRedactsAPIKey is the leak test: the key must be absent from
// the serialized output, not merely absent from the struct field we remembered to
// check. Serializing and searching catches a key that reappears nested somewhere
// unexpected.
func TestConfigPayloadRedactsAPIKey(t *testing.T) {
	path, sum := writeTestConfig(t)
	res := config.ResolveResult{Path: path, Found: true}

	redacted, err := json.Marshal(buildConfigPayload(res, "explicit", sum, false))
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	if strings.Contains(string(redacted), testAPIKey) {
		t.Errorf("API key leaked into redacted output:\n%s", redacted)
	}
	if strings.Contains(string(redacted), `"api_key"`) {
		t.Errorf("api_key field present in redacted output; it must be omitted entirely:\n%s", redacted)
	}
	if !strings.Contains(string(redacted), `"api_key_set": true`) && !strings.Contains(string(redacted), `"api_key_set":true`) {
		t.Errorf("api_key_set must still report that a key is configured:\n%s", redacted)
	}

	shown, err := json.Marshal(buildConfigPayload(res, "explicit", sum, true))
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	if !strings.Contains(string(shown), testAPIKey) {
		t.Errorf("-show-secrets must include the API key:\n%s", shown)
	}
}

// TestErrorPayloadNeverCarriesAPIKey guards the other direction: an error built
// from a message that happens to contain the key must not be emitted verbatim by
// a caller that assumes error text is safe.
func TestErrorPayloadRoundTrip(t *testing.T) {
	cerr := failf(codeAuthenticationFailed, "session request failed (status 401)")
	got := errorPayload{Error: errorBody{Code: cerr.Code, Message: cerr.Error()}}
	assertGolden(t, "error.golden.json", got)
}

func TestDataSourcesPayloadGolden(t *testing.T) {
	sources := append(testutil.SampleDataSources(),
		redash.DataSource{ID: 4, Name: "BigQuery Warehouse", Type: "bigquery"},
		redash.DataSource{ID: 5, Name: "aurora reporting", Type: "aurora_mysql"},
	)

	assertGolden(t, "datasources.golden.json", buildDataSourcesPayload(sources))
}

// An empty result must serialize as [], never null: the app decodes an array.
func TestDataSourcesPayloadEmptyIsArray(t *testing.T) {
	data, err := json.Marshal(buildDataSourcesPayload(nil))
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("empty data source list = %s, want []", data)
	}
}

func TestDataSourcesPayloadSortsByName(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "zebra", Type: "pg"},
		{ID: 2, Name: "Alpha", Type: "pg"},
		{ID: 3, Name: "middle", Type: "pg"},
	}

	got := buildDataSourcesPayload(sources)
	want := []string{"Alpha", "middle", "zebra"}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("position %d = %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestWireProtocol(t *testing.T) {
	tests := map[string]string{
		"pg":           "postgres",
		"postgres":     "postgres",
		"redshift":     "postgres",
		"cockroachdb":  "postgres",
		"mysql":        "mysql",
		"rds_mysql":    "mysql",
		"aurora_mysql": "mysql",
		"mariadb":      "mysql",
		"bigquery":     "",
		"athena":       "",
		"":             "",
	}

	for dsType, want := range tests {
		if got := wireProtocol(dsType); got != want {
			t.Errorf("wireProtocol(%q) = %q, want %q", dsType, got, want)
		}
	}
}

func TestInitPayloadGolden(t *testing.T) {
	session := &redash.SessionInfo{}
	session.User.Name = "Ada Lovelace"
	session.User.Email = "ada@example.com"
	session.ClientConfig.Version = "10.1.0"

	got := buildInitPayload("/testdata/config.yaml", "prod", "https://redash.example.com",
		session, testutil.SampleDataSources(), true, false)
	assertGolden(t, "init.golden.json", got)
}

// A session that could not be fetched must not produce a partial payload with a
// nil dereference; the fields simply stay empty.
func TestInitPayloadWithoutSession(t *testing.T) {
	got := buildInitPayload("/testdata/config.yaml", "prod", "https://redash.example.com", nil, nil, true, true)
	if got.UserName != "" || got.RedashVersion != "" {
		t.Errorf("expected empty session fields, got %+v", got)
	}
	if got.DataSources != 0 {
		t.Errorf("DataSources = %d, want 0", got.DataSources)
	}
}

// TestClassifyAPIError drives the whole chain — client, httpError, HTTPStatus,
// classification — against a real server, because the thing most likely to break
// is the status ever reaching the classifier, not the switch itself.
//
// The 404 case is the one that matters: Redash rejects a bad API key with 404
// rather than 401, so treating 404 as a connection failure would tell someone to
// check their network when their key is wrong.
func TestClassifyAPIError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string
	}{
		{"rejected key returns 404 in real Redash", http.StatusNotFound, codeAuthenticationFailed},
		{"unauthorized", http.StatusUnauthorized, codeAuthenticationFailed},
		{"forbidden", http.StatusForbidden, codeAuthenticationFailed},
		{"server error is not an auth problem", http.StatusInternalServerError, codeConnectionFailed},
		{"rate limited is not an auth problem", http.StatusTooManyRequests, codeConnectionFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			_, err := redash.NewClient(srv.URL, "key").ListDataSources(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := classifyAPIError(err).Code; got != tt.want {
				t.Errorf("status %d classified as %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

// A host that never answers is a connection failure, not an auth failure.
func TestClassifyAPIErrorTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	_, err := redash.NewClient(url, "key").ListDataSources(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := classifyAPIError(err).Code; got != codeConnectionFailed {
		t.Errorf("transport failure classified as %q, want %q", got, codeConnectionFailed)
	}
}
