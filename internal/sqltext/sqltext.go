// Package sqltext provides lightweight, dependency-free SQL text analysis shared
// by the PostgreSQL and MySQL wire layers: splitting a request into statements,
// redacting string-literal/comment contents before keyword matching, and
// whole-identifier token matching. It is deliberately not a full SQL parser; it
// only understands enough lexical structure (quotes, dollar-quoting, comments)
// to stop classifiers from matching text inside literals or comments.
package sqltext

import "strings"

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// codeMask returns a slice the same length as sql where mask[i] is true only when
// byte i is "code": outside any string literal, quoted identifier, dollar-quoted
// string, or comment. Used for classification and statement splitting.
func codeMask(sql string) []bool {
	return scanMask(sql, true)
}

// scanMask builds the code mask. When protectIdentifierQuotes is true, double-quote
// and backtick identifier regions are treated as non-code (suitable for keyword
// classification and statement splitting). When false, only single-quoted strings,
// dollar-quoted strings, and comments are non-code, so identifier qualifiers, which
// live inside `...`/"...", remain replaceable.
func scanMask(sql string, protectIdentifierQuotes bool) []bool {
	n := len(sql)
	mask := make([]bool, n)
	i := 0
	for i < n {
		c := sql[i]

		if c == '-' && i+1 < n && sql[i+1] == '-' {
			for i < n && sql[i] != '\n' {
				i++
			}
			continue
		}

		// Block comment: /* ... */ (not nested, matching SQL standard)
		if c == '/' && i+1 < n && sql[i+1] == '*' {
			i += 2
			for i < n {
				if sql[i] == '*' && i+1 < n && sql[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			continue
		}

		if c == '\'' || (protectIdentifierQuotes && (c == '"' || c == '`')) {
			quote := c
			i++
			for i < n {
				// MySQL (default, non-ANSI_QUOTES sql_mode) processes backslash escapes
				// inside both '...' and "..." string literals; backtick identifiers do not.
				if (quote == '\'' || quote == '"') && sql[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if sql[i] == quote {
					if i+1 < n && sql[i+1] == quote {
						i += 2 // doubled-quote escape ('' "" ``)
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}

		// Dollar-quoted string (PostgreSQL): $tag$ ... $tag$
		if c == '$' {
			j := i + 1
			for j < n && isIdentByte(sql[j]) {
				j++
			}
			if j < n && sql[j] == '$' {
				tag := sql[i : j+1]
				rest := i + len(tag)
				idx := strings.Index(sql[rest:], tag)
				if idx < 0 {
					i = n // unterminated: treat remainder as literal
					continue
				}
				i = rest + idx + len(tag)
				continue
			}
			// Not a dollar quote (e.g. a $1 placeholder): treat as code.
			mask[i] = true
			i++
			continue
		}

		mask[i] = true
		i++
	}
	return mask
}

// Statements splits sql into individual statements on top-level semicolons,
// ignoring semicolons inside quotes, dollar-quoting, and comments. Segments that
// carry no actual code (only whitespace and/or comments) are dropped, so a
// trailing ';' or a trailing comment does not count as another statement.
func Statements(sql string) []string {
	mask := codeMask(sql)
	var stmts []string
	start := 0
	flush := func(end int) {
		if segmentHasCode(sql, mask, start, end) {
			stmts = append(stmts, strings.TrimSpace(sql[start:end]))
		}
	}
	for i := 0; i < len(sql); i++ {
		if sql[i] == ';' && mask[i] {
			flush(i)
			start = i + 1
		}
	}
	flush(len(sql))
	return stmts
}

func segmentHasCode(sql string, mask []bool, start, end int) bool {
	for i := start; i < end; i++ {
		if mask[i] {
			switch sql[i] {
			case ' ', '\t', '\n', '\r':
			default:
				return true
			}
		}
	}
	return false
}

// IsMultiStatement reports whether sql contains more than one non-empty statement.
func IsMultiStatement(sql string) bool {
	return len(Statements(sql)) > 1
}

// Redact replaces the contents of string literals, quoted identifiers,
// dollar-quoted strings, and comments with spaces, leaving code bytes intact.
// Length and line structure are preserved. Use it before keyword/identifier
// matching so a match cannot fire inside a literal or comment.
func Redact(sql string) string {
	mask := codeMask(sql)
	b := make([]byte, len(sql))
	for i := 0; i < len(sql); i++ {
		if mask[i] {
			b[i] = sql[i]
		} else if sql[i] == '\n' {
			b[i] = '\n'
		} else {
			b[i] = ' '
		}
	}
	return string(b)
}

// ReplaceOutsideStrings replaces every occurrence of old with replacement, but only
// where the match lies entirely outside single-quoted string literals, dollar-quoted
// strings, and comments. Backtick/double-quoted identifier regions ARE eligible,
// which is where a database qualifier like `db`. or "db". appears, so callers can
// strip such a qualifier without mutating text inside an actual string value.
// old must be non-empty.
func ReplaceOutsideStrings(sql, old, replacement string) string {
	if old == "" {
		return sql
	}
	mask := scanMask(sql, false)
	var b strings.Builder
	i := 0
	for i < len(sql) {
		if i+len(old) <= len(sql) && sql[i:i+len(old)] == old && allTrue(mask[i:i+len(old)]) {
			b.WriteString(replacement)
			i += len(old)
			continue
		}
		b.WriteByte(sql[i])
		i++
	}
	return b.String()
}

func allTrue(bs []bool) bool {
	for _, v := range bs {
		if !v {
			return false
		}
	}
	return true
}

// SplitTopLevelCommas splits s on commas that are outside string literals, quoted
// identifiers, dollar-quoting, and comments, and at parenthesis depth zero, so a
// comma inside a function call (e.g. format_type(a, b)) or a literal does not split
// the item. Used to parse a SELECT list into individual select items.
func SplitTopLevelCommas(s string) []string {
	mask := codeMask(s)
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		if !mask[i] {
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// ContainsToken reports whether tok appears in s bounded by non-identifier bytes,
// so "pg_class" matches "from pg_class" but not "pg_classification_rules". When
// tok ends in a non-identifier byte (e.g. "version(" or "pg_catalog."), the right
// boundary is not enforced. Callers should pass an already-lowercased s and tok.
func ContainsToken(s, tok string) bool {
	if tok == "" {
		return false
	}
	from := 0
	for {
		idx := strings.Index(s[from:], tok)
		if idx < 0 {
			return false
		}
		idx += from
		end := idx + len(tok)
		leftOK := idx == 0 || !isIdentByte(s[idx-1])
		rightOK := end == len(s) || !isIdentByte(tok[len(tok)-1]) || !isIdentByte(s[end])
		if leftOK && rightOK {
			return true
		}
		from = idx + 1
	}
}
