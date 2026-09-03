package mysqlwire

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

const (
	serverVersion = "8.0.0-redash-wire"
	localUser     = "redash@localhost"
)

// localSession is what a locally answered query can see of the connection.
type localSession struct {
	dbName  string
	connID  uint32
	sources []redash.DataSource
	schema  []redash.SchemaTable
	// readOnly flips read_only, super_read_only, transaction_read_only and
	// tx_read_only to 1, so a client that checks before writing gets the truth.
	readOnly bool
}

// normalize lowercases a statement after redacting string-literal and comment
// contents, so classification never matches inside a literal or comment.
func normalize(sql string) string {
	return strings.ToLower(strings.TrimSpace(sqltext.MySQL.Redact(sql)))
}

// hasKeyword reports whether lower starts with kw as a whole word, so "select"
// is found ahead of a newline or a tab as well as a space.
func hasKeyword(lower, kw string) bool {
	return strings.HasPrefix(lower, kw) && (len(lower) == len(kw) || !isIdentByte(lower[len(kw)]))
}

// localFunctions are the zero-argument functions a FROM-less SELECT can ask
// about the session; their presence makes the statement local.
var localFunctions = []string{
	"version", "database", "schema", "current_user", "user",
	"session_user", "system_user", "connection_id",
}

func isLocalQuery(sql string) bool {
	lower := normalize(sql)

	if strings.HasPrefix(lower, "set ") {
		return true
	}
	if strings.HasPrefix(lower, "begin") || strings.HasPrefix(lower, "start transaction") ||
		strings.HasPrefix(lower, "commit") || strings.HasPrefix(lower, "rollback") {
		return true
	}
	if strings.HasPrefix(lower, "show databases") || strings.HasPrefix(lower, "show schemas") {
		return true
	}
	if strings.HasPrefix(lower, "show tables") || strings.HasPrefix(lower, "show full tables") {
		return true
	}
	if strings.HasPrefix(lower, "show variables") || strings.HasPrefix(lower, "show session variables") ||
		strings.HasPrefix(lower, "show global variables") {
		return true
	}
	if strings.HasPrefix(lower, "show session status") || strings.HasPrefix(lower, "show status") {
		return true
	}

	if sqltext.ContainsToken(lower, "information_schema.") {
		return true
	}

	// A SELECT with no FROM that reads server state is answered here; one that
	// reads a table goes to Redash however it mentions DATABASE() or @@version.
	// FROM is matched as a token of the redacted text, so it is found on its own
	// line and never inside a literal or comment.
	if hasKeyword(lower, "select") && !sqltext.ContainsToken(lower, "from") {
		if strings.Contains(lower, "@@") {
			return true
		}
		for _, fn := range localFunctions {
			if sqltext.ContainsToken(lower, fn+"(") {
				return true
			}
		}
	}

	return false
}

func handleLocalQuery(sql string, sess localSession) (*mysql.Result, error) {
	lower := normalize(sql)

	if strings.HasPrefix(lower, "set ") ||
		strings.HasPrefix(lower, "begin") || strings.HasPrefix(lower, "start transaction") ||
		strings.HasPrefix(lower, "commit") || strings.HasPrefix(lower, "rollback") {
		return nil, nil
	}

	if strings.HasPrefix(lower, "show databases") || strings.HasPrefix(lower, "show schemas") {
		return handleShowDatabases(sess.sources)
	}

	if strings.HasPrefix(lower, "show tables") || strings.HasPrefix(lower, "show full tables") {
		return handleShowTables(sql, sess)
	}

	if strings.HasPrefix(lower, "show variables") || strings.HasPrefix(lower, "show session variables") ||
		strings.HasPrefix(lower, "show global variables") {
		return handleShowVariables(sql, sess.readOnly)
	}

	if strings.HasPrefix(lower, "show session status") || strings.HasPrefix(lower, "show status") {
		return handleShowStatus(lower)
	}

	if strings.Contains(lower, "information_schema.") {
		return handleInformationSchema(sql, sess)
	}

	return handleLocalSelect(sql, sess)
}

// handleLocalSelect answers a FROM-less SELECT with one column per select-list
// item, named the way MySQL names it: the alias when there is one, otherwise the
// item's text as written. Every item is evaluated, so Connector/J's 19-variable
// startup probe gets 19 columns back and SELECT DATABASE(), USER(), VERSION()
// gets three. An item the proxy cannot evaluate is NULL.
func handleLocalSelect(sql string, sess localSession) (*mysql.Result, error) {
	var names []string
	var values []any
	for _, item := range sqltext.MySQL.SplitTopLevelCommas(selectList(sql)) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		expr, alias := splitAlias(item)
		if alias == "" {
			alias = expr
		}
		names = append(names, alias)
		values = append(values, evalLocalExpr(expr, sess))
	}
	if len(names) == 0 {
		return singleResult("result", "")
	}
	rs, err := buildResultset(names, [][]any{values})
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

// selectModifiers may precede the select list; they never name a column.
var selectModifiers = map[string]bool{
	"all": true, "distinct": true, "distinctrow": true,
	"sql_no_cache": true, "sql_cache": true, "sql_calc_found_rows": true,
}

