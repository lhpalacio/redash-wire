package pgwire

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

func HandleCatalogQuery(conn io.Writer, sql string, schema []redash.SchemaTable, sources []redash.DataSource) error {
	lower := normalize(sql)

	switch {
	case sqltext.ContainsToken(lower, "pg_database"):
		return handlePgDatabaseQuery(conn, sources)
	case strings.Contains(lower, "pg_statio_user_tables"):
		return handlePgStatioQuery(conn, schema)
	case sqltext.ContainsToken(lower, "information_schema.columns"):
		return handleInfoSchemaColumns(conn, sql, schema)
	case sqltext.ContainsToken(lower, "information_schema.tables"):
		return handleInfoSchemaTables(conn, sql, schema)
	case sqltext.ContainsToken(lower, "pg_attribute"):
		return handlePgAttribute(conn, sql, schema)
	case sqltext.ContainsToken(lower, "pg_class"):
		return handlePgClassQuery(conn, sql, lower, schema)
	case sqltext.ContainsToken(lower, "pg_namespace"):
		return handlePgNamespaceQuery(conn)
	case sqltext.ContainsToken(lower, "pg_type"):
		return SendEmptyResult(conn, []string{"oid", "typname"})
	case sqltext.ContainsToken(lower, "pg_proc"):
		return SendEmptyResult(conn, []string{"oid", "proname"})
	default:
		return SendEmptyResult(conn, []string{"name"})
	}
}

func sendResult(conn io.Writer, columns []string, rows [][]any) error {
	fields := make([]pgproto3.FieldDescription, len(columns))
	for i, name := range columns {
		fields[i] = pgproto3.FieldDescription{
			Name: []byte(name), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0,
		}
	}

	buf, err := encode((&pgproto3.RowDescription{Fields: fields}).Encode(nil))
	if err != nil {
		return err
	}
	for _, row := range rows {
		vals := make([][]byte, len(row))
		for i, v := range row {
			if v == nil {
				vals[i] = nil
				continue
			}
			vals[i] = []byte(fmt.Sprintf("%v", v))
		}
		buf, err = encode((&pgproto3.DataRow{Values: vals}).Encode(buf))
		if err != nil {
			return err
		}
	}
	buf, err = encode((&pgproto3.CommandComplete{CommandTag: []byte(fmt.Sprintf("SELECT %d", len(rows)))}).Encode(buf))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}
	_, err = conn.Write(buf)
	return err
}

// selCol is one entry from a SELECT list: the output (header) name shown to the
// client and the lowercased unqualified source column used to synthesize a value.
type selCol struct {
	output string
	source string
}

var (
	selectListRe  = regexp.MustCompile(`(?is)^\s*select\s+(.*?)\s+from\s`)
	asSeparatorRe = regexp.MustCompile(`(?i)\sas\s`)
)

// requestedColumns parses the SELECT list of a simple introspection query so the
// synthesized result has exactly the columns (and aliases) the client asked for.
func requestedColumns(sql string, defaults []selCol) []selCol {
	// Locate the SELECT-list span on the redacted text (so a literal/comment can't
	// be mistaken for the FROM boundary), then slice the ORIGINAL text at the same
	// offsets (Redact preserves byte positions) so quoted identifiers and casing
	// survive.
	loc := selectListRe.FindStringSubmatchIndex(sqltext.Redact(sql))
	if loc == nil {
		return defaults
	}
	list := strings.TrimSpace(sql[loc[2]:loc[3]])
	if list == "" || strings.Contains(list, "*") {
		return defaults
	}
	parts := sqltext.SplitTopLevelCommas(list)
	cols := make([]selCol, 0, len(parts))
	for _, p := range parts {
		if c, ok := parseSelCol(p); ok {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return defaults
	}
	return cols
}

func parseSelCol(item string) (selCol, bool) {
	item = strings.TrimSpace(item)
	if item == "" {
		return selCol{}, false
	}
	src, output := item, ""
	if loc := asSeparatorRe.FindStringIndex(item); loc != nil {
		src = strings.TrimSpace(item[:loc[0]])
		output = strings.TrimSpace(item[loc[1]:])
	} else if !strings.HasPrefix(item, `"`) {
		// "column alias" form without AS, but never for a double-quoted identifier,
		// which may legitimately contain spaces.
		if fields := strings.Fields(item); len(fields) == 2 {
			src, output = fields[0], fields[1]
		}
	}
	src = unqualify(src)
	if output == "" {
		output = src
	}
	output = strings.Trim(output, `"`)
	if output == "" {
		return selCol{}, false
	}
	return selCol{output: output, source: strings.ToLower(unqualify(src))}, true
}

func unqualify(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.Trim(s, `"`)
}

func outputNames(cols []selCol) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.output
	}
	return names
}

