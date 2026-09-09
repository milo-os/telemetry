package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// migrationsDir is where the SQL files shipped to the Job's ConfigMap live.
const testMigrationsDir = "../config/clickhouse-migrations/migrations"

// TestMigrationFiles_SafeToSplit guards the migration files against the one
// way MultiStatementEnabled can go wrong: the driver splits files on ";"
// textually, so a semicolon inside a string literal, quoted identifier, or
// comment would be cut in half and sent to ClickHouse as two broken queries.
//
// This matters more than a normal syntax error because migrate marks a version
// dirty *before* running it: a migration that fails halfway leaves the database
// at "Dirty database version N" and every subsequent run refuses to proceed
// until an operator intervenes.
func TestMigrationFiles_SafeToSplit(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(testMigrationsDir, "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no migrations found in %s", testMigrationsDir)

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			content, err := os.ReadFile(file)
			require.NoError(t, err)

			lines, err := unsafeSemicolons(string(content))
			require.NoError(t, err)
			require.Empty(t, lines,
				"semicolon inside a string, quoted identifier, or comment on line(s) %v -- "+
					"the migration driver splits on \";\" and would break this file in half",
				lines)
		})
	}
}

// unsafeSemicolons returns the 1-based line numbers of semicolons in sql that
// sit inside a string literal, a quoted identifier, or a comment -- i.e. the
// ones a textual ";" split would mangle. It errors if a quote or block comment
// is left unterminated, which would swallow everything after it.
func unsafeSemicolons(sql string) ([]int, error) {
	const (
		normal = iota
		single // '...'
		double // "..."
		backtick
		lineComment
		blockComment
	)

	state, line := normal, 1
	var found []int

	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if c == '\n' {
			line++
			if state == lineComment {
				state = normal
			}
			continue
		}

		switch state {
		case normal:
			switch {
			case c == '\'':
				state = single
			case c == '"':
				state = double
			case c == '`':
				state = backtick
			case c == '#':
				state = lineComment
			case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
				state, i = lineComment, i+1
			case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
				state, i = blockComment, i+1
			}

		case single, double, backtick:
			quote := map[int]byte{single: '\'', double: '"', backtick: '`'}[state]
			switch {
			// ClickHouse accepts both backslash escapes and doubling.
			case c == '\\' && state != backtick:
				i++
			case c == quote && i+1 < len(sql) && sql[i+1] == quote:
				i++
			case c == quote:
				state = normal
			case c == ';':
				found = append(found, line)
			}

		case lineComment:
			if c == ';' {
				found = append(found, line)
			}

		case blockComment:
			switch {
			case c == '*' && i+1 < len(sql) && sql[i+1] == '/':
				state, i = normal, i+1
			case c == ';':
				found = append(found, line)
			}
		}
	}

	switch state {
	case single, double, backtick:
		return found, fmt.Errorf("unterminated quote at end of file")
	case blockComment:
		return found, fmt.Errorf("unterminated block comment at end of file")
	}
	return found, nil
}

func TestUnsafeSemicolons(t *testing.T) {
	for name, tc := range map[string]struct {
		sql  string
		want []int
	}{
		"statement separators are fine": {
			sql: "CREATE TABLE a (x String);\nCREATE TABLE b (y String);\n",
		},
		"semicolon in string literal": {
			sql:  "SELECT splitByChar(';', Body)\nFROM logs;\n",
			want: []int{1},
		},
		"semicolon in map key": {
			sql:  "ProjectId String MATERIALIZED ResourceAttributes['a;b']\n",
			want: []int{1},
		},
		"semicolon in line comment": {
			sql:  "-- drop it; then recreate\nDROP TABLE logs;\n",
			want: []int{1},
		},
		"semicolon in hash comment": {
			sql:  "# note; here\nDROP TABLE logs;\n",
			want: []int{1},
		},
		"semicolon in block comment": {
			sql:  "/* a\n   b; c */\nDROP TABLE logs;\n",
			want: []int{2},
		},
		"semicolon in quoted identifier": {
			sql:  "CREATE TABLE `weird;name` (x String);\n",
			want: []int{1},
		},
		"line comment ends at newline": {
			sql: "-- comment\nDROP TABLE logs;\n",
		},
		"doubled quote does not end the string": {
			sql:  "SELECT 'it''s ; here';\n",
			want: []int{1},
		},
		"backslash-escaped quote does not end the string": {
			sql:  "SELECT 'a\\' ; b';\n",
			want: []int{1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := unsafeSemicolons(tc.sql)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUnsafeSemicolons_Unterminated(t *testing.T) {
	for name, sql := range map[string]string{
		"quote":         "SELECT 'oops\n",
		"block comment": "/* oops\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := unsafeSemicolons(sql)
			require.Error(t, err)
		})
	}
}

// TestMigrationFiles_Unqualified keeps migrations database-agnostic: the
// connection is already scoped to CLICKHOUSE_DATABASE, so a hardcoded prefix
// would silently write to the wrong database.
func TestMigrationFiles_Unqualified(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(testMigrationsDir, "*.sql"))
	require.NoError(t, err)

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			content, err := os.ReadFile(file)
			require.NoError(t, err)
			require.NotContains(t, strings.ToLower(string(content)), "o11y.",
				"migrations must not qualify object names with a database")
		})
	}
}
