package pgwire

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/lhpalacio/redash-wire/internal/redash"
)

func TestHandleCatalogQuery(t *testing.T) {
	schema := []redash.SchemaTable{
		{Name: "users", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "name"}}},
		{Name: "orders", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "total"}}},
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
		{Name: "users", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "name"}}},
		{Name: "orders", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "total"}}},
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
		{Name: "users", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "name"}}},
		{Name: "orders", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "total"}}},
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
	if err := HandleLocalQuery(&buf, "SELECT datname FROM\npg_database", LocalSession{StartupParams: map[string]string{}, Sources: sources, ListenAddr: "127.0.0.1:5432"}); err != nil {
		t.Fatal(err)
	}
	_, rows := collectResult(t, &buf)
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2 data sources", len(rows))
	}
}

// Queries psql 16-18 send for \d, \dt and \d users (src/bin/psql/describe.c:
// listTables and describeOneTableDetails), captured verbatim from psql against
// the proxy. psql reads the results by column index, so the number and order of
// columns must match what it asked for.
const (
	psqlListRelations = `SELECT n.nspname as "Schema",
  c.relname as "Name",
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'i' THEN 'index' WHEN 'S' THEN 'sequence' WHEN 't' THEN 'TOAST table' WHEN 'f' THEN 'foreign table' WHEN 'p' THEN 'partitioned table' WHEN 'I' THEN 'partitioned index' END as "Type",
  pg_catalog.pg_get_userbyid(c.relowner) as "Owner"
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam
WHERE c.relkind IN ('r','p','v','m','S','f','')
      AND n.nspname <> 'pg_catalog'
      AND n.nspname !~ '^pg_toast'
      AND n.nspname <> 'information_schema'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1,2;`

	psqlListTables = `SELECT n.nspname as "Schema",
  c.relname as "Name",
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'i' THEN 'index' WHEN 'S' THEN 'sequence' WHEN 't' THEN 'TOAST table' WHEN 'f' THEN 'foreign table' WHEN 'p' THEN 'partitioned table' WHEN 'I' THEN 'partitioned index' END as "Type",
  pg_catalog.pg_get_userbyid(c.relowner) as "Owner"
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam
WHERE c.relkind IN ('r','p','')
      AND n.nspname <> 'pg_catalog'
      AND n.nspname !~ '^pg_toast'
      AND n.nspname <> 'information_schema'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1,2;`

	// \d users, step 1: resolve the name pattern to an oid.
	psqlDescribeLookup = `SELECT c.oid,
  n.nspname,
  c.relname
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname OPERATOR(pg_catalog.~) '^(users)$' COLLATE pg_catalog.default
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 2, 3;`

	// \d users, step 2: the relation's flags, read as PQgetvalue(res, 0, 0..14).
	psqlDescribeTable = `SELECT c.relchecks, c.relkind, c.relhasindex, c.relhasrules, c.relhastriggers, c.relrowsecurity, c.relforcerowsecurity, false AS relhasoids, c.relispartition, '', c.reltablespace, CASE WHEN c.reloftype = 0 THEN '' ELSE c.reloftype::pg_catalog.regtype::pg_catalog.text END, c.relpersistence, c.relreplident, am.amname
FROM pg_catalog.pg_class c
 LEFT JOIN pg_catalog.pg_class tc ON (c.reltoastrelid = tc.oid)
LEFT JOIN pg_catalog.pg_am am ON (c.relam = am.oid)
WHERE c.oid = '16384';`

	// \d users, step 3: the columns, with scalar subqueries in the select list.
	psqlDescribeColumns = `SELECT a.attname,
  pg_catalog.format_type(a.atttypid, a.atttypmod),
  (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid, true)
   FROM pg_catalog.pg_attrdef d
   WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef),
  a.attnotnull,
  (SELECT c.collname FROM pg_catalog.pg_collation c, pg_catalog.pg_type t
   WHERE c.oid = a.attcollation AND t.oid = a.atttypid AND a.attcollation <> t.typcollation) AS attcollation,
  a.attidentity,
  a.attgenerated
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = '16384' AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum;`

	// \d+ users: the verbose flags query carries the reloptions expression (a
	// FROM inside parentheses and a comma inside a literal) and the column query
	// four more items, read at the indices psql assigned while building it.
	psqlDescribeTableVerbose = `SELECT c.relchecks, c.relkind, c.relhasindex, c.relhasrules, c.relhastriggers, c.relrowsecurity, c.relforcerowsecurity, false AS relhasoids, c.relispartition, pg_catalog.array_to_string(c.reloptions || array(select 'toast.' || x from pg_catalog.unnest(tc.reloptions) x), ', ')
, c.reltablespace, CASE WHEN c.reloftype = 0 THEN '' ELSE c.reloftype::pg_catalog.regtype::pg_catalog.text END, c.relpersistence, c.relreplident, am.amname
FROM pg_catalog.pg_class c
 LEFT JOIN pg_catalog.pg_class tc ON (c.reltoastrelid = tc.oid)
LEFT JOIN pg_catalog.pg_am am ON (c.relam = am.oid)
WHERE c.oid = '16384';`

	psqlDescribeColumnsVerbose = `SELECT a.attname,
  pg_catalog.format_type(a.atttypid, a.atttypmod),
  (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid, true)
   FROM pg_catalog.pg_attrdef d
   WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef),
  a.attnotnull,
  (SELECT c.collname FROM pg_catalog.pg_collation c, pg_catalog.pg_type t
   WHERE c.oid = a.attcollation AND t.oid = a.atttypid AND a.attcollation <> t.typcollation) AS attcollation,
  a.attidentity,
  a.attgenerated,
  a.attstorage,
  a.attcompression AS attcompression,
  CASE WHEN a.attstattarget=-1 THEN NULL ELSE a.attstattarget END AS attstattarget,
  pg_catalog.col_description(a.attrelid, a.attnum)
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = '16384' AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum;`

	// \d users, later: parents and children through pg_inherits. The proxy has
	// no inheritance, so an inner join against it yields nothing; answering with
	// the table list printed "Inherits: users, orders" under every table.
	psqlDescribeParents = `SELECT c.oid::pg_catalog.regclass
FROM pg_catalog.pg_class c, pg_catalog.pg_inherits i
WHERE c.oid = i.inhparent AND i.inhrelid = '16384'
  AND c.relkind != 'p' AND c.relkind != 'I'
ORDER BY inhseqno;`

	psqlDescribeChildren = `SELECT c.oid::pg_catalog.regclass, c.relkind, inhdetachpending, pg_catalog.pg_get_expr(c.relpartbound, c.oid)
FROM pg_catalog.pg_class c, pg_catalog.pg_inherits i
WHERE c.oid = i.inhrelid AND i.inhparent = '16384'
ORDER BY pg_catalog.pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT', c.oid::pg_catalog.regclass::pg_catalog.text;`
)

