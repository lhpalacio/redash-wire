package mysqlwire

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

// normalize lowercases a statement after redacting string-literal and comment
// contents, so classification never matches inside a literal or comment.
func normalize(sql string) string {
	return strings.ToLower(strings.TrimSpace(sqltext.Redact(sql)))
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

	if strings.HasPrefix(lower, "select ") && !strings.Contains(lower, " from ") {
		patterns := []string{
			"version()", "database()", "current_user()", "user()",
			"connection_id()", "@@version", "@@version_comment",
			"@@max_allowed_packet", "@@character_set_client",
			"@@character_set_connection", "@@character_set_results",
			"@@collation_connection", "@@init_connect",
			"@@interactive_timeout", "@@license", "@@lower_case_table_names",
			"@@net_write_timeout", "@@sql_mode", "@@system_time_zone",
			"@@time_zone", "@@tx_isolation", "@@transaction_isolation",
			"@@wait_timeout",
		}
		for _, p := range patterns {
			if sqltext.ContainsToken(lower, p) {
				return true
			}
		}
	}

	return false
}

func handleLocalQuery(sql string, dbName string, sources []redash.DataSource, schema []redash.SchemaTable) (*mysql.Result, error) {
	lower := normalize(sql)

	if strings.HasPrefix(lower, "set ") ||
		strings.HasPrefix(lower, "begin") || strings.HasPrefix(lower, "start transaction") ||
		strings.HasPrefix(lower, "commit") || strings.HasPrefix(lower, "rollback") {
		return nil, nil
	}

	if strings.HasPrefix(lower, "show databases") || strings.HasPrefix(lower, "show schemas") {
		return handleShowDatabases(sources)
	}

	if strings.HasPrefix(lower, "show tables") || strings.HasPrefix(lower, "show full tables") {
		return handleShowTables(lower, schema)
	}

	if strings.HasPrefix(lower, "show variables") || strings.HasPrefix(lower, "show session variables") ||
		strings.HasPrefix(lower, "show global variables") {
		return handleShowVariables()
	}

	if strings.HasPrefix(lower, "show session status") || strings.HasPrefix(lower, "show status") {
		return handleShowStatus(lower)
	}

	if strings.Contains(lower, "information_schema.") {
		return handleInformationSchema(lower, schema)
	}

	switch {
	case strings.Contains(lower, "@@version_comment"):
		return singleResult("@@version_comment", "redash-wire MySQL proxy")
	case strings.Contains(lower, "@@version"):
		return singleResult("@@version", "8.0.0-redash-wire")
	case strings.Contains(lower, "version()"):
		return singleResult("version()", "8.0.0-redash-wire")
	case strings.Contains(lower, "database()"):
		if dbName == "" {
			return singleResult("database()", nil)
		}
		return singleResult("database()", dbName)
	case strings.Contains(lower, "current_user()"), strings.Contains(lower, "user()"):
		return singleResult("user()", "redash@localhost")
	case strings.Contains(lower, "connection_id()"):
		return singleResult("connection_id()", 1)
	case strings.Contains(lower, "@@max_allowed_packet"):
		return singleResult("@@max_allowed_packet", 67108864)
	case strings.Contains(lower, "@@character_set_client"):
		return singleResult("@@character_set_client", "utf8mb4")
	case strings.Contains(lower, "@@character_set_connection"):
		return singleResult("@@character_set_connection", "utf8mb4")
	case strings.Contains(lower, "@@character_set_results"):
		return singleResult("@@character_set_results", "utf8mb4")
	case strings.Contains(lower, "@@collation_connection"):
		return singleResult("@@collation_connection", "utf8mb4_general_ci")
	case strings.Contains(lower, "@@sql_mode"):
		return singleResult("@@sql_mode", "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION")
	case strings.Contains(lower, "@@time_zone"):
		return singleResult("@@time_zone", "SYSTEM")
	case strings.Contains(lower, "@@system_time_zone"):
		return singleResult("@@system_time_zone", "UTC")
	case strings.Contains(lower, "@@tx_isolation"), strings.Contains(lower, "@@transaction_isolation"):
		return singleResult("@@transaction_isolation", "REPEATABLE-READ")
	case strings.Contains(lower, "@@wait_timeout"):
		return singleResult("@@wait_timeout", 28800)
	case strings.Contains(lower, "@@interactive_timeout"):
		return singleResult("@@interactive_timeout", 28800)
	case strings.Contains(lower, "@@net_write_timeout"):
		return singleResult("@@net_write_timeout", 60)
	case strings.Contains(lower, "@@lower_case_table_names"):
		return singleResult("@@lower_case_table_names", 0)
	case strings.Contains(lower, "@@license"):
		return singleResult("@@license", "GPL")
	case strings.Contains(lower, "@@init_connect"):
		return singleResult("@@init_connect", "")
	}

	return singleResult("result", "")
}

