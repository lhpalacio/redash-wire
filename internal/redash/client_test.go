package redash

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func checkAuthHeader(t *testing.T, r *http.Request, wantKey string) {
	t.Helper()
	got := r.Header.Get("Authorization")
	want := "Key " + wantKey
	if got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}
}

func TestListDataSources(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantErr   string
		wantCount int
		wantFirst DataSource
	}{
		{
			name: "success with two sources",
			handler: func(w http.ResponseWriter, r *http.Request) {
				checkAuthHeader(t, r, "test-key")
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `[
					{"id":1,"name":"PostgreSQL","type":"pg"},
					{"id":2,"name":"MySQL","type":"mysql"}
				]`)
			},
			wantCount: 2,
			wantFirst: DataSource{ID: 1, Name: "PostgreSQL", Type: "pg"},
		},
		{
			name: "server error 500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				checkAuthHeader(t, r, "test-key")
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: "status 500",
		},
		{
			name: "invalid JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				checkAuthHeader(t, r, "test-key")
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `not json`)
			},
			wantErr: "decoding data sources",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			t.Cleanup(srv.Close)

			c := NewClient(srv.URL, "test-key", WithPollInterval(10*time.Millisecond), WithPollTimeout(50*time.Millisecond))
			sources, err := c.ListDataSources(context.Background())

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
			if len(sources) != tt.wantCount {
				t.Fatalf("got %d sources, want %d", len(sources), tt.wantCount)
			}
			if sources[0] != tt.wantFirst {
				t.Errorf("first source = %+v, want %+v", sources[0], tt.wantFirst)
			}
		})
	}
}

func TestGetSchema(t *testing.T) {
	tests := []struct {
		name        string
		response    string
		wantTable   string
		wantColumns []string
	}{
		{
			name: "MySQL format columns (string array)",
			response: `{
				"schema": [
					{"name":"users","columns":["id","name","email"]}
				]
			}`,
			wantTable:   "users",
			wantColumns: []string{"id", "name", "email"},
		},
		{
			name: "PG format columns (object array)",
			response: `{
				"schema": [
					{"name":"orders","columns":[
						{"name":"id","type":"integer"},
						{"name":"total","type":"numeric"}
					]}
				]
			}`,
			wantTable:   "orders",
			wantColumns: []string{"id", "total"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkAuthHeader(t, r, "test-key")
				if r.URL.Path != "/api/data_sources/42/schema" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tt.response)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(srv.URL, "test-key", WithPollInterval(10*time.Millisecond), WithPollTimeout(50*time.Millisecond))
			tables, err := c.GetSchema(context.Background(), 42)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(tables) != 1 {
				t.Fatalf("got %d tables, want 1", len(tables))
			}
			if tables[0].Name != tt.wantTable {
				t.Errorf("table name = %q, want %q", tables[0].Name, tt.wantTable)
			}
			if len(tables[0].Columns) != len(tt.wantColumns) {
				t.Fatalf("got %d columns, want %d", len(tables[0].Columns), len(tt.wantColumns))
			}
			for i, col := range tables[0].Columns {
				if col != tt.wantColumns[i] {
					t.Errorf("column[%d] = %q, want %q", i, col, tt.wantColumns[i])
				}
			}
		})
	}
}

func TestExecuteQuery_Immediate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuthHeader(t, r, "test-key")

		if r.Method != http.MethodPost || r.URL.Path != "/api/query_results" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}

		var body queryRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.Query != "SELECT 1" {
			t.Errorf("query = %q, want %q", body.Query, "SELECT 1")
		}
		if body.DataSourceID != 7 {
			t.Errorf("data_source_id = %d, want 7", body.DataSourceID)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"query_result": {
				"data": {
					"columns": [
						{"name":"id","friendly_name":"ID","type":"integer"},
						{"name":"value","friendly_name":"Value","type":"string"}
					],
					"rows": [
						{"id": 1, "value": "hello"},
						{"id": 2, "value": "world"}
					]
				}
			}
		}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "test-key", WithPollInterval(10*time.Millisecond), WithPollTimeout(5*time.Second))
	result, err := c.ExecuteQuery(context.Background(), "SELECT 1", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Columns) != 2 {
		t.Fatalf("got %d columns, want 2", len(result.Columns))
	}
	if result.Columns[0].Name != "id" {
		t.Errorf("column[0].Name = %q, want %q", result.Columns[0].Name, "id")
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0]["value"] != "hello" {
		t.Errorf("rows[0][value] = %v, want %q", result.Rows[0]["value"], "hello")
	}
}