func TestHandleCatalogQuery_PsqlDescribe(t *testing.T) {
	schema := []redash.SchemaTable{
		{Name: "users", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "name"}}},
		{Name: "orders", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "total"}}},
	}

	tests := []struct {
		name     string
		sql      string
		wantCols []string
		wantRows [][]string
	}{
		{
			name:     "backslash d lists every table",
			sql:      psqlListRelations,
			wantCols: []string{"Schema", "Name", "Type", "Owner"},
			wantRows: [][]string{{"public", "users", "table", "redash"}, {"public", "orders", "table", "redash"}},
		},
		{
			name:     "backslash dt lists every table",
			sql:      psqlListTables,
			wantCols: []string{"Schema", "Name", "Type", "Owner"},
			wantRows: [][]string{{"public", "users", "table", "redash"}, {"public", "orders", "table", "redash"}},
		},
		{
			name:     "backslash d users resolves one oid",
			sql:      psqlDescribeLookup,
			wantCols: []string{"oid", "nspname", "relname"},
			wantRows: [][]string{{"16384", "public", "users"}},
		},
		{
			name:     "backslash d users reads the flags by index",
			sql:      psqlDescribeTable,
			wantCols: []string{"relchecks", "relkind", "relhasindex", "relhasrules", "relhastriggers", "relrowsecurity", "relforcerowsecurity", "relhasoids", "relispartition", "?column?", "reltablespace", "case", "relpersistence", "relreplident", "amname"},
			wantRows: [][]string{{"0", "r", "f", "f", "f", "f", "f", "f", "f", "", "0", "", "p", "d", "heap"}},
		},
		{
			name:     "backslash d users lists only that table's columns",
			sql:      psqlDescribeColumns,
			wantCols: []string{"attname", "format_type", "pg_get_expr", "attnotnull", "attcollation", "attidentity", "attgenerated"},
			wantRows: [][]string{{"id", "text", "", "f", "", "", ""}, {"name", "text", "", "f", "", "", ""}},
		},
		{
			name:     "backslash d+ users reads the verbose flags by index",
			sql:      psqlDescribeTableVerbose,
			wantCols: []string{"relchecks", "relkind", "relhasindex", "relhasrules", "relhastriggers", "relrowsecurity", "relforcerowsecurity", "relhasoids", "relispartition", "array_to_string", "reltablespace", "case", "relpersistence", "relreplident", "amname"},
			wantRows: [][]string{{"0", "r", "f", "f", "f", "f", "f", "f", "f", "", "0", "", "p", "d", "heap"}},
		},
		{
			name:     "backslash d+ users lists storage per column",
			sql:      psqlDescribeColumnsVerbose,
			wantCols: []string{"attname", "format_type", "pg_get_expr", "attnotnull", "attcollation", "attidentity", "attgenerated", "attstorage", "attcompression", "attstattarget", "col_description"},
			wantRows: [][]string{{"id", "text", "", "f", "", "", "", "x", "", "", ""}, {"name", "text", "", "f", "", "", "", "x", "", "", ""}},
		},
		{
			name:     "second table by oid",
			sql:      strings.ReplaceAll(psqlDescribeColumns, "'16384'", "'16385'"),
			wantCols: []string{"attname", "format_type", "pg_get_expr", "attnotnull", "attcollation", "attidentity", "attgenerated"},
			wantRows: [][]string{{"id", "text", "", "f", "", "", ""}, {"total", "text", "", "f", "", "", ""}},
		},
		{
			name:     "parents through pg_inherits are none",
			sql:      psqlDescribeParents,
			wantCols: []string{"oid"},
			wantRows: nil,
		},
		{
			name:     "children through pg_inherits are none",
			sql:      psqlDescribeChildren,
			wantCols: []string{"oid", "relkind", "inhdetachpending", "pg_get_expr"},
			wantRows: nil,
		},
		{
			name:     "unknown relation name resolves to nothing",
			sql:      strings.ReplaceAll(psqlDescribeLookup, "^(users)$", "^(nosuch)$"),
			wantCols: []string{"oid", "nspname", "relname"},
			wantRows: nil,
		},
		{
			name:     "wildcard pattern from backslash d us*",
			sql:      strings.ReplaceAll(psqlDescribeLookup, "^(users)$", "^(us.*)$"),
			wantCols: []string{"oid", "nspname", "relname"},
			wantRows: [][]string{{"16384", "public", "users"}},
		},
		{
			name:     "schema-qualified pattern for another schema resolves to nothing",
			sql:      strings.ReplaceAll(psqlDescribeLookup, "COLLATE pg_catalog.default", "COLLATE pg_catalog.default\n  AND n.nspname OPERATOR(pg_catalog.~) '^(other)$' COLLATE pg_catalog.default"),
			wantCols: []string{"oid", "nspname", "relname"},
			wantRows: nil,
		},
		{
			name:     "schema-qualified pattern for public",
			sql:      strings.ReplaceAll(psqlDescribeLookup, "COLLATE pg_catalog.default", "COLLATE pg_catalog.default\n  AND n.nspname OPERATOR(pg_catalog.~) '^(public)$' COLLATE pg_catalog.default"),
			wantCols: []string{"oid", "nspname", "relname"},
			wantRows: [][]string{{"16384", "public", "users"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := HandleCatalogQuery(&buf, tt.sql, schema, nil); err != nil {
				t.Fatalf("HandleCatalogQuery: %v", err)
			}
			cols, rows := collectResult(t, &buf)
			if !reflect.DeepEqual(cols, tt.wantCols) {
				t.Errorf("columns = %q, want %q", cols, tt.wantCols)
			}
			if len(rows) == 0 && len(tt.wantRows) == 0 {
				return
			}
			if !reflect.DeepEqual(rows, tt.wantRows) {
				t.Errorf("rows = %q, want %q", rows, tt.wantRows)
			}
		})
	}
}