var tableNameFilterRe = regexp.MustCompile(`(?i)table_name\s*=\s*'([^']+)'`)

func tableNameFilter(sql string) string {
	m := tableNameFilterRe.FindStringSubmatch(sql)
	if m == nil {
		return ""
	}
	return m[1]
}

type kindPredicate struct {
	negated bool
	values  []string
}

var (
	relkindPredRe   = compileKindPredRe("relkind")
	tableTypePredRe = compileKindPredRe("table_type")

	leadingQuotedRe = regexp.MustCompile(`^\s*'([^']*)'`)
	quotedRe        = regexp.MustCompile(`'([^']*)'`)
)

func compileKindPredRe(column string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + column + `\b\s*(=\s*any\s*\(|not\s+in\s*\(|in\s*\(|!=|<>|=)`)
}

// columnPredicates parses every `column <op> <values>` comparison in sql. Operators
// are matched on the redacted text so a literal cannot fake one; the values live
// inside literals, so they are read from the original text at the same offsets
// (Redact preserves byte positions). Comparisons against anything other than a
// literal (e.g. another column in a join) are ignored.
func columnPredicates(sql string, re *regexp.Regexp) []kindPredicate {
	redacted := strings.ToLower(sqltext.Redact(sql))
	var preds []kindPredicate
	for _, loc := range re.FindAllStringSubmatchIndex(redacted, -1) {
		op := strings.Join(strings.Fields(redacted[loc[2]:loc[3]]), "")
		negated := op == "<>" || op == "!=" || strings.HasPrefix(op, "notin")

		var values []string
		if strings.HasSuffix(op, "(") {
			end := strings.IndexByte(redacted[loc[3]:], ')')
			if end < 0 {
				end = len(redacted) - loc[3]
			}
			for _, m := range quotedRe.FindAllStringSubmatch(sql[loc[3]:loc[3]+end], -1) {
				values = append(values, expandArrayLiteral(m[1])...)
			}
		} else {
			m := leadingQuotedRe.FindStringSubmatch(sql[loc[3]:])
			if m == nil {
				continue
			}
			values = expandArrayLiteral(m[1])
		}
		preds = append(preds, kindPredicate{negated: negated, values: values})
	}
	return preds
}

func expandArrayLiteral(v string) []string {
	if !strings.HasPrefix(v, "{") || !strings.HasSuffix(v, "}") {
		return []string{v}
	}
	parts := strings.Split(strings.Trim(v, "{}"), ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(strings.TrimSpace(p), `"`))
	}
	return out
}

// predicatesAdmit reports whether the predicates can match want: no negated
// predicate may name it, and if positive predicates exist at least one must name
// it (multiple positives are OR branches of one filter). fold normalizes values
// for comparison.
func predicatesAdmit(preds []kindPredicate, want string, fold func(string) string) bool {
	target := fold(want)
	hasPositive, wantInPositive := false, false
	for _, p := range preds {
		if p.negated {
			for _, v := range p.values {
				if fold(v) == target {
					return false
				}
			}
			continue
		}
		hasPositive = true
		for _, v := range p.values {
			if fold(v) == target {
				wantInPositive = true
			}
		}
	}
	return !hasPositive || wantInPositive
}

// relkindAdmitsTables reports whether a pg_class query's relkind filter can match
// an ordinary table ('r'), the only kind the proxy presents. A query for other
// kinds (views, matviews, sequences) must return zero rows; answering it with the
// table list made GUI clients show every table twice. Values stay case-sensitive
// ('S' is a sequence, 's' is not).
func relkindAdmitsTables(sql string) bool {
	return predicatesAdmit(columnPredicates(sql, relkindPredRe), "r", func(s string) string { return s })
}

