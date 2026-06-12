package pgwire

import (
	"bytes"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

func TestHandleCatalogQuery(t *testing.T) {
	schema := []redash.SchemaTable{
		{Name: "users", Columns: []string{"id", "name"}},
		{Name: "orders", Columns: []string{"id", "total"}},
	}
	sources := []redash.DataSource{
		{ID: 1, Name: "mydb", Type: "pg"},
	}

	tests := []struct {
		name         string
		sql          string
		wantDataRows int
	}{
		{
			name:         "pg_database returns sources",
			sql:          "SELECT datname FROM pg_database",
			wantDataRows: 1,
		},
		{
			name:         "pg_class returns tables",
			sql:          "SELECT oid, relname FROM pg_class WHERE relkind='r'",
			wantDataRows: 2,
		},
		{
			name:         "pg_namespace returns public",
			sql:          "SELECT nspname FROM pg_namespace",
			wantDataRows: 1,
		},
		{
			name:         "pg_type returns empty",
			sql:          "SELECT oid, typname FROM pg_type",
			wantDataRows: 0,
		},
		{
			name:         "pg_proc returns empty",
			sql:          "SELECT oid, proname FROM pg_proc",
			wantDataRows: 0,
		},
		{
			name:         "pg_statio_user_tables returns tables",
			sql:          "SELECT * FROM pg_statio_user_tables",
			wantDataRows: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			if err := HandleCatalogQuery(&buf, tt.sql, schema, sources); err != nil {
				t.Fatalf("HandleCatalogQuery(%q): %v", tt.sql, err)
			}

			fe := pgproto3.NewFrontend(&buf, nil)

			var (
				dataRowCount int
				gotRD        bool
				gotCC        bool
				gotRFQ       bool
			)

			for {
				msg, err := fe.Receive()
				if err != nil {
					break
				}
				switch msg.(type) {
				case *pgproto3.RowDescription:
					gotRD = true
				case *pgproto3.DataRow:
					dataRowCount++
				case *pgproto3.CommandComplete:
					gotCC = true
				case *pgproto3.ReadyForQuery:
					gotRFQ = true
				}
			}

			if !gotRD {
				t.Error("missing RowDescription")
			}
			if !gotCC {
				t.Error("missing CommandComplete")
			}
			if !gotRFQ {
				t.Error("missing ReadyForQuery")
			}
			if dataRowCount != tt.wantDataRows {
				t.Errorf("DataRow count = %d, want %d", dataRowCount, tt.wantDataRows)
			}
		})
	}
}

func collectResult(t *testing.T, buf *bytes.Buffer) (cols []string, rows [][]string) {
	t.Helper()
	fe := pgproto3.NewFrontend(buf, nil)
	for {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		switch m := msg.(type) {
		case *pgproto3.RowDescription:
			for _, f := range m.Fields {
				cols = append(cols, string(f.Name))
			}
		case *pgproto3.DataRow:
			row := make([]string, len(m.Values))
			for i, v := range m.Values {
				row[i] = string(v)
			}
			rows = append(rows, row)
		}
	}
	return cols, rows
}

func TestHandleCatalogQuery_InformationSchema(t *testing.T) {
	schema := []redash.SchemaTable{
		{Name: "users", Columns: []string{"id", "name"}},
		{Name: "orders", Columns: []string{"id", "total"}},
	}

	t.Run("information_schema.tables returns requested columns", func(t *testing.T) {
		var buf bytes.Buffer
		sql := "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'"
		if err := HandleCatalogQuery(&buf, sql, schema, nil); err != nil {
			t.Fatal(err)
		}
		cols, rows := collectResult(t, &buf)
		if len(cols) != 1 || cols[0] != "table_name" {
			t.Fatalf("columns = %v, want [table_name]", cols)
		}
		if len(rows) != 2 || rows[0][0] != "users" || rows[1][0] != "orders" {
			t.Errorf("rows = %v, want users/orders", rows)
		}
	})

	t.Run("function call with comma in select list keeps headers intact", func(t *testing.T) {
		var buf bytes.Buffer
		// Mirrors psql \d-style introspection: a comma inside format_type(...) must not
		// split the select list and corrupt the column headers.
		sql := "SELECT attname, format_type(atttypid, atttypmod) AS data_type FROM pg_attribute WHERE table_name = 'users'"
		if err := HandleCatalogQuery(&buf, sql, schema, nil); err != nil {
			t.Fatal(err)
		}
		cols, rows := collectResult(t, &buf)
		if len(cols) != 2 || cols[0] != "attname" || cols[1] != "data_type" {
			t.Fatalf("columns = %v, want [attname data_type]", cols)
		}
		if len(rows) != 2 || rows[0][0] != "id" || rows[1][0] != "name" {
			t.Errorf("rows = %v, want id/name attnames", rows)
		}
	})

	t.Run("information_schema.columns filtered by table_name", func(t *testing.T) {
		var buf bytes.Buffer
		sql := "SELECT column_name, ordinal_position FROM information_schema.columns WHERE table_name = 'users'"
		if err := HandleCatalogQuery(&buf, sql, schema, nil); err != nil {
			t.Fatal(err)
		}
		cols, rows := collectResult(t, &buf)
		if len(cols) != 2 || cols[0] != "column_name" || cols[1] != "ordinal_position" {
			t.Fatalf("columns = %v", cols)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %v, want 2 columns of users", rows)
		}
		if rows[0][0] != "id" || rows[0][1] != "1" || rows[1][0] != "name" || rows[1][1] != "2" {
			t.Errorf("rows = %v, want id/1 name/2", rows)
		}
	})
}