func TestExecuteQuery_WithJobPolling(t *testing.T) {
	var pollCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuthHeader(t, r, "test-key")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/query_results":
			fmt.Fprint(w, `{"job":{"id":"job-abc","status":1}}`)

		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs/job-abc":
			n := pollCount.Add(1)
			if n < 3 {
				fmt.Fprintf(w, `{"job":{"id":"job-abc","status":%d}}`, jobStatusStarted)
			} else {
				fmt.Fprint(w, `{"job":{"id":"job-abc","status":3,"query_result_id":99}}`)
			}

		case r.Method == http.MethodGet && r.URL.Path == "/api/query_results/99":
			fmt.Fprint(w, `{
				"query_result": {
					"data": {
						"columns": [{"name":"n","friendly_name":"N","type":"integer"}],
						"rows": [{"n": 42}]
					}
				}
			}`)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "test-key", WithPollInterval(10*time.Millisecond), WithPollTimeout(5*time.Second))
	result, err := c.ExecuteQuery(context.Background(), "SELECT 42", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(result.Rows))
	}
	// json.Number is used because the client calls dec.UseNumber().
	if got, ok := result.Rows[0]["n"].(json.Number); !ok || got.String() != "42" {
		t.Errorf("rows[0][n] = %v (%T), want json.Number 42", result.Rows[0]["n"], result.Rows[0]["n"])
	}

	if got := pollCount.Load(); got < 3 {
		t.Errorf("expected at least 3 job polls, got %d", got)
	}
}

func TestExecuteQuery_JobFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuthHeader(t, r, "test-key")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/query_results":
			fmt.Fprint(w, `{"job":{"id":"job-fail","status":1}}`)

		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs/job-fail":
			fmt.Fprint(w, `{"job":{"id":"job-fail","status":4,"error":"syntax error at position 10"}}`)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "test-key", WithPollInterval(10*time.Millisecond), WithPollTimeout(5*time.Second))
	_, err := c.ExecuteQuery(context.Background(), "INVALID SQL", 1)
	if err == nil {
		t.Fatal("expected error for failed job, got nil")
	}
	if !strings.Contains(err.Error(), "syntax error at position 10") {
		t.Fatalf("error %q does not contain expected message", err)
	}
}

func TestExecuteQuery_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuthHeader(t, r, "test-key")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/query_results":
			fmt.Fprint(w, `{"job":{"id":"job-slow","status":1}}`)

		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs/job-slow":
			// Stays queued forever so the poll times out.
			fmt.Fprint(w, `{"job":{"id":"job-slow","status":1}}`)

		case r.Method == http.MethodDelete && r.URL.Path == "/api/jobs/job-slow":
			// Best-effort cancellation on timeout.
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "test-key", WithPollInterval(10*time.Millisecond), WithPollTimeout(50*time.Millisecond))
	_, err := c.ExecuteQuery(context.Background(), "SELECT pg_sleep(999)", 1)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error %q does not indicate timeout", err)
	}
}

// TestExecuteQuery_TransientPollError verifies a one-off non-fatal job-status
// failure does not abort an otherwise-healthy query.
func TestExecuteQuery_TransientPollError(t *testing.T) {
	var pollCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/query_results":
			fmt.Fprint(w, `{"job":{"id":"job-x","status":1}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs/job-x":
			n := pollCount.Add(1)
			switch {
			case n == 1:
				w.WriteHeader(http.StatusBadGateway)
			case n < 3:
				fmt.Fprintf(w, `{"job":{"id":"job-x","status":%d}}`, jobStatusStarted)
			default:
				fmt.Fprint(w, `{"job":{"id":"job-x","status":3,"query_result_id":5}}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/query_results/5":
			fmt.Fprint(w, `{"query_result":{"data":{"columns":[{"name":"n","type":"integer"}],"rows":[{"n":1}]}}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "test-key", WithPollInterval(5*time.Millisecond), WithPollTimeout(5*time.Second))
	result, err := c.ExecuteQuery(context.Background(), "SELECT 1", 1)
	if err != nil {
		t.Fatalf("transient 502 should not abort the query: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(result.Rows))
	}
}

// TestExecuteQuery_FatalPollError verifies a 404 during polling aborts immediately.
func TestExecuteQuery_FatalPollError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/query_results":
			fmt.Fprint(w, `{"job":{"id":"job-y","status":1}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs/job-y":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "test-key", WithPollInterval(5*time.Millisecond), WithPollTimeout(5*time.Second))
	_, err := c.ExecuteQuery(context.Background(), "SELECT 1", 1)
	if err == nil {
		t.Fatal("expected fatal error on 404, got nil")
	}
}