// tableTypeAdmitsBaseTables is the information_schema.tables analogue: a query
// filtered to a table_type other than 'BASE TABLE' must not return the table list.
func tableTypeAdmitsBaseTables(sql string) bool {
	return predicatesAdmit(columnPredicates(sql, tableTypePredRe), "BASE TABLE", strings.ToUpper)
}

func handleInfoSchemaTables(conn io.Writer, sql string, schema []redash.SchemaTable) error {
	cols := requestedColumns(sql, []selCol{
		{"table_schema", "table_schema"}, {"table_name", "table_name"}, {"table_type", "table_type"},
	})
	if !tableTypeAdmitsBaseTables(sql) {
		schema = nil
	}
	rows := make([][]any, 0, len(schema))
	for _, t := range schema {
		rows = append(rows, infoSchemaTableRow(cols, t.Name))
	}
	return sendResult(conn, outputNames(cols), rows)
}

func infoSchemaTableRow(cols []selCol, table string) []any {
	row := make([]any, len(cols))
	for i, c := range cols {
		switch c.source {
		case "table_name":
			row[i] = table
		case "table_schema":
			row[i] = "public"
		case "table_catalog":
			row[i] = "redash"
		case "table_type":
			row[i] = "BASE TABLE"
		default:
			row[i] = nil
		}
	}
	return row
}

func handleInfoSchemaColumns(conn io.Writer, sql string, schema []redash.SchemaTable) error {
	cols := requestedColumns(sql, []selCol{
		{"table_name", "table_name"}, {"column_name", "column_name"},
		{"ordinal_position", "ordinal_position"}, {"data_type", "data_type"}, {"is_nullable", "is_nullable"},
	})
	filter := tableNameFilter(sql)

	var rows [][]any
	for _, t := range schema {
		if filter != "" && !strings.EqualFold(t.Name, filter) {
			continue
		}
		for ord, colName := range t.Columns {
			rows = append(rows, infoSchemaColumnRow(cols, t.Name, colName, ord+1))
		}
	}
	return sendResult(conn, outputNames(cols), rows)
}

func infoSchemaColumnRow(cols []selCol, table, column string, ordinal int) []any {
	row := make([]any, len(cols))
	for i, c := range cols {
		switch c.source {
		case "table_name":
			row[i] = table
		case "column_name":
			row[i] = column
		case "ordinal_position":
			row[i] = ordinal
		case "data_type", "udt_name":
			// Redash's schema endpoint does not expose per-column types; report text.
			row[i] = "text"
		case "is_nullable":
			row[i] = "YES"
		case "table_schema":
			row[i] = "public"
		case "table_catalog":
			row[i] = "redash"
		default:
			row[i] = nil
		}
	}
	return row
}

// Column types are unknown from Redash, so atttypid is reported as text (25).
func handlePgAttribute(conn io.Writer, sql string, schema []redash.SchemaTable) error {
	cols := requestedColumns(sql, []selCol{
		{"attname", "attname"}, {"attnum", "attnum"}, {"atttypid", "atttypid"},
	})
	filter := tableNameFilter(sql)

	var rows [][]any
	for _, t := range schema {
		if filter != "" && !strings.EqualFold(t.Name, filter) {
			continue
		}
		for i, colName := range t.Columns {
			row := make([]any, len(cols))
			for j, c := range cols {
				switch c.source {
				case "attname":
					row[j] = colName
				case "attnum":
					row[j] = i + 1
				case "atttypid":
					row[j] = OidText
				case "attnotnull":
					row[j] = "f"
				default:
					row[j] = nil
				}
			}
			rows = append(rows, row)
		}
	}
	return sendResult(conn, outputNames(cols), rows)
}

