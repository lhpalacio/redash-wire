package setup

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
	}{
		{"https://redash.example.com", false},
		{"http://localhost:5000", false},
		{"", true},
		{"redash.example.com", true},
		{"ftp://redash.example.com", true},
		{"https://", true},
	}
	for _, tt := range tests {
		if err := ValidateURL(tt.url); (err != nil) != tt.wantErr {
			t.Errorf("ValidateURL(%q) = %v, wantErr %v", tt.url, err, tt.wantErr)
		}
	}
}

// TestValidateConnection covers the wizard's connection check: a key that
// authenticates and can list data sources passes with both; one that
// authenticates but cannot list data sources (a restricted user) is reported
// as a failure, since the proxy would fail the same way at startup, while the
// session is still returned so the wizard can say who the key belongs to; and
// a key Redash rejects yields nothing.
func TestValidateConnection(t *testing.T) {
	tests := []struct {
		name          string
		sessionStatus int
		sourcesStatus int
		wantErr       bool
		wantSession   bool
		wantSources   int
	}{
		{"key can list data sources", http.StatusOK, http.StatusOK, false, true, 1},
		{"key authenticates but cannot list data sources", http.StatusOK, http.StatusForbidden, true, true, 0},
		{"key is rejected", http.StatusNotFound, http.StatusOK, true, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Key test-key" {
					t.Errorf("Authorization = %q, want %q", got, "Key test-key")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/session":
					w.WriteHeader(tt.sessionStatus)
					fmt.Fprint(w, `{"user":{"name":"Admin","email":"admin@example.com"},"client_config":{"version":"26.3.0"}}`)
				case "/api/data_sources":
					w.WriteHeader(tt.sourcesStatus)
					fmt.Fprint(w, `[{"id":1,"name":"Sample PostgreSQL","type":"pg"}]`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)

			session, sources, err := ValidateConnection(srv.URL, "test-key")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if (session != nil) != tt.wantSession {
				t.Errorf("session = %+v, want present: %v", session, tt.wantSession)
			}
			if tt.wantSession && session.User.Name != "Admin" {
				t.Errorf("session user = %q, want Admin", session.User.Name)
			}
			if len(sources) != tt.wantSources {
				t.Errorf("got %d data sources, want %d", len(sources), tt.wantSources)
			}
		})
	}
}
