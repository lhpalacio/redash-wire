package pgwire

import (
	"regexp"
	"strings"

	"github.com/lhpalacio/redash-wire/internal/sqltext"
)

// timestampInWhereRe matches quoted-column = 'timestamp-literal' patterns where
// the timestamp carries 1-3 fractional-second digits: the millisecond precision
// Redash returns and the only case where truncation loss can occur. Whole-second
// literals are intentionally NOT matched, since rewriting them with date_trunc
// would widen exact equality and could match (and delete) extra rows. A zone
// suffix (Z, +HH, +HH:MM) is accepted because that is how the proxy itself
// renders a timestamptz, and a GUI round-trips the value it was shown.
var timestampInWhereRe = regexp.MustCompile(
	`"([^"]+)"\s*=\s*'(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{1,3}(?:Z|[+-]\d{2}(?::\d{2})?)?)'`,
)

// RewriteTimestampComparisons wraps timestamp equality comparisons in DML WHERE
// clauses with date_trunc('milliseconds', ...) so that Redash's millisecond-
// truncated values match the database's microsecond-precision stored values.
//
// Only UPDATE and DELETE statements are rewritten, and only the WHERE clause is
// modified; the SET clause is left untouched.
func RewriteTimestampComparisons(sql string) string {
	trimmed := strings.TrimSpace(sql)
	lower := strings.ToLower(trimmed)

	if !strings.HasPrefix(lower, "update") && !strings.HasPrefix(lower, "delete") {
		return sql
	}

	// Locate WHERE in the redacted text so a " where " inside a string literal in
	// the SET clause is not mistaken for the clause boundary.
	whereIdx := strings.Index(strings.ToLower(sqltext.Postgres.Redact(sql)), " where ")
	if whereIdx == -1 {
		return sql
	}

	prefix := sql[:whereIdx+7]
	whereClause := sql[whereIdx+7:]

	rewritten := timestampInWhereRe.ReplaceAllString(
		whereClause,
		`date_trunc('milliseconds', "$1") = '$2'`,
	)

	if rewritten == whereClause {
		return sql
	}

	return prefix + rewritten
}
