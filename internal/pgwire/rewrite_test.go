package pgwire

import "testing"

func TestRewriteTimestampComparisons(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "UPDATE with timestamp in WHERE",
			input:    `UPDATE "public"."customers" SET "name" = 'new' WHERE "id" = 1 AND "created_at" = '2026-03-27 19:56:34.792'`,
			expected: `UPDATE "public"."customers" SET "name" = 'new' WHERE "id" = 1 AND date_trunc('milliseconds', "created_at") = '2026-03-27 19:56:34.792'`,
		},
		{
			// Whole-second literals are NOT rewritten: date_trunc would widen exact
			// equality and could match (and delete) rows with sub-second precision.
			name:     "UPDATE with timestamp no fractional seconds is left unchanged",
			input:    `UPDATE "t" SET "x" = 1 WHERE "ts" = '2026-03-27 19:56:34'`,
			expected: `UPDATE "t" SET "x" = 1 WHERE "ts" = '2026-03-27 19:56:34'`,
		},
		{
			name:     "WHERE keyword inside a SET literal is not the clause boundary",
			input:    `UPDATE "t" SET "note" = 'x where y' WHERE "ts" = '2026-03-27 19:56:34.792'`,
			expected: `UPDATE "t" SET "note" = 'x where y' WHERE date_trunc('milliseconds', "ts") = '2026-03-27 19:56:34.792'`,
		},
		{
			name:     "UPDATE with 1 fractional digit",
			input:    `UPDATE "t" SET "x" = 1 WHERE "ts" = '2026-01-01 00:00:00.5'`,
			expected: `UPDATE "t" SET "x" = 1 WHERE date_trunc('milliseconds', "ts") = '2026-01-01 00:00:00.5'`,
		},
		{
			name:     "DELETE with timestamp",
			input:    `DELETE FROM "t" WHERE "created_at" = '2026-03-27 10:00:00.123'`,
			expected: `DELETE FROM "t" WHERE date_trunc('milliseconds', "created_at") = '2026-03-27 10:00:00.123'`,
		},
		{
			name:     "multiple timestamp columns",
			input:    `UPDATE "t" SET "x" = 1 WHERE "created_at" = '2026-03-27 10:00:00.100' AND "updated_at" = '2026-03-27 11:00:00.200'`,
			expected: `UPDATE "t" SET "x" = 1 WHERE date_trunc('milliseconds', "created_at") = '2026-03-27 10:00:00.100' AND date_trunc('milliseconds', "updated_at") = '2026-03-27 11:00:00.200'`,
		},
		{
			name:     "does not rewrite SET clause",
			input:    `UPDATE "t" SET "ts" = '2026-03-27 10:00:00.123' WHERE "id" = 1`,
			expected: `UPDATE "t" SET "ts" = '2026-03-27 10:00:00.123' WHERE "id" = 1`,
		},
		{
			name:     "does not rewrite 6-digit fractional seconds",
			input:    `UPDATE "t" SET "x" = 1 WHERE "ts" = '2026-03-27 10:00:00.123456'`,
			expected: `UPDATE "t" SET "x" = 1 WHERE "ts" = '2026-03-27 10:00:00.123456'`,
		},
		{
			name:     "does not rewrite SELECT",
			input:    `SELECT * FROM "t" WHERE "ts" = '2026-03-27 10:00:00.123'`,
			expected: `SELECT * FROM "t" WHERE "ts" = '2026-03-27 10:00:00.123'`,
		},
		{
			name:     "does not rewrite non-timestamp strings",
			input:    `UPDATE "t" SET "x" = 1 WHERE "name" = 'hello' AND "id" = 1`,
			expected: `UPDATE "t" SET "x" = 1 WHERE "name" = 'hello' AND "id" = 1`,
		},
		{
			name:     "no WHERE clause",
			input:    `UPDATE "t" SET "x" = 1`,
			expected: `UPDATE "t" SET "x" = 1`,
		},
		{
			name:     "mixed timestamp and non-timestamp in WHERE",
			input:    `UPDATE "public"."customers" SET "name" = 'test' WHERE "id" = 1 AND "name" = 'old' AND "created_at" = '2026-03-27 19:56:34.792'`,
			expected: `UPDATE "public"."customers" SET "name" = 'test' WHERE "id" = 1 AND "name" = 'old' AND date_trunc('milliseconds', "created_at") = '2026-03-27 19:56:34.792'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RewriteTimestampComparisons(tt.input)
			if got != tt.expected {
				t.Errorf("\n  input:    %s\n  expected: %s\n  got:      %s", tt.input, tt.expected, got)
			}
		})
	}
}
