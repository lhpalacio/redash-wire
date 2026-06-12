package mysqlwire

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/testutil"
)

func newTestHandler(t *testing.T, sources []redash.DataSource, mock *testutil.MockRedashAPI) *handler {
	t.Helper()
	registry := testutil.NewMockSourceRegistry(sources)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newHandler(context.Background(), logger, mock, registry)
}

func TestUseDB(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "prod_mysql", Type: "mysql"},
		{ID: 2, Name: "prod_pg", Type: "pg"},
	}

	t.Run("valid MySQL source", func(t *testing.T) {
		h := newTestHandler(t, sources, &testutil.MockRedashAPI{})
		err := h.UseDB("prod_mysql")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.dataSourceID != 1 {
			t.Errorf("dataSourceID = %d, want 1", h.dataSourceID)
		}
		if h.dbName != "prod_mysql" {
			t.Errorf("dbName = %q, want %q", h.dbName, "prod_mysql")
		}
	})

	t.Run("non-MySQL source returns error", func(t *testing.T) {
		h := newTestHandler(t, sources, &testutil.MockRedashAPI{})
		err := h.UseDB("prod_pg")
		if err == nil {
			t.Fatal("expected error for non-MySQL source, got nil")
		}
	})

	t.Run("nonexistent source returns error", func(t *testing.T) {
		h := newTestHandler(t, sources, &testutil.MockRedashAPI{})
		err := h.UseDB("nonexistent")
		if err == nil {
			t.Fatal("expected error for nonexistent source, got nil")
		}
	})

	t.Run("UseDB resets schema cache", func(t *testing.T) {
		fetches := 0
		mock := &testutil.MockRedashAPI{
			GetSchemaFunc: func(_ context.Context, _ int) ([]redash.SchemaTable, error) {
				fetches++
				return []redash.SchemaTable{{Name: "t"}}, nil
			},
		}
		h := newTestHandler(t, sources, mock)

		if err := h.UseDB("prod_mysql"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = h.getSchema()
		if fetches != 1 {
			t.Fatalf("expected 1 fetch after priming, got %d", fetches)
		}

		// Re-selecting even the same database must reset the cache so the next getSchema re-fetches.
		if err := h.UseDB("prod_mysql"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = h.getSchema()
		if fetches != 2 {
			t.Errorf("expected re-fetch after UseDB reset, got %d fetches", fetches)
		}
	})

	t.Run("case insensitive lookup", func(t *testing.T) {
		h := newTestHandler(t, sources, &testutil.MockRedashAPI{})
		err := h.UseDB("PROD_MYSQL")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.dataSourceID != 1 {
			t.Errorf("dataSourceID = %d, want 1", h.dataSourceID)
		}
	})
}

func TestHandleQuery_Routing(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "prod_mysql", Type: "mysql"},
		{ID: 2, Name: "staging_mysql", Type: "mysql"},
	}

	t.Run("local query does not call ExecuteQuery", func(t *testing.T) {
		called := false
		mock := &testutil.MockRedashAPI{
			ExecuteQueryFunc: func(_ context.Context, _ string, _ int) (*redash.QueryResult, error) {
				called = true
				return &redash.QueryResult{}, nil
			},
		}
		h := newTestHandler(t, sources, mock)

		_, err := h.HandleQuery("SET NAMES utf8mb4")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Error("ExecuteQuery should not be called for local queries")
		}
	})

	t.Run("remote query with data source calls ExecuteQuery", func(t *testing.T) {
		var capturedSQL string
		var capturedDSID int
		mock := &testutil.MockRedashAPI{
			ExecuteQueryFunc: func(_ context.Context, sql string, dsID int) (*redash.QueryResult, error) {
				capturedSQL = sql
				capturedDSID = dsID
				return &redash.QueryResult{
					Columns: []redash.Column{{Name: "id", Type: "integer"}},
					Rows:    []map[string]any{{"id": 1}},
				}, nil
			},
		}
		h := newTestHandler(t, sources, mock)

		if err := h.UseDB("prod_mysql"); err != nil {
			t.Fatalf("UseDB: %v", err)
		}

		result, err := h.HandleQuery("SELECT * FROM users")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedSQL != "SELECT * FROM users" {
			t.Errorf("ExecuteQuery SQL = %q, want %q", capturedSQL, "SELECT * FROM users")
		}
		if capturedDSID != 1 {
			t.Errorf("ExecuteQuery dataSourceID = %d, want 1", capturedDSID)
		}
		if result == nil || result.Resultset == nil {
			t.Fatal("expected result with Resultset")
		}
	})

	t.Run("remote query without data source returns error", func(t *testing.T) {
		mock := &testutil.MockRedashAPI{}
		h := newTestHandler(t, sources, mock)

		_, err := h.HandleQuery("SELECT * FROM users")
		if err == nil {
			t.Fatal("expected error when no database selected, got nil")
		}
	})

	t.Run("USE command switches database", func(t *testing.T) {
		mock := &testutil.MockRedashAPI{
			ExecuteQueryFunc: func(_ context.Context, _ string, dsID int) (*redash.QueryResult, error) {
				return &redash.QueryResult{
					Columns: []redash.Column{{Name: "n", Type: "integer"}},
					Rows:    []map[string]any{{"n": 1}},
				}, nil
			},
		}
		h := newTestHandler(t, sources, mock)

		_, err := h.HandleQuery("USE prod_mysql")
		if err != nil {
			t.Fatalf("HandleQuery(USE): %v", err)
		}
		if h.dataSourceID != 1 {
			t.Errorf("dataSourceID after USE = %d, want 1", h.dataSourceID)
		}

		_, err = h.HandleQuery("USE staging_mysql")
		if err != nil {
			t.Fatalf("HandleQuery(USE staging): %v", err)
		}
		if h.dataSourceID != 2 {
			t.Errorf("dataSourceID after second USE = %d, want 2", h.dataSourceID)
		}
	})

	t.Run("USE with backticks and semicolons", func(t *testing.T) {
		mock := &testutil.MockRedashAPI{}
		h := newTestHandler(t, sources, mock)

		_, err := h.HandleQuery("USE `prod_mysql`;")
		if err != nil {
			t.Fatalf("HandleQuery(USE with backticks): %v", err)
		}
		if h.dataSourceID != 1 {
			t.Errorf("dataSourceID = %d, want 1", h.dataSourceID)
		}
	})

	t.Run("empty query returns nil", func(t *testing.T) {
		mock := &testutil.MockRedashAPI{}
		h := newTestHandler(t, sources, mock)

		result, err := h.HandleQuery("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for empty query, got %v", result)
		}
	})

	t.Run("whitespace-only query returns nil", func(t *testing.T) {
		mock := &testutil.MockRedashAPI{}
		h := newTestHandler(t, sources, mock)

		result, err := h.HandleQuery("   ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for whitespace query, got %v", result)
		}
	})
}