// selectList returns the select-list text of a SELECT: what follows the
// keyword up to the top-level FROM, a trailing LIMIT (the mysql CLI's `select
// @@version_comment limit 1`) or a semicolon, all located in the redacted text
// so none is matched inside a literal, and outside parentheses so a subquery's
// FROM does not end the list early.
func selectList(sql string) string {
	red := strings.ToLower(sqltext.MySQL.Redact(sql))
	start := strings.Index(red, "select")
	if start < 0 {
		return ""
	}
	start += len("select")
	end := len(red)
	depth := 0
scan:
	for i := start; i < len(red); i++ {
		switch red[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ';':
			if depth == 0 {
				end = i
				break scan
			}
		case 'l':
			if depth == 0 && tokenAt(red, i, "limit") {
				end = i
				break scan
			}
		case 'f':
			if depth == 0 && tokenAt(red, i, "from") {
				end = i
				break scan
			}
		}
	}
	list := sql[start:end]
	for {
		trimmed := strings.TrimLeft(list, " \t\r\n")
		word, rest := trimmed, ""
		if i := strings.IndexAny(trimmed, " \t\r\n"); i >= 0 {
			word, rest = trimmed[:i], trimmed[i:]
		}
		if !selectModifiers[strings.ToLower(word)] {
			return list
		}
		list = rest
	}
}

// tokenAt reports whether kw starts at s[i] bounded by non-identifier bytes.
func tokenAt(s string, i int, kw string) bool {
	if !strings.HasPrefix(s[i:], kw) {
		return false
	}
	end := i + len(kw)
	return (i == 0 || !isIdentByte(s[i-1])) && (end == len(s) || !isIdentByte(s[end]))
}

// notAnAlias are words that end a select item without naming it.
var notAnAlias = map[string]bool{
	"and": true, "as": true, "false": true, "in": true, "is": true, "like": true,
	"not": true, "null": true, "or": true, "true": true,
}

// splitAlias separates a select item into its expression and its alias, which
// is either introduced by AS or bare (`@@version v`), and may be a quoted
// identifier or, after AS, a string.
func splitAlias(item string) (expr, alias string) {
	toks := lexTokens(item)
	n := len(toks)
	if n >= 3 && toks[n-2].is(tokWord, "as") && toks[n-1].kind != tokOp && toks[n-1].kind != tokVar {
		return strings.TrimSpace(item[:toks[n-2].pos]), toks[n-1].aliasText()
	}
	if n >= 2 && (toks[n-1].kind == tokWord || toks[n-1].kind == tokQuoted) && !notAnAlias[toks[n-1].text] {
		prev := toks[n-2]
		if prev.kind != tokOp || prev.text == ")" {
			return strings.TrimSpace(item[:toks[n-1].pos]), toks[n-1].aliasText()
		}
	}
	return strings.TrimSpace(item), ""
}

// evalLocalExpr resolves one select item: a system variable, a literal, or one
// of the session functions; anything else is NULL.
func evalLocalExpr(expr string, sess localSession) any {
	toks := lexTokens(expr)
	switch len(toks) {
	case 1:
		t := toks[0]
		switch t.kind {
		case tokVar:
			v, _ := lookupVariable(t.text, sess.readOnly)
			return v
		case tokNumber:
			if n, err := strconv.ParseInt(t.text, 10, 64); err == nil {
				return n
			}
			if f, err := strconv.ParseFloat(t.text, 64); err == nil {
				return f
			}
			return t.text
		case tokString:
			return t.text
		case tokWord:
			switch t.text {
			case "null":
				return nil
			case "true":
				return int64(1)
			case "false":
				return int64(0)
			case "current_user":
				return localUser
			}
		}
	case 3:
		if toks[0].kind == tokWord && toks[1].is(tokOp, "(") && toks[2].is(tokOp, ")") {
			return callLocalFunction(toks[0].text, sess)
		}
	}
	return nil
}

func callLocalFunction(name string, sess localSession) any {
	switch name {
	case "version":
		return serverVersion
	case "database", "schema":
		if sess.dbName == "" {
			return nil
		}
		return sess.dbName
	case "user", "current_user", "session_user", "system_user":
		return localUser
	case "connection_id":
		return int64(sess.connID)
	}
	return nil
}

// sessionVariables backs both SHOW VARIABLES and SELECT @@name. Values keep
// their Go type so SELECT @@max_allowed_packet stays an integer column.
var sessionVariables = []struct {
	name  string
	value any
}{
	{"auto_increment_increment", 1},
	{"autocommit", 1},
	{"character_set_client", "utf8mb4"},
	{"character_set_connection", "utf8mb4"},
	{"character_set_database", "utf8mb4"},
	{"character_set_results", "utf8mb4"},
	{"character_set_server", "utf8mb4"},
	{"collation_connection", "utf8mb4_general_ci"},
	{"collation_database", "utf8mb4_general_ci"},
	{"collation_server", "utf8mb4_general_ci"},
	{"init_connect", ""},
	{"interactive_timeout", 28800},
	{"license", "GPL"},
	{"lower_case_table_names", 0},
	{"max_allowed_packet", 67108864},
	{"net_read_timeout", 30},
	{"net_write_timeout", 60},
	{"performance_schema", 0},
	{"read_only", 0},
	{"sql_mode", "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"},
	{"super_read_only", 0},
	{"system_time_zone", "UTC"},
	{"time_zone", "SYSTEM"},
	{"transaction_isolation", "REPEATABLE-READ"},
	{"transaction_read_only", 0},
	{"tx_isolation", "REPEATABLE-READ"},
	{"tx_read_only", 0},
	{"version", serverVersion},
	{"version_comment", "redash-wire MySQL proxy"},
	{"wait_timeout", 28800},
}

