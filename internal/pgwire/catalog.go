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

// firstTableOID is the pg_class oid of the first cached table; the others follow
// in schema order. The schema is cached for the session, so an oid a client
// reads from one listing (psql's \d table does) names the same table later.
const firstTableOID = 16384

func tableOID(i int) int { return firstTableOID + i }

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
		return handlePgClassQuery(conn, sql, schema)
	case sqltext.ContainsToken(lower, "pg_namespace"):
		return handlePgNamespaceQuery(conn)
	case sqltext.ContainsToken(lower, "pg_type"):
		return SendEmptyResult(conn, []string{"oid", "typname"})
	case sqltext.ContainsToken(lower, "pg_proc"):
		return SendEmptyResult(conn, []string{"oid", "proname"})
	case !sqltext.ContainsToken(lower, "from"):
		// A FROM-less SELECT of catalog functions (psql fetches a table's comment
		// this way) always yields exactly one row; an empty result would make a
		// client that reads row 0 fail.
		cols := requestedColumns(sql, []selCol{{output: "result"}})
		return sendResult(conn, outputNames(cols), [][]any{make([]any, len(cols))})
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
// client and the source expression, alias and top-level casts removed, from
// which a value is synthesized.
type selCol struct {
	output string
	expr   string
	cast   string // the trailing ::cast chain, lowercased, if any
}

var (
	selectStartRe = regexp.MustCompile(`^\s*select\s+(?:(?:distinct|all)\s+)?`)
	identRe       = regexp.MustCompile(`^(?:[A-Za-z_][\w$]*|"[^"]+")(?:\.(?:[A-Za-z_][\w$]*|"[^"]+"))*$`)
	bareIdentRe   = regexp.MustCompile(`^[A-Za-z_][\w$]*$`)
	funcCallRe    = regexp.MustCompile(`(?s)^(?:[A-Za-z_][\w$]*\.)*([A-Za-z_][\w$]*)\s*\(.*\)$`)
	castSuffixRe  = regexp.MustCompile(`(?:\s*::\s*(?:[A-Za-z_][\w$]*\.)*[A-Za-z_][\w$]*(?:\([^)]*\))?)+\s*$`)
	literalRe     = regexp.MustCompile(`(?s)^'(.*)'$`)
	numberRe      = regexp.MustCompile(`^-?\d+(?:\.\d+)?$`)
)