func handlePgNamespaceQuery(conn io.Writer) error {
	fields := []pgproto3.FieldDescription{
		{Name: []byte("nspname"), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0},
	}

	buf, err := encode((&pgproto3.RowDescription{Fields: fields}).Encode(nil))
	if err != nil {
		return err
	}

	buf, err = encode((&pgproto3.DataRow{
		Values: [][]byte{[]byte("public")},
	}).Encode(buf))
	if err != nil {
		return err
	}

	buf, err = encode((&pgproto3.CommandComplete{
		CommandTag: []byte("SELECT 1"),
	}).Encode(buf))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

func handlePgClassQuery(conn io.Writer, sql, lower string, schema []redash.SchemaTable) error {
	if !relkindAdmitsTables(sql) {
		schema = nil
	}
	if strings.Contains(lower, "estimated_row") || strings.Contains(lower, "reltuples") {
		return handlePgClassWithRowCount(conn, schema)
	}
	return handlePgClassTableListing(conn, schema)
}

func handlePgClassTableListing(conn io.Writer, schema []redash.SchemaTable) error {
	fields := []pgproto3.FieldDescription{
		{Name: []byte("oid"), DataTypeOID: OidInt4, DataTypeSize: 4, TypeModifier: -1, Format: 0},
		{Name: []byte("table_name"), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0},
		{Name: []byte("table_schema"), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0},
	}

	buf, err := encode((&pgproto3.RowDescription{Fields: fields}).Encode(nil))
	if err != nil {
		return err
	}

	for i, t := range schema {
		buf, err = encode((&pgproto3.DataRow{
			Values: [][]byte{
				[]byte(strconv.Itoa(16384 + i)),
				[]byte(t.Name),
				[]byte("public"),
			},
		}).Encode(buf))
		if err != nil {
			return err
		}
	}

	buf, err = encode((&pgproto3.CommandComplete{
		CommandTag: []byte(fmt.Sprintf("SELECT %d", len(schema))),
	}).Encode(buf))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

func handlePgClassWithRowCount(conn io.Writer, schema []redash.SchemaTable) error {
	fields := []pgproto3.FieldDescription{
		{Name: []byte("owner"), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0},
		{Name: []byte("name"), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0},
		{Name: []byte("estimated_row"), DataTypeOID: OidInt8, DataTypeSize: 8, TypeModifier: -1, Format: 0},
	}

	buf, err := encode((&pgproto3.RowDescription{Fields: fields}).Encode(nil))
	if err != nil {
		return err
	}

	for _, t := range schema {
		buf, err = encode((&pgproto3.DataRow{
			Values: [][]byte{
				[]byte("redash"),
				[]byte(t.Name),
				[]byte("-1"),
			},
		}).Encode(buf))
		if err != nil {
			return err
		}
	}

	buf, err = encode((&pgproto3.CommandComplete{
		CommandTag: []byte(fmt.Sprintf("SELECT %d", len(schema))),
	}).Encode(buf))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}

func handlePgStatioQuery(conn io.Writer, schema []redash.SchemaTable) error {
	fields := []pgproto3.FieldDescription{
		{Name: []byte("name"), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0},
		{Name: []byte("comment"), DataTypeOID: OidText, DataTypeSize: -1, TypeModifier: -1, Format: 0},
		{Name: []byte("total_size"), DataTypeOID: OidInt8, DataTypeSize: 8, TypeModifier: -1, Format: 0},
		{Name: []byte("data_size"), DataTypeOID: OidInt8, DataTypeSize: 8, TypeModifier: -1, Format: 0},
		{Name: []byte("index_size"), DataTypeOID: OidInt8, DataTypeSize: 8, TypeModifier: -1, Format: 0},
	}

	buf, err := encode((&pgproto3.RowDescription{Fields: fields}).Encode(nil))
	if err != nil {
		return err
	}

	for _, t := range schema {
		buf, err = encode((&pgproto3.DataRow{
			Values: [][]byte{
				[]byte(t.Name),
				nil,
				nil,
				nil,
				nil,
			},
		}).Encode(buf))
		if err != nil {
			return err
		}
	}

	buf, err = encode((&pgproto3.CommandComplete{
		CommandTag: []byte(fmt.Sprintf("SELECT %d", len(schema))),
	}).Encode(buf))
	if err != nil {
		return err
	}
	buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	return err
}