// TestHandleCatalogQuery_KindFilters: a query whose relkind/table_type filter
// cannot match an ordinary table must return zero rows, not the table list, or
// GUI clients show every table twice in the sidebar. The TablePlus cases are
// verbatim queries captured from its session.
func TestHandleCatalogQuery_KindFilters(t *testing.T) {
	schema := []redash.SchemaTable{
		{Name: "users", Columns: []string{"id", "name"}},
		{Name: "orders", Columns: []string{"id", "total"}},
	}

	tests := []struct {
		name     string
		sql      string
		wantRows int
	}{
		{
			name:     "TablePlus materialized views",
			sql:      "SELECT c.relname AS table_name, n.nspname AS table_schema, 'MATERIALIZED VIEW' as table_type FROM pg_catalog.pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = 'm';",
			wantRows: 0,
		},
		{
			name:     "TablePlus tables and views",
			sql:      "SELECT p.oid AS oid, p.relname AS table_name, n.nspname as table_schema FROM pg_class AS p JOIN pg_namespace AS n ON p.relnamespace=n.oid WHERE p.relkind='r' OR p.relkind='v' OR p.relkind='p';",
			wantRows: 2,
		},
		{
			name:     "psql dt relkind IN",
			sql:      "SELECT c.relname FROM pg_catalog.pg_class c WHERE c.relkind IN ('r','p','') AND pg_catalog.pg_table_is_visible(c.oid)",
			wantRows: 2,
		},
		{
			name:     "psql dv views only IN",
			sql:      "SELECT c.relname FROM pg_catalog.pg_class c WHERE c.relkind IN ('v','')",
			wantRows: 0,
		},
		{
			name:     "sequences only",
			sql:      "SELECT relname FROM pg_class WHERE relkind = 'S'",
			wantRows: 0,
		},
		{
			name:     "relkind ANY array",
			sql:      "SELECT relname FROM pg_class WHERE relkind = ANY (ARRAY['v','m'])",
			wantRows: 0,
		},
		{
			name:     "relkind ANY array literal includes tables",
			sql:      "SELECT relname FROM pg_class WHERE relkind = ANY ('{r,v}')",
			wantRows: 2,
		},
		{
			name:     "DBeaver-style NOT IN keeps tables",
			sql:      "SELECT c.relname FROM pg_class c WHERE c.relkind NOT IN ('i','I','c')",
			wantRows: 2,
		},
		{
			name:     "negated equality on tables",
			sql:      "SELECT relname FROM pg_class WHERE relkind <> 'r'",
			wantRows: 0,
		},
		{
			name:     "relkind compared to a column is not a filter",
			sql:      "SELECT c.relname FROM pg_class c JOIN pg_class p ON c.relkind = p.relkind",
			wantRows: 2,
		},
		{
			name:     "relkind inside a string literal is not a filter",
			sql:      "SELECT relname FROM pg_class WHERE obj_description(oid) = 'relkind = ''m'''",
			wantRows: 2,
		},
		{
			name:     "information_schema views only",
			sql:      "SELECT table_name FROM information_schema.tables WHERE table_type = 'VIEW'",
			wantRows: 0,
		},
		{
			name:     "information_schema base tables",
			sql:      "SELECT table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE'",
			wantRows: 2,
		},
		{
			name:     "information_schema tables and views IN",
			sql:      "SELECT table_name FROM information_schema.tables WHERE table_type IN ('BASE TABLE', 'VIEW')",
			wantRows: 2,
		},
		{
			name:     "information_schema no filter",
			sql:      "SELECT table_name, table_schema, table_type FROM information_schema.tables",
			wantRows: 2,
		},
		{
			name:     "row-count listing for matviews only",
			sql:      "SELECT relname, reltuples FROM pg_class WHERE relkind = 'm'",
			wantRows: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := HandleCatalogQuery(&buf, tt.sql, schema, nil); err != nil {
				t.Fatalf("HandleCatalogQuery(%q): %v", tt.sql, err)
			}
			_, rows := collectResult(t, &buf)
			if len(rows) != tt.wantRows {
				t.Errorf("rows = %d, want %d (%v)", len(rows), tt.wantRows, rows)
			}
		})
	}
}

// TestHandleLocalQuery_PgDatabaseWhitespace guards the classifier/dispatch
// agreement: a pg_database query separated by a newline (not a literal space) must
// still return the data-source list, not an empty result.
func TestHandleLocalQuery_PgDatabaseWhitespace(t *testing.T) {
	sources := []redash.DataSource{
		{ID: 1, Name: "a", Type: "pg"},
		{ID: 2, Name: "b", Type: "pg"},
	}
	var buf bytes.Buffer
	if err := HandleLocalQuery(&buf, "SELECT datname FROM\npg_database", map[string]string{}, sources, "127.0.0.1:5432"); err != nil {
		t.Fatal(err)
	}
	_, rows := collectResult(t, &buf)
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2 data sources", len(rows))
	}
}