func isIdentByte(b byte) bool {
	return b == '_' || b == '$' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// tokenAt reports whether tok occurs at s[i] as a whole word.
func tokenAt(s string, i int, tok string) bool {
	if !strings.HasPrefix(s[i:], tok) {
		return false
	}
	end := i + len(tok)
	return (i == 0 || !isIdentByte(s[i-1])) && (end == len(s) || !isIdentByte(s[end]))
}

// selectList returns the text between SELECT and the statement's top-level
// FROM. The scan is parenthesis-aware on the redacted text, so a FROM inside a
// scalar subquery in the list (psql's \d emits several) or inside a literal
// does not end the list early; the slice is of the original text, so quoted
// identifiers and casing survive (Redact preserves byte positions).
func selectList(sql string) (string, bool) {
	red := strings.ToLower(sqltext.Postgres.Redact(sql))
	m := selectStartRe.FindStringIndex(red)
	if m == nil {
		return "", false
	}
	depth := 0
	for i := m[1]; i < len(red); i++ {
		switch red[i] {
		case '(':
			depth++
		case ')':
			depth--
		case 'f':
			if depth == 0 && tokenAt(red, i, "from") {
				return sql[m[1]:i], true
			}
		}
	}
	return sql[m[1]:], true
}

// requestedColumns parses the SELECT list of an introspection query so the
// synthesized result has exactly the columns (and aliases) the client asked for.
func requestedColumns(sql string, defaults []selCol) []selCol {
	list, ok := selectList(sql)
	if !ok {
		return defaults
	}
	list = strings.TrimSpace(list)
	if list == "" || strings.Contains(list, "*") {
		return defaults
	}
	var cols []selCol
	for _, p := range sqltext.Postgres.SplitTopLevelCommas(list) {
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
	expr, alias := splitAlias(item)
	c := exprCol(expr)
	c.output = alias
	if c.output == "" {
		c.output = exprName(c.expr)
	}
	return c, c.output != ""
}

// exprCol wraps an expression, separating a trailing ::cast chain so the value
// can be resolved from the bare expression.
func exprCol(expr string) selCol {
	expr = strings.TrimSpace(expr)
	c := selCol{expr: expr}
	if loc := castSuffixRe.FindStringIndex(expr); loc != nil {
		c.cast = strings.ToLower(expr[loc[0]:])
		c.expr = strings.TrimSpace(expr[:loc[0]])
	}
	return c
}

// splitAlias separates a select item into expression and alias: the last
// top-level AS, located on the redacted text so an "as" inside a literal or a
// subquery does not count, else the "expr alias" form of exactly two words.
func splitAlias(item string) (expr, alias string) {
	red := strings.ToLower(sqltext.Postgres.Redact(item))
	depth, at := 0, -1
	for i := 0; i+3 < len(red); i++ {
		switch red[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ' ', '\t', '\n', '\r':
			if depth == 0 && red[i+1:i+3] == "as" && isSpace(red[i+3]) {
				at = i
			}
		}
	}
	if at >= 0 {
		return strings.TrimSpace(item[:at]), strings.Trim(strings.TrimSpace(item[at+3:]), `"`)
	}
	if f := strings.Fields(red); len(f) == 2 {
		i := strings.LastIndex(red, f[1])
		if alias := item[i:]; bareIdentRe.MatchString(alias) {
			return strings.TrimSpace(item[:i]), alias
		}
	}
	return item, ""
}

// exprName is the header a real server gives an unaliased select item: the
// last component of an identifier, the name of a function call, the inner name
// of a scalar subquery, and ?column? for anything else.
func exprName(expr string) string {
	switch {
	case identRe.MatchString(expr):
		return unqualify(expr)
	case strings.HasPrefix(expr, "("):
		inner := strings.TrimSuffix(strings.TrimPrefix(expr, "("), ")")
		if cols := requestedColumns(inner, nil); len(cols) > 0 {
			return cols[0].output
		}
		return "?column?"
	case tokenAt(strings.ToLower(expr), 0, "case"):
		return "case"
	}
	if m := funcCallRe.FindStringSubmatch(expr); m != nil {
		return m[1]
	}
	return "?column?"
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

// lookup resolves the identifiers (and, with an "fn:" prefix, the function
// calls) a catalog row can answer; ok is false for anything the proxy cannot
// know, which is then NULL rather than a guess.
type lookup func(key string) (v any, ok bool)

// evalExpr synthesizes the value of one select item for a row.
func evalExpr(c selCol, get lookup) any {
	expr := strings.TrimSpace(c.expr)
	lower := strings.ToLower(expr)
	switch {
	case expr == "":
		return nil
	case lower == "true":
		return "t"
	case lower == "false":
		return "f"
	case lower == "null":
		return nil
	case identRe.MatchString(expr):
		key := strings.ToLower(unqualify(expr))
		if strings.Contains(c.cast, "regclass") {
			key = "regclass" // an oid shown as a relation name
		}
		v, _ := get(key)
		return v
	case numberRe.MatchString(expr):
		return expr
	case strings.HasPrefix(expr, "'"):
		if m := literalRe.FindStringSubmatch(expr); m != nil {
			return strings.ReplaceAll(m[1], "''", "'")
		}
		return nil
	case tokenAt(lower, 0, "case"):
		return evalCase(expr, get)
	}
	if m := funcCallRe.FindStringSubmatch(expr); m != nil {
		v, _ := get("fn:" + strings.ToLower(m[1]))
		return v
	}
	return nil
}

func rowFor(cols []selCol, get lookup) []any {
	row := make([]any, len(cols))
	for i, c := range cols {
		row[i] = evalExpr(c, get)
	}
	return row
}

func sameValue(a, b any) bool {
	return a != nil && b != nil && fmt.Sprint(a) == fmt.Sprint(b)
}

// evalCase evaluates a CASE expression for a row: the simple form compares the
// operand with each WHEN value, the searched form tests each WHEN condition
// with evalCond. Keywords are located on the redacted text at nesting depth
// zero, so a literal or a subquery cannot supply them.
func evalCase(expr string, get lookup) any {
	red := strings.ToLower(sqltext.Postgres.Redact(expr))
	type span struct {
		kw         string
		start, end int
	}
	var spans []span
	depth := 0
	for i := 0; i < len(red); i++ {
		switch red[i] {
		case '(':
			depth++
		case ')':
			depth--
		default:
			if depth != 0 {
				continue
			}
			for _, kw := range []string{"case", "when", "then", "else", "end"} {
				if tokenAt(red, i, kw) {
					spans = append(spans, span{kw, i, i + len(kw)})
					i += len(kw) - 1
					break
				}
			}
		}
	}
	if len(spans) < 4 || spans[0].kw != "case" || spans[len(spans)-1].kw != "end" {
		return nil
	}
	operand := strings.TrimSpace(expr[spans[0].end:spans[1].start])
	var opv any
	if operand != "" {
		opv = evalExpr(exprCol(operand), get)
	}
	for k := 1; k+1 < len(spans); k++ {
		s, next := spans[k], spans[k+1]
		switch s.kw {
		case "when":
			if next.kw != "then" || k+2 >= len(spans) {
				return nil
			}
			cond := expr[s.end:next.start]
			result := expr[next.end:spans[k+2].start]
			var hit bool
			if operand != "" {
				hit = sameValue(opv, evalExpr(exprCol(cond), get))
			} else {
				hit = evalCond(cond, get)
			}
			if hit {
				return evalExpr(exprCol(result), get)
			}
			k++
		case "else":
			return evalExpr(exprCol(expr[s.end:next.start]), get)
		}
	}
	return nil
}

var condRe = regexp.MustCompile(`(?is)^\s*(.+?)\s*(=|<>|!=|\bnot\s+in\b|\bin\b)\s*(.+?)\s*$`)

// evalCond evaluates one "a = b", "a <> b" or "a IN (...)" condition. Anything
// else, or a NULL operand, is not satisfied, as in SQL.
func evalCond(cond string, get lookup) bool {
	m := condRe.FindStringSubmatchIndex(sqltext.Postgres.Redact(cond))
	if m == nil {
		return false
	}
	left := evalExpr(exprCol(cond[m[2]:m[3]]), get)
	if left == nil {
		return false
	}
	op := strings.Join(strings.Fields(strings.ToLower(cond[m[4]:m[5]])), " ")
	right := strings.TrimSpace(cond[m[6]:m[7]])
	switch op {
	case "=", "<>", "!=":
		r := evalExpr(exprCol(right), get)
		return r != nil && (op == "=") == sameValue(left, r)
	default:
		right = strings.TrimSuffix(strings.TrimPrefix(right, "("), ")")
		found := false
		for _, item := range sqltext.Postgres.SplitTopLevelCommas(right) {
			if sameValue(left, evalExpr(exprCol(item), get)) {
				found = true
			}
		}
		return found == (op == "in")
	}
}

// pgClassLookup answers for one cached table as a plain heap table owned by
// "redash" in schema public, with the pg_class flags a real server would report
// for a table that has no indexes, rules, triggers, or policies (so psql's \d
// skips the lookups the proxy could not answer anyway).
func pgClassLookup(i int, t redash.SchemaTable) lookup {
	return func(key string) (any, bool) {
		switch key {
		case "oid", "relfilenode":
			return tableOID(i), true
		case "relname", "regclass", "table_name", "tablename":
			return t.Name, true
		case "nspname", "table_schema", "schemaname":
			return "public", true
		case "table_catalog":
			return "redash", true
		case "table_type":
			return "BASE TABLE", true
		case "relkind":
			return "r", true
		case "relnamespace":
			return 2200, true
		case "relowner":
			return 10, true
		case "fn:pg_get_userbyid", "tableowner":
			return "redash", true
		case "relam":
			return 2, true
		case "amname":
			return "heap", true
		case "relnatts":
			return len(t.Columns), true
		case "reltuples":
			return -1, true
		case "relpages", "relchecks", "reltablespace", "reltoastrelid", "reloftype":
			return 0, true
		case "relpersistence":
			return "p", true
		case "relreplident":
			return "d", true
		case "relispopulated", "fn:pg_table_is_visible":
			return "t", true
		case "relhasindex", "relhasrules", "relhastriggers", "relrowsecurity", "relforcerowsecurity",
			"relhasoids", "relispartition", "relisshared", "relhassubclass":
			return "f", true
		}
		return nil, false
	}
}

// pgAttributeLookup answers for one column of a cached table. Redash's schema
// endpoint does not expose per-column types, so every column is text (25),
// nullable, without default, identity, or generation expression.
func pgAttributeLookup(i int, t redash.SchemaTable, ordinal int, column string) lookup {
	class := pgClassLookup(i, t)
	return func(key string) (any, bool) {
		switch key {
		case "attname", "column_name":
			return column, true
		case "attnum", "ordinal_position":
			return ordinal, true
		case "attrelid":
			return tableOID(i), true
		case "atttypid":
			return OidText, true
		case "fn:format_type", "typname", "data_type", "udt_name", "type_name", "column_type":
			return "text", true
		case "atttypmod", "attlen":
			return -1, true
		case "attndims", "attinhcount":
			return 0, true
		case "attcollation":
			return 100, true
		case "attstorage":
			return "x", true // text is TOAST-able: extended
		case "attalign":
			return "i", true
		case "attidentity", "attgenerated", "attcompression":
			return "", true
		case "attnotnull", "atthasdef", "attisdropped", "attbyval":
			return "f", true
		case "attislocal":
			return "t", true
		case "is_nullable":
			return "YES", true
		}
		return class(key)
	}
}

// relationFilter is the part of a catalog query's WHERE clause the proxy can
// honor: which tables it names (by relation name, pattern, or oid), whether it
// asks for a schema other than public, and whether it inner-joins a catalog
// the proxy models as empty (psql's inheritance lookups do), in which case no
// table qualifies.
type relationFilter struct {
	names    []string
	patterns []*regexp.Regexp
	oids     []int
	none     bool
}

// The operator regexes end at the operator: the literal that follows is blank
// in the redacted text and is read from the original at that offset.
var (
	nameEqRe      = regexp.MustCompile(`\b(?:relname|table_name|tablename)\s*=`)
	oidEqRe       = regexp.MustCompile(`\b(?:oid|attrelid)\s*=`)
	namePatternRe = regexp.MustCompile(`\brelname\s*(~\*?|operator\s*\(\s*pg_catalog\s*\.\s*~\*?\s*\))`)
	schemaRe      = regexp.MustCompile(`\b(?:nspname|table_schema|schemaname)\s*(=|~\*?|operator\s*\(\s*pg_catalog\s*\.\s*~\*?\s*\))`)
	emptyJoinRe   = regexp.MustCompile(`(,|\bjoin|\bfrom)\s+(?:pg_catalog\.)?(?:pg_inherits|pg_index|pg_constraint|pg_trigger|pg_rewrite|pg_policy|pg_depend|pg_partitioned_table)\b`)
	leadingIntRe  = regexp.MustCompile(`^\s*(\d+)\b`)
)

// parseRelationFilter reads the predicates from the redacted text (so a literal
// cannot fake one) and their values from the original at the same offsets.
func parseRelationFilter(sql string) relationFilter {
	red := strings.ToLower(sqltext.Postgres.Redact(sql))
	var f relationFilter

	for _, loc := range nameEqRe.FindAllStringIndex(red, -1) {
		if name, ok := quotedAt(sql, loc[1]); ok {
			f.names = append(f.names, name)
		}
	}
	for _, loc := range oidEqRe.FindAllStringIndex(red, -1) {
		if m := leadingIntRe.FindStringSubmatch(sql[loc[1]:]); m != nil {
			n, _ := strconv.Atoi(m[1])
			f.oids = append(f.oids, n)
			continue
		}
		v, ok := quotedAt(sql, loc[1])
		if !ok {
			continue // compared to another column, as in a join
		}
		if n, err := strconv.Atoi(v); err == nil {
			f.oids = append(f.oids, n)
		} else {
			// 'name'::regclass, possibly schema-qualified and quoted.
			f.names = append(f.names, unqualify(v))
		}
	}
	for _, loc := range namePatternRe.FindAllStringSubmatchIndex(red, -1) {
		pat, ok := quotedAt(sql, loc[1])
		if !ok {
			continue
		}
		if strings.Contains(red[loc[2]:loc[3]], "*") {
			pat = "(?i)" + pat
		}
		if re, err := regexp.Compile(pat); err == nil {
			f.patterns = append(f.patterns, re)
		}
	}
	for _, loc := range schemaRe.FindAllStringSubmatchIndex(red, -1) {
		v, ok := quotedAt(sql, loc[1])
		if !ok {
			continue
		}
		op := red[loc[2]:loc[3]]
		if op == "=" {
			f.none = f.none || !strings.EqualFold(v, "public")
			continue
		}
		if strings.Contains(op, "*") {
			v = "(?i)" + v
		}
		if re, err := regexp.Compile(v); err == nil && !re.MatchString("public") {
			f.none = true
		}
	}
	for _, loc := range emptyJoinRe.FindAllStringSubmatchIndex(red, -1) {
		if red[loc[2]:loc[3]] == "join" && isOuterJoin(red[:loc[2]]) {
			continue
		}
		f.none = true
	}
	return f
}

// quotedAt reads the string literal that opens at sql[i].
func quotedAt(sql string, i int) (string, bool) {
	m := leadingQuotedRe.FindStringSubmatch(sql[i:])
	if m == nil {
		return "", false
	}
	return m[1], true
}

// isOuterJoin reports whether the text before a JOIN keyword makes it a LEFT or
// FULL join, which keeps every table even against an empty catalog.
func isOuterJoin(before string) bool {
	f := strings.Fields(before)
	if len(f) == 0 {
		return false
	}
	switch f[len(f)-1] {
	case "left", "full", "outer":
		return true
	}
	return false
}

func (f relationFilter) admits(i int, t redash.SchemaTable) bool {
	if f.none {
		return false
	}
	if len(f.names) > 0 {
		found := false
		for _, n := range f.names {
			if strings.EqualFold(n, t.Name) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	for _, re := range f.patterns {
		if !re.MatchString(t.Name) {
			return false
		}
	}
	if len(f.oids) > 0 {
		found := false
		for _, oid := range f.oids {
			if oid == tableOID(i) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// admittedTables returns the indices of the cached tables a query's filters
// admit; kindOK is the verdict of the relkind/table_type predicates.
func admittedTables(sql string, schema []redash.SchemaTable, kindOK bool) []int {
	if !kindOK {
		return nil
	}
	f := parseRelationFilter(sql)
	var idx []int
	for i, t := range schema {
		if f.admits(i, t) {
			idx = append(idx, i)
		}
	}
	return idx
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
	redacted := strings.ToLower(sqltext.Postgres.Redact(sql))
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
		{output: "table_schema", expr: "table_schema"},
		{output: "table_name", expr: "table_name"},
		{output: "table_type", expr: "table_type"},
	})
	var rows [][]any
	for _, i := range admittedTables(sql, schema, tableTypeAdmitsBaseTables(sql)) {
		rows = append(rows, rowFor(cols, pgClassLookup(i, schema[i])))
	}
	return sendResult(conn, outputNames(cols), rows)
}

func handleInfoSchemaColumns(conn io.Writer, sql string, schema []redash.SchemaTable) error {
	cols := requestedColumns(sql, []selCol{
		{output: "table_name", expr: "table_name"},
		{output: "column_name", expr: "column_name"},
		{output: "ordinal_position", expr: "ordinal_position"},
		{output: "data_type", expr: "data_type"},
		{output: "is_nullable", expr: "is_nullable"},
	})
	return sendResult(conn, outputNames(cols), columnRows(sql, schema, cols, true))
}

func handlePgAttribute(conn io.Writer, sql string, schema []redash.SchemaTable) error {
	cols := requestedColumns(sql, []selCol{
		{output: "attname", expr: "attname"},
		{output: "attnum", expr: "attnum"},
		{output: "atttypid", expr: "atttypid"},
	})
	return sendResult(conn, outputNames(cols), columnRows(sql, schema, cols, relkindAdmitsTables(sql)))
}

// columnRows builds one row per column of every table the query admits.
func columnRows(sql string, schema []redash.SchemaTable, cols []selCol, kindOK bool) [][]any {
	var rows [][]any
	for _, i := range admittedTables(sql, schema, kindOK) {
		for ord, column := range schema[i].Columns {
			rows = append(rows, rowFor(cols, pgAttributeLookup(i, schema[i], ord+1, column)))
		}
	}
	return rows
}

func handlePgClassQuery(conn io.Writer, sql string, schema []redash.SchemaTable) error {
	cols := requestedColumns(sql, []selCol{
		{output: "oid", expr: "oid"},
		{output: "table_name", expr: "table_name"},
		{output: "table_schema", expr: "table_schema"},
	})
	var rows [][]any
	for _, i := range admittedTables(sql, schema, relkindAdmitsTables(sql)) {
		rows = append(rows, rowFor(cols, pgClassLookup(i, schema[i])))
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