// readOnlyVariables are the ones that answer 1 under read-only mode: what a
// MySQL server started with --read-only (and super_read_only) reports.
var readOnlyVariables = map[string]bool{
	"read_only": true, "super_read_only": true, "transaction_read_only": true, "tx_read_only": true,
}

// variableValue applies the read-only override to a variable's stock value.
func variableValue(name string, value any, readOnly bool) any {
	if readOnly && readOnlyVariables[name] {
		return 1
	}
	return value
}

// lookupVariable resolves @@name, @@session.name, @@local.name and
// @@global.name, case-insensitively. An unknown variable is NULL rather than
// an error, since a client probing a dozen at once should still get the rest.
func lookupVariable(ref string, readOnly bool) (any, bool) {
	name, ok := strings.CutPrefix(strings.ToLower(ref), "@@")
	if !ok {
		return nil, false
	}
	for _, scope := range [...]string{"session.", "local.", "global."} {
		name = strings.TrimPrefix(name, scope)
	}
	for _, v := range sessionVariables {
		if v.name == name {
			return variableValue(v.name, v.value, readOnly), true
		}
	}
	return nil, false
}

func handleShowDatabases(sources []redash.DataSource) (*mysql.Result, error) {
	values := make([][]any, len(sources))
	for i, ds := range sources {
		values[i] = []any{ds.Name}
	}
	rs, err := buildResultset([]string{"Database"}, values)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

// handleShowTables answers SHOW [FULL] TABLES [{FROM | IN} db] [LIKE 'pattern'
// | WHERE expr] from the cached schema of the current database. FROM/IN may
// only name the current database, since that is the only schema in hand;
// WHERE supports Tables_in_<db> and Table_type compared with =, !=, <> or
// LIKE, joined by AND. Anything else is refused rather than answered with the
// wrong list.
func handleShowTables(sql string, sess localSession) (*mysql.Result, error) {
	toks := lexTokens(strings.TrimRight(strings.TrimSpace(sql), ";"))
	i := 1 // past SHOW
	full := false
	if i < len(toks) && toks[i].is(tokWord, "full") {
		full = true
		i++
	}
	i++ // past TABLES

	column := "Tables"
	if sess.dbName != "" {
		column = "Tables_in_" + sess.dbName
	}
	if i < len(toks) && (toks[i].is(tokWord, "from") || toks[i].is(tokWord, "in")) {
		i++
		if i >= len(toks) || (toks[i].kind != tokWord && toks[i].kind != tokQuoted) {
			return nil, showTablesSyntaxError()
		}
		named := toks[i].aliasText()
		i++
		if !strings.EqualFold(named, sess.dbName) {
			for _, ds := range sess.sources {
				if strings.EqualFold(ds.Name, named) {
					return nil, mysql.NewDefaultError(mysql.ER_NOT_SUPPORTED_YET,
						fmt.Sprintf("SHOW TABLES FROM a database other than the current one; USE `%s` first", ds.Name))
				}
			}
			return nil, mysql.NewDefaultError(mysql.ER_BAD_DB_ERROR, named)
		}
	}

	match := func(name, tableType string) bool { return true }
	if i < len(toks) {
		switch {
		case toks[i].is(tokWord, "like") && i+1 < len(toks) && toks[i+1].kind == tokString && i+2 == len(toks):
			pattern := toks[i+1].text
			column += " (" + pattern + ")"
			like := likeMatcher(pattern)
			match = func(name, _ string) bool { return like(name) }
		case toks[i].is(tokWord, "where"):
			var err error
			if match, err = showTablesWhere(toks[i+1:], sess.dbName); err != nil {
				return nil, err
			}
		default:
			return nil, showTablesSyntaxError()
		}
	}

	var values [][]any
	for _, t := range sess.schema {
		if !match(t.Name, "BASE TABLE") {
			continue
		}
		if full {
			values = append(values, []any{t.Name, "BASE TABLE"})
		} else {
			values = append(values, []any{t.Name})
		}
	}
	names := []string{column}
	if full {
		names = append(names, "Table_type")
	}
	rs, err := buildResultset(names, values)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func showTablesSyntaxError() error {
	return mysql.NewError(mysql.ER_PARSE_ERROR,
		"You have an error in your SQL syntax; expected SHOW [FULL] TABLES [{FROM | IN} db] [LIKE 'pattern' | WHERE expr]")
}

// showTablesWhere compiles the WHERE clause of SHOW TABLES into a predicate
// over (table name, table type).
func showTablesWhere(toks []token, dbName string) (func(name, tableType string) bool, error) {
	unsupported := func() error {
		return mysql.NewDefaultError(mysql.ER_NOT_SUPPORTED_YET,
			fmt.Sprintf("SHOW TABLES WHERE beyond Tables_in_%s or Table_type compared with =, !=, <> or LIKE, joined by AND", dbName))
	}
	if len(toks) == 0 {
		return nil, showTablesSyntaxError()
	}

	var preds []func(name, tableType string) bool
	var cond []token
	for i := 0; i <= len(toks); i++ {
		if i < len(toks) && !toks[i].is(tokWord, "and") {
			cond = append(cond, toks[i])
			continue
		}
		pred, err := showTablesCondition(cond, dbName, unsupported)
		if err != nil {
			return nil, err
		}
		preds = append(preds, pred)
		cond = nil
	}
	return func(name, tableType string) bool {
		for _, p := range preds {
			if !p(name, tableType) {
				return false
			}
		}
		return true
	}, nil
}

// showTablesCondition compiles one `<column> <op> 'value'` comparison.
func showTablesCondition(cond []token, dbName string, unsupported func() error) (func(name, tableType string) bool, error) {
	if len(cond) < 3 || (cond[0].kind != tokWord && cond[0].kind != tokQuoted) {
		return nil, unsupported()
	}
	var field func(name, tableType string) string
	switch col := cond[0].aliasText(); {
	case strings.EqualFold(col, "tables_in_"+dbName):
		field = func(name, _ string) string { return name }
	case strings.EqualFold(col, "table_type"):
		field = func(_, tableType string) string { return tableType }
	default:
		return nil, unsupported()
	}

	op := cond[1].text
	rest := cond[2:]
	if cond[1].is(tokWord, "not") && len(rest) > 0 && rest[0].is(tokWord, "like") {
		op, rest = "not like", rest[1:]
	}
	if len(rest) != 1 || rest[0].kind != tokString {
		return nil, unsupported()
	}
	value := rest[0].text

	var test func(string) bool
	switch op {
	case "=":
		test = func(s string) bool { return strings.EqualFold(s, value) }
	case "!=", "<>":
		test = func(s string) bool { return !strings.EqualFold(s, value) }
	case "like":
		test = likeMatcher(value)
	case "not like":
		like := likeMatcher(value)
		test = func(s string) bool { return !like(s) }
	default:
		return nil, unsupported()
	}
	return func(name, tableType string) bool { return test(field(name, tableType)) }, nil
}

// likeMatcher compiles a MySQL LIKE pattern: % matches any run of characters,
// _ exactly one, a backslash escapes the next character, and the comparison is
// case-insensitive like the default collation.
func likeMatcher(pattern string) func(string) bool {
	var re strings.Builder
	re.WriteString("(?is)^")
	escaped := false
	for _, r := range pattern {
		switch {
		case escaped:
			re.WriteString(regexp.QuoteMeta(string(r)))
			escaped = false
		case r == '\\':
			escaped = true
		case r == '%':
			re.WriteString(".*")
		case r == '_':
			re.WriteString(".")
		default:
			re.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if escaped {
		re.WriteString(`\\`)
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString
}

// handleShowVariables answers SHOW [SESSION | GLOBAL] VARIABLES [LIKE
// 'pattern' | WHERE Variable_name = 'name' | WHERE Variable_name IN (...)]
// from the fixed session variables, with the read-only overrides applied.
// The filter matters: a client that asks for one variable reads the first
// row it gets, whatever its name.
func handleShowVariables(sql string, readOnly bool) (*mysql.Result, error) {
	match := func(string) bool { return true }
	toks := lexTokens(strings.TrimRight(strings.TrimSpace(sql), ";"))
	for i, t := range toks {
		switch {
		case t.is(tokWord, "like") && i+1 < len(toks) && toks[i+1].kind == tokString:
			match = likeMatcher(toks[i+1].text)
		case t.is(tokWord, "where"):
			if names := variableNamesIn(toks[i+1:]); len(names) > 0 {
				match = func(name string) bool { return slices.Contains(names, name) }
			}
		}
	}
	var values [][]any
	for _, v := range sessionVariables {
		if match(v.name) {
			values = append(values, []any{v.name, fmt.Sprint(variableValue(v.name, v.value, readOnly))})
		}
	}
	return resultOf([]string{"Variable_name", "Value"}, values)
}

// variableNamesIn collects the names a SHOW VARIABLES WHERE clause compares
// Variable_name with, through = or IN (...), lowercased.
func variableNamesIn(toks []token) []string {
	var names []string
	for j := 0; j+2 < len(toks); j++ {
		if !strings.EqualFold(toks[j].text, "variable_name") || (toks[j].kind != tokWord && toks[j].kind != tokQuoted) {
			continue
		}
		switch {
		case toks[j+1].is(tokOp, "=") && toks[j+2].kind == tokString:
			names = append(names, strings.ToLower(toks[j+2].text))
		case toks[j+1].is(tokWord, "in") && toks[j+2].is(tokOp, "("):
			for k := j + 3; k < len(toks) && !toks[k].is(tokOp, ")"); k++ {
				if toks[k].kind == tokString {
					names = append(names, strings.ToLower(toks[k].text))
				}
			}
		}
	}
	return names
}

func handleShowStatus(lower string) (*mysql.Result, error) {
	if strings.Contains(lower, "ssl_version") {
		rs, err := buildResultset([]string{"Variable_name", "Value"}, [][]any{{"Ssl_version", ""}})
		if err != nil {
			return nil, err
		}
		return mysql.NewResult(rs), nil
	}

	rs, err := buildResultset([]string{"Variable_name", "Value"}, [][]any{})
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func handleInformationSchema(sql string, sess localSession) (*mysql.Result, error) {
	lower := normalize(sql)

	if strings.Contains(lower, "information_schema.routines") {
		rs, err := buildResultset([]string{"function_schema", "function_name", "create_statement", "function_type"}, [][]any{})
		if err != nil {
			return nil, err
		}
		return mysql.NewResult(rs), nil
	}

	if strings.Contains(lower, "information_schema.tables") {
		return handleInfoSchemaTables(lower, sess.schema)
	}

	if strings.Contains(lower, "information_schema.collation_character_set_applicability") {
		return handleInfoSchemaTableDetails(sess.schema)
	}

	if strings.Contains(lower, "information_schema.columns") {
		return handleInfoSchemaColumns(sql, sess)
	}

	// Redash reports no indexes, keys or constraints, so the other catalog
	// tables are empty; the client still gets the columns it selected, named as
	// it named them.
	star := []string{"name"}
	if strings.Contains(lower, "information_schema.statistics") {
		star = infoSchemaStatisticsStar
	}
	return resultOf(itemNames(selectItems(sql, star)), nil)
}

// infoSchemaColumnsStar and infoSchemaStatisticsStar are the columns a SELECT
// * gets from information_schema.columns and .statistics, in the real server's
// order.
var (
	infoSchemaColumnsStar = []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME",
		"ORDINAL_POSITION", "COLUMN_DEFAULT", "IS_NULLABLE", "DATA_TYPE",
		"CHARACTER_MAXIMUM_LENGTH", "CHARACTER_OCTET_LENGTH", "NUMERIC_PRECISION",
		"NUMERIC_SCALE", "DATETIME_PRECISION", "CHARACTER_SET_NAME", "COLLATION_NAME",
		"COLUMN_TYPE", "COLUMN_KEY", "EXTRA", "PRIVILEGES", "COLUMN_COMMENT",
		"GENERATION_EXPRESSION", "SRS_ID",
	}
	infoSchemaStatisticsStar = []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "NON_UNIQUE", "INDEX_SCHEMA",
		"INDEX_NAME", "SEQ_IN_INDEX", "COLUMN_NAME", "COLLATION", "CARDINALITY",
		"SUB_PART", "PACKED", "NULLABLE", "INDEX_TYPE", "COMMENT", "INDEX_COMMENT",
		"IS_VISIBLE", "EXPRESSION",
	}
)

// handleInfoSchemaColumns answers a query over information_schema.columns,
// which is how TablePlus and DBeaver read a table's structure, from the schema
// Redash reports: one row per column of every table the WHERE clause admits,
// with the columns and aliases of the select list. Redash knows a column's
// name, type and comment, so those are filled in; nullability, defaults, keys,
// lengths and the rest are NULL rather than guessed.
func handleInfoSchemaColumns(sql string, sess localSession) (*mysql.Result, error) {
	items := selectItems(sql, infoSchemaColumnsStar)
	admit := infoSchemaFilter(sql)
	var rows [][]any
	if admit("table_schema", sess.dbName) {
		for _, t := range sess.schema {
			if !admit("table_name", t.Name) {
				continue
			}
			for ord, col := range t.Columns {
				if !admit("column_name", col.Name) {
					continue
				}
				row := make([]any, len(items))
				for j, it := range items {
					row[j] = it.eval(func(field string) any {
						return infoSchemaColumnValue(field, sess.dbName, t.Name, ord+1, col)
					})
				}
				rows = append(rows, row)
			}
		}
	}
	return resultOf(itemNames(items), rows)
}

// infoSchemaColumnValue is one field of an information_schema.columns row.
func infoSchemaColumnValue(field, dbName, table string, ordinal int, col redash.SchemaColumn) any {
	switch field {
	case "table_catalog":
		return "def"
	case "table_schema":
		return dbName
	case "table_name":
		return table
	case "column_name":
		return col.Name
	case "ordinal_position":
		return int64(ordinal)
	case "data_type", "column_type":
		if col.Type == "" {
			return nil
		}
		return col.Type
	case "column_comment":
		return col.Comment
	}
	return nil
}

// selectItem is one entry of a select list: the name the output column gets
// and the expression it is computed from.
type selectItem struct {
	name string
	expr string
}

// selectItems parses the select list of a query over a catalog table. Each
// item is named the way the real server names it: the alias when there is
// one, otherwise the item's text as written. A `*` (or `t.*`) stands for
// star, the table's own columns.
func selectItems(sql string, star []string) []selectItem {
	var items []selectItem
	for _, item := range sqltext.MySQL.SplitTopLevelCommas(selectList(sql)) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == "*" || strings.HasSuffix(item, ".*") {
			for _, s := range star {
				items = append(items, selectItem{name: s, expr: s})
			}
			continue
		}
		expr, alias := splitAlias(item)
		if alias == "" {
			alias = expr
		}
		items = append(items, selectItem{name: alias, expr: expr})
	}
	if len(items) == 0 {
		for _, s := range star {
			items = append(items, selectItem{name: s, expr: s})
		}
	}
	return items
}

func itemNames(items []selectItem) []string {
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.name
	}
	return names
}

// eval resolves the item for one row: a column of the catalog table, bare or
// qualified, through get (lowercased, so TABLE_NAME and table_name are the
// same column); a literal as itself; anything else, a function call or a
// CASE, is NULL.
func (it selectItem) eval(get func(field string) any) any {
	toks := lexTokens(it.expr)
	isIdent := func(t token) bool { return t.kind == tokWord || t.kind == tokQuoted }
	switch {
	case len(toks) == 1 && isIdent(toks[0]):
		return get(strings.ToLower(toks[0].text))
	case len(toks) == 3 && isIdent(toks[0]) && toks[1].is(tokOp, ".") && isIdent(toks[2]):
		return get(strings.ToLower(toks[2].text))
	case len(toks) == 1 && toks[0].kind == tokString:
		return toks[0].text
	case len(toks) == 1 && toks[0].kind == tokNumber:
		if n, err := strconv.ParseInt(toks[0].text, 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(toks[0].text, 64); err == nil {
			return f
		}
	}
	return nil
}

// infoSchemaFilter compiles the WHERE clause of a catalog query into a
// predicate saying whether a row whose field has the given value is admitted.
// It understands `field = 'v'`, `field IN ('v', ...)` and `field LIKE 'p'` on
// table_schema, table_name and column_name, bare or qualified, joined by AND:
// the forms TablePlus, DBeaver and Connector/J use. Comparisons are
// case-insensitive like the real server's. Any other condition is ignored,
// and a clause with an OR admits everything, so a client is never answered
// with fewer rows than it asked for.
func infoSchemaFilter(sql string) func(field, value string) bool {
	accept := func(string, string) bool { return true }
	red := strings.ToLower(sqltext.MySQL.Redact(sql))
	at := -1
	for i, depth := 0, 0; i < len(red) && at < 0; i++ {
		switch red[i] {
		case '(':
			depth++
		case ')':
			depth--
		case 'w':
			if depth == 0 && tokenAt(red, i, "where") {
				at = i + len("where")
			}
		}
	}
	if at < 0 {
		return accept
	}

	conds := map[string][]func(string) bool{}
	toks := lexTokens(sql[at:])
	isIdent := func(t token) bool { return t.kind == tokWord || t.kind == tokQuoted }
	for i := 0; i < len(toks); i++ {
		if toks[i].is(tokWord, "or") {
			return accept
		}
		if !isIdent(toks[i]) {
			continue
		}
		field := strings.ToLower(toks[i].text)
		j := i + 1
		if j+1 < len(toks) && toks[j].is(tokOp, ".") && isIdent(toks[j+1]) {
			field = strings.ToLower(toks[j+1].text)
			j += 2
		}
		if j >= len(toks) {
			break
		}
		switch field {
		case "table_schema", "table_name", "column_name":
		default:
			continue
		}
		var test func(string) bool
		switch {
		case toks[j].is(tokOp, "=") && j+1 < len(toks) && toks[j+1].kind == tokString:
			v := toks[j+1].text
			test = func(s string) bool { return strings.EqualFold(s, v) }
			i = j + 1
		case toks[j].is(tokWord, "like") && j+1 < len(toks) && toks[j+1].kind == tokString:
			test = likeMatcher(toks[j+1].text)
			i = j + 1
		case toks[j].is(tokWord, "in") && j+1 < len(toks) && toks[j+1].is(tokOp, "("):
			var values []string
			k := j + 2
			for ; k < len(toks) && !toks[k].is(tokOp, ")"); k++ {
				if toks[k].kind == tokString {
					values = append(values, toks[k].text)
				}
			}
			test = func(s string) bool {
				for _, v := range values {
					if strings.EqualFold(s, v) {
						return true
					}
				}
				return false
			}
			i = k
		default:
			continue
		}
		conds[field] = append(conds[field], test)
	}
	return func(field, value string) bool {
		for _, test := range conds[field] {
			if !test(value) {
				return false
			}
		}
		return true
	}
}

// resultOf is a text result set with the given columns and rows.
func resultOf(names []string, rows [][]any) (*mysql.Result, error) {
	if rows == nil {
		rows = [][]any{}
	}
	rs, err := buildResultset(names, rows)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func handleInfoSchemaTables(lower string, schema []redash.SchemaTable) (*mysql.Result, error) {
	if strings.Contains(lower, "data_length") || strings.Contains(lower, "total_size") {
		values := make([][]any, len(schema))
		for i, t := range schema {
			values[i] = []any{t.Name, nil, nil, nil, nil}
		}
		rs, err := buildResultset([]string{"name", "comment", "data_size", "index_size", "total_size"}, values)
		if err != nil {
			return nil, err
		}
		return mysql.NewResult(rs), nil
	}

	values := make([][]any, len(schema))
	for i, t := range schema {
		values[i] = []any{t.Name, "BASE TABLE"}
	}
	rs, err := buildResultset([]string{"table_name", "table_type"}, values)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func handleInfoSchemaTableDetails(schema []redash.SchemaTable) (*mysql.Result, error) {
	values := make([][]any, len(schema))
	for i, t := range schema {
		values[i] = []any{"utf8mb4", "utf8mb4_general_ci", "InnoDB", t.Name, -1}
	}
	rs, err := buildResultset([]string{"charset", "collation", "engine", "name", "estimated_row"}, values)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func singleResult(colName string, value any) (*mysql.Result, error) {
	rs, err := buildResultset([]string{colName}, [][]any{{value}})
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

// tokKind classifies a lexed token; see lexTokens.
type tokKind int

const (
	tokWord   tokKind = iota // identifier or keyword; text is lowercased
	tokVar                   // @@name, @@session.name or @name; text is lowercased
	tokNumber                // numeric literal
	tokString                // '...' or "..."; text is the unquoted value
	tokQuoted                // `...`; text is the unquoted identifier
	tokOp                    // punctuation or operator
)

type token struct {
	kind tokKind
	text string // normalized form: lowercased word/var, unquoted string/identifier
	orig string // as written
	pos  int    // byte offset in the source
}

func (t token) is(kind tokKind, text string) bool {
	return t.kind == kind && t.text == text
}

// aliasText is the token as a column or database name: an identifier keeps the
// case it was written in, a quoted one its contents.
func (t token) aliasText() string {
	if t.kind == tokWord {
		return t.orig
	}
	return t.text
}

// lexTokens splits MySQL text into tokens, dropping comments. It knows just
// enough lexical structure for the statements answered locally: quoting and
// escaping rules, @@variables, and the two-byte comparison operators.
func lexTokens(s string) []token {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '#' || (c == '-' && i+1 < len(s) && s[i+1] == '-'):
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			if end := strings.Index(s[i+2:], "*/"); end < 0 {
				i = len(s)
			} else {
				i += 2 + end + 2
			}
		case c == '\'' || c == '"':
			text, end := lexQuoted(s, i, true)
			toks = append(toks, token{kind: tokString, text: text, orig: s[i:end], pos: i})
			i = end
		case c == '`':
			text, end := lexQuoted(s, i, false)
			toks = append(toks, token{kind: tokQuoted, text: text, orig: s[i:end], pos: i})
			i = end
		case c == '@':
			end := i + 1
			if end < len(s) && s[end] == '@' {
				end++
			}
			for end < len(s) && (isIdentByte(s[end]) || s[end] == '.') {
				end++
			}
			toks = append(toks, token{kind: tokVar, text: strings.ToLower(s[i:end]), orig: s[i:end], pos: i})
			i = end
		case c >= '0' && c <= '9':
			end := i
			for end < len(s) && (isIdentByte(s[end]) || s[end] == '.') {
				end++
			}
			toks = append(toks, token{kind: tokNumber, text: s[i:end], orig: s[i:end], pos: i})
			i = end
		case isIdentByte(c):
			end := i
			for end < len(s) && isIdentByte(s[end]) {
				end++
			}
			toks = append(toks, token{kind: tokWord, text: strings.ToLower(s[i:end]), orig: s[i:end], pos: i})
			i = end
		default:
			end := i + 1
			if end < len(s) && strings.IndexByte("<>!", c) >= 0 && strings.IndexByte("<>=", s[end]) >= 0 {
				end++ // <>, !=, <=, >=
			}
			toks = append(toks, token{kind: tokOp, text: s[i:end], orig: s[i:end], pos: i})
			i = end
		}
	}
	return toks
}

// lexQuoted reads the quoted region opening at s[i] and returns its unquoted
// contents and the index just past the closing quote. A doubled quote stands
// for one; with backslashEscapes (string literals, not `identifiers`) the
// usual MySQL escapes are decoded, except \% and \_, which LIKE needs intact.
func lexQuoted(s string, i int, backslashEscapes bool) (string, int) {
	q := s[i]
	var b strings.Builder
	j := i + 1
	for j < len(s) {
		c := s[j]
		switch {
		case backslashEscapes && c == '\\' && j+1 < len(s):
			switch next := s[j+1]; next {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '0':
				b.WriteByte(0)
			case 'b':
				b.WriteByte('\b')
			case 'Z':
				b.WriteByte(26)
			case '%', '_':
				b.WriteByte('\\')
				b.WriteByte(next)
			default:
				b.WriteByte(next)
			}
			j += 2
		case c == q:
			if j+1 < len(s) && s[j+1] == q {
				b.WriteByte(q)
				j += 2
				continue
			}
			return b.String(), j + 1
		default:
			b.WriteByte(c)
			j++
		}
	}
	return b.String(), j // unterminated: the rest of the text is the value
}

func buildResult(sql string, result *redash.QueryResult) (*mysql.Result, error) {
	lower := strings.ToLower(strings.TrimSpace(sql))

	if strings.HasPrefix(lower, "insert") || strings.HasPrefix(lower, "update") ||
		strings.HasPrefix(lower, "delete") || strings.HasPrefix(lower, "create") ||
		strings.HasPrefix(lower, "drop") || strings.HasPrefix(lower, "alter") {
		return &mysql.Result{
			AffectedRows: uint64(len(result.Rows)),
		}, nil
	}

	if len(result.Columns) == 0 {
		return &mysql.Result{}, nil
	}

	names := make([]string, len(result.Columns))
	for i, col := range result.Columns {
		names[i] = col.Name
	}

	// go-mysql's BuildSimpleResultset infers each column's MySQL type from the
	// first non-null value and rejects the whole result if a later value in that
	// column maps to a different Go type. Decide each column's representation once,
	// across all rows, so a single overflowing/odd value can never poison it.
	kinds := make([]colKind, len(result.Columns))
	for j, col := range result.Columns {
		kinds[j] = columnKind(col, result.Rows)
	}

	values := make([][]any, len(result.Rows))
	for i, row := range result.Rows {
		rowValues := make([]any, len(result.Columns))
		for j, col := range result.Columns {
			val, ok := row[col.Name]
			if !ok || val == nil {
				rowValues[j] = nil
				continue
			}
			rowValues[j] = convertValue(val, col.Type, kinds[j])
		}
		values[i] = rowValues
	}

	rs, err := buildResultset(names, values)
	if err != nil {
		return nil, fmt.Errorf("building result set: %w", err)
	}
	return mysql.NewResult(rs), nil
}

// colKind is the single Go representation chosen for every value in a column.
type colKind int

const (
	kindString colKind = iota // emit values as text (VAR_STRING); always exact
	kindInt                   // emit values as int64 (column typed LONGLONG)
	kindFloat                 // emit values as float64 (column typed DOUBLE)
	kindBool                  // emit values as int64 0/1
)

// columnKind picks one representation for an entire column. Integers that all fit
// int64 stay int64 (preserving the numeric type); if any value overflows, the
// column degrades to text so precision is never lost. Floats stay float64 only
// when every value round-trips exactly through a binary double; high-precision
// DECIMALs degrade to text instead of being silently corrupted.
func columnKind(col redash.Column, rows []map[string]any) colKind {
	switch col.Type {
	case "boolean":
		// A nested JSON value is serialized to a string by convertValue, which would
		// mix with the int64 emitted for real booleans; degrade the whole column to
		// text so every value is consistently typed.
		for _, row := range rows {
			switch row[col.Name].(type) {
			case map[string]any, []any:
				return kindString
			}
		}
		return kindBool
	case "integer":
		for _, row := range rows {
			v := row[col.Name]
			if v == nil {
				continue
			}
			if _, ok := asInt64(v); !ok {
				return kindString
			}
		}
		return kindInt
	case "float":
		for _, row := range rows {
			v := row[col.Name]
			if v == nil {
				continue
			}
			if _, ok := asExactFloat64(v); !ok {
				return kindString
			}
		}
		return kindFloat
	default:
		return kindString
	}
}

func convertValue(val any, redashType string, kind colKind) any {
	// Nested JSON values are always serialized to a JSON string regardless of the
	// declared column type.
	switch val.(type) {
	case map[string]any, []any:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}

	switch kind {
	case kindBool:
		return boolTo01(val)
	case kindInt:
		if n, ok := asInt64(val); ok {
			return n
		}
		return stringifyValue(val, redashType)
	case kindFloat:
		if f, ok := asExactFloat64(val); ok {
			return f
		}
		return stringifyValue(val, redashType)
	default:
		return stringifyValue(val, redashType)
	}
}

func asInt64(val any) (int64, bool) {
	switch v := val.(type) {
	case json.Number:
		i, err := v.Int64()
		return i, err == nil
	case float64:
		return int64(v), v == math.Trunc(v)
	}
	return 0, false
}

// asExactFloat64 reports whether val is a number with few enough significant
// digits to be represented exactly by a binary float64 (15 decimal digits is the
// guaranteed-round-trip threshold for IEEE-754 doubles).
func asExactFloat64(val any) (float64, bool) {
	switch v := val.(type) {
	case json.Number:
		if significantDigits(v.String()) > 15 {
			return 0, false
		}
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	}
	return 0, false
}

func significantDigits(num string) int {
	count := 0
	for i := 0; i < len(num); i++ {
		c := num[i]
		if c == 'e' || c == 'E' {
			break // exponent does not add significand digits
		}
		if c >= '0' && c <= '9' {
			count++
		}
	}
	return count
}

func boolTo01(val any) int64 {
	switch v := val.(type) {
	case bool:
		if v {
			return 1
		}
		return 0
	case json.Number:
		if i, err := v.Int64(); err == nil && i != 0 {
			return 1
		}
		if f, err := v.Float64(); err == nil && f != 0 {
			return 1
		}
		return 0
	case string:
		lower := strings.ToLower(v)
		if lower == "true" || lower == "1" || lower == "t" {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func stringifyValue(val any, redashType string) string {
	if n, ok := val.(json.Number); ok {
		// Preserve the exact decimal text (no float64 round-trip).
		return n.String()
	}
	if redashType == "datetime" {
		s := fmt.Sprintf("%v", val)
		s = strings.Replace(s, "T", " ", 1)
		return strings.TrimSuffix(s, "Z")
	}
	return fmt.Sprintf("%v", val)
}

// buildResultset is mysql.BuildSimpleResultset with one correction. go-mysql
// takes a byte-slice view of each string value and sends a nil slice as SQL
// NULL, and its view of "" is nil, so every empty text value, from a Redash
// row or from SHOW VARIABLES, reached the client as NULL. An empty []byte is
// a zero-length string on the wire, so "" is sent as that.
func buildResultset(names []string, values [][]any) (*mysql.Resultset, error) {
	for _, row := range values {
		for i, v := range row {
			if s, ok := v.(string); ok && s == "" {
				row[i] = []byte{}
			}
		}
	}
	return mysql.BuildSimpleResultset(names, values, false)
}