// TestHandleCatalogQuery_RelationFilters covers the WHERE forms GUI clients use
// to single out one table, and the header a real server would give each item.
func TestHandleCatalogQuery_RelationFilters(t *testing.T) {
	schema := []redash.SchemaTable{
		{Name: "users", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "name"}}},
		{Name: "orders", Columns: []redash.SchemaColumn{{Name: "id"}, {Name: "total"}}},
	}

	tests := []struct {
		name     string
		sql      string
		wantCols []string
		wantRows [][]string
	}{
		{
			name:     "relname equality",
			sql:      "SELECT c.relname FROM pg_catalog.pg_class c WHERE c.relname = 'orders' AND c.relkind = 'r'",
			wantCols: []string{"relname"},
			wantRows: [][]string{{"orders"}},
		},
		{
			name:     "relname regex",
			sql:      "SELECT relname FROM pg_class WHERE relname ~ '^(users)$'",
			wantCols: []string{"relname"},
			wantRows: [][]string{{"users"}},
		},
		{
			name:     "relname case-insensitive regex",
			sql:      "SELECT relname FROM pg_class WHERE relname ~* '^USERS$'",
			wantCols: []string{"relname"},
			wantRows: [][]string{{"users"}},
		},
		{
			name:     "unquoted oid",
			sql:      "SELECT c.relname, c.oid FROM pg_class c WHERE c.oid = 16385",
			wantCols: []string{"relname", "oid"},
			wantRows: [][]string{{"orders", "16385"}},
		},
		{
			name:     "oid as regclass literal",
			sql:      "SELECT relname FROM pg_class WHERE oid = 'orders'::regclass",
			wantCols: []string{"relname"},
			wantRows: [][]string{{"orders"}},
		},
		{
			name:     "TablePlus columns by schema-qualified regclass",
			sql:      "SELECT a.attname AS column_name, pg_catalog.format_type(a.atttypid, a.atttypmod) AS data_type, a.attnotnull AS not_null FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'public.users'::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum",
			wantCols: []string{"column_name", "data_type", "not_null"},
			wantRows: [][]string{{"id", "text", "f"}, {"name", "text", "f"}},
		},
		{
			name:     "attrelid as quoted regclass",
			sql:      `SELECT attname FROM pg_attribute WHERE attrelid = '"public"."orders"'::regclass AND attnum > 0`,
			wantCols: []string{"attname"},
			wantRows: [][]string{{"id"}, {"total"}},
		},
		{
			name:     "attrelid as unquoted oid",
			sql:      "SELECT attname, attnum FROM pg_attribute WHERE attrelid = 16385 ORDER BY attnum",
			wantCols: []string{"attname", "attnum"},
			wantRows: [][]string{{"id", "1"}, {"total", "2"}},
		},
		{
			name:     "columns joined through pg_class by relname",
			sql:      "SELECT a.attname, c.relname, n.nspname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON a.attrelid = c.oid JOIN pg_catalog.pg_namespace n ON c.relnamespace = n.oid WHERE n.nspname = 'public' AND c.relname = 'users' AND a.attnum > 0",
			wantCols: []string{"attname", "relname", "nspname"},
			wantRows: [][]string{{"id", "users", "public"}, {"name", "users", "public"}},
		},
		{
			name:     "another schema has no columns",
			sql:      "SELECT a.attname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON a.attrelid = c.oid JOIN pg_catalog.pg_namespace n ON c.relnamespace = n.oid WHERE n.nspname = 'staging' AND c.relname = 'users'",
			wantCols: []string{"attname"},
			wantRows: nil,
		},
		{
			name:     "left join to an empty catalog keeps the tables",
			sql:      "SELECT c.oid, c.relname, d.description FROM pg_catalog.pg_class c LEFT OUTER JOIN pg_catalog.pg_description d ON d.objoid = c.oid AND d.objsubid = 0 AND d.classoid = 'pg_class'::regclass WHERE c.relkind NOT IN ('i','I','c')",
			wantCols: []string{"oid", "relname", "description"},
			wantRows: [][]string{{"16384", "users", ""}, {"16385", "orders", ""}},
		},
		{
			name:     "inner join to an empty catalog yields nothing",
			sql:      "SELECT c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_index i ON i.indrelid = c.oid",
			wantCols: []string{"relname"},
			wantRows: nil,
		},
		{
			name:     "regclass cast renders the table name",
			sql:      "SELECT c.oid::regclass, c.oid::pg_catalog.regclass::text AS qualified FROM pg_class c WHERE c.oid = '16384'",
			wantCols: []string{"oid", "qualified"},
			wantRows: [][]string{{"users", "users"}},
		},
		{
			name:     "information_schema.columns by table and schema",
			sql:      "SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'orders' ORDER BY ordinal_position",
			wantCols: []string{"column_name", "data_type"},
			wantRows: [][]string{{"id", "text"}, {"total", "text"}},
		},
		{
			name:     "information_schema.tables in another schema",
			sql:      "SELECT table_name FROM information_schema.tables WHERE table_schema = 'other'",
			wantCols: []string{"table_name"},
			wantRows: nil,
		},
		{
			name:     "literal in the select list is not a filter",
			sql:      "SELECT relname, 'oid = 1' AS note FROM pg_class WHERE relkind = 'r'",
			wantCols: []string{"relname", "note"},
			wantRows: [][]string{{"users", "oid = 1"}, {"orders", "oid = 1"}},
		},
		{
			name:     "table comment lookup without FROM yields one row",
			sql:      "SELECT pg_catalog.obj_description('16384'::pg_catalog.regclass, 'pg_class')",
			wantCols: []string{"obj_description"},
			wantRows: [][]string{{""}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := HandleCatalogQuery(&buf, tt.sql, schema, nil); err != nil {
				t.Fatalf("HandleCatalogQuery: %v", err)
			}
			cols, rows := collectResult(t, &buf)
			if !reflect.DeepEqual(cols, tt.wantCols) {
				t.Errorf("columns = %q, want %q", cols, tt.wantCols)
			}
			if len(rows) == 0 && len(tt.wantRows) == 0 {
				return
			}
			if !reflect.DeepEqual(rows, tt.wantRows) {
				t.Errorf("rows = %q, want %q", rows, tt.wantRows)
			}
		})
	}
}