func handleShowDatabases(sources []redash.DataSource) (*mysql.Result, error) {
	values := make([][]any, len(sources))
	for i, ds := range sources {
		values[i] = []any{ds.Name}
	}
	rs, err := mysql.BuildSimpleResultset([]string{"Database"}, values, false)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func handleShowTables(lower string, schema []redash.SchemaTable) (*mysql.Result, error) {
	isFull := strings.HasPrefix(lower, "show full tables")

	if isFull {
		values := make([][]any, len(schema))
		for i, t := range schema {
			values[i] = []any{t.Name, "BASE TABLE"}
		}
		rs, err := mysql.BuildSimpleResultset([]string{"Tables", "Table_type"}, values, false)
		if err != nil {
			return nil, err
		}
		return mysql.NewResult(rs), nil
	}

	values := make([][]any, len(schema))
	for i, t := range schema {
		values[i] = []any{t.Name}
	}
	rs, err := mysql.BuildSimpleResultset([]string{"Tables"}, values, false)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func handleShowVariables() (*mysql.Result, error) {
	vars := [][2]string{
		{"character_set_client", "utf8mb4"},
		{"character_set_connection", "utf8mb4"},
		{"character_set_results", "utf8mb4"},
		{"collation_connection", "utf8mb4_general_ci"},
		{"init_connect", ""},
		{"interactive_timeout", "28800"},
		{"license", "GPL"},
		{"lower_case_table_names", "0"},
		{"max_allowed_packet", "67108864"},
		{"net_write_timeout", "60"},
		{"sql_mode", "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"},
		{"system_time_zone", "UTC"},
		{"time_zone", "SYSTEM"},
		{"transaction_isolation", "REPEATABLE-READ"},
		{"version", "8.0.0-redash-wire"},
		{"version_comment", "redash-wire MySQL proxy"},
		{"wait_timeout", "28800"},
	}

	values := make([][]any, len(vars))
	for i, v := range vars {
		values[i] = []any{v[0], v[1]}
	}
	rs, err := mysql.BuildSimpleResultset([]string{"Variable_name", "Value"}, values, false)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func handleShowStatus(lower string) (*mysql.Result, error) {
	if strings.Contains(lower, "ssl_version") {
		rs, err := mysql.BuildSimpleResultset(
			[]string{"Variable_name", "Value"},
			[][]any{{"Ssl_version", ""}},
			false,
		)
		if err != nil {
			return nil, err
		}
		return mysql.NewResult(rs), nil
	}

	rs, err := mysql.BuildSimpleResultset([]string{"Variable_name", "Value"}, [][]any{}, false)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func handleInformationSchema(lower string, schema []redash.SchemaTable) (*mysql.Result, error) {
	if strings.Contains(lower, "information_schema.routines") {
		rs, err := mysql.BuildSimpleResultset(
			[]string{"function_schema", "function_name", "create_statement", "function_type"},
			[][]any{}, false,
		)
		if err != nil {
			return nil, err
		}
		return mysql.NewResult(rs), nil
	}

	if strings.Contains(lower, "information_schema.tables") {
		return handleInfoSchemaTables(lower, schema)
	}

	if strings.Contains(lower, "information_schema.collation_character_set_applicability") {
		return handleInfoSchemaTableDetails(schema)
	}

	rs, err := mysql.BuildSimpleResultset([]string{"name"}, [][]any{}, false)
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
		rs, err := mysql.BuildSimpleResultset(
			[]string{"name", "comment", "data_size", "index_size", "total_size"},
			values, false,
		)
		if err != nil {
			return nil, err
		}
		return mysql.NewResult(rs), nil
	}

	values := make([][]any, len(schema))
	for i, t := range schema {
		values[i] = []any{t.Name, "BASE TABLE"}
	}
	rs, err := mysql.BuildSimpleResultset([]string{"table_name", "table_type"}, values, false)
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
	rs, err := mysql.BuildSimpleResultset(
		[]string{"charset", "collation", "engine", "name", "estimated_row"},
		values, false,
	)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func singleResult(colName string, value any) (*mysql.Result, error) {
	rs, err := mysql.BuildSimpleResultset([]string{colName}, [][]any{{value}}, false)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
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

	rs, err := mysql.BuildSimpleResultset(names, values, false)
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
