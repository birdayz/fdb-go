package sqldriver_test

// LIKE over SQL, pinned to the 4.14.2.0 outcomes measured on the live JVM
// (conformance/ws_e_probe_conformance_test.go): wildcards cross line
// terminators, `_` consumes a whole surrogate pair, the escape is validated
// only when a row evaluates the pattern, and the three escape errors carry
// Java's SQLSTATEs (22019, 2200B, 22025).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// likeIDs runs q and returns the first column of every row, or the error the
// statement raised — at Query time or while iterating.
func likeIDs(ctx context.Context, db *sql.DB, q string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const (
	likeMsgOperands = "The like operator expects string operands but was invoked with an operand of another type."
	likeMsgLength   = "The like operator expects an escape character of length 1."
	likeMsgWildcard = "The like operator rejects wildcards as the escape character."
	likeMsgSequence = "The like operator pattern requires all escape characters to be followed by a special character."
)

type likeCase struct {
	name, sql string
	want      []int64       // rows, when code is empty
	code      api.ErrorCode // expected SQLSTATE, or empty
	msg       string
}

func runLikeCases(t *testing.T, ctx context.Context, db *sql.DB, cases []likeCase) {
	t.Helper()
	for _, c := range cases {
		got, err := likeIDs(ctx, db, c.sql)
		if c.code == "" {
			if err != nil {
				t.Errorf("%s: %s: %v", c.name, c.sql, err)
			} else if !slices.Equal(got, c.want) {
				t.Errorf("%s: %s = %v, want %v", c.name, c.sql, got, c.want)
			}
			continue
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != c.code || apiErr.Message != c.msg {
			t.Errorf("%s: %s: got rows %v err %v, want %s %q", c.name, c.sql, got, err, c.code, c.msg)
		}
	}
}

func TestFDB_LikeJavaSemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	setup := openTestDB(t, "/FRL/testdb_like_java")
	mustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_like_java")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE like_java_tmpl "+
		"CREATE TYPE AS ENUM color ('RED', 'GREEN') "+
		"CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE U (id BIGINT, s STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE E (id BIGINT, c color, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_like_java/s WITH TEMPLATE like_java_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_LIKE_JAVA?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, ctx, "INSERT INTO T VALUES (1, 'abc', NULL), (2, 'a\nb', 5), (3, '\U0001D11Ex', 7), "+
		"(4, 'a%b', 1), (5, 'a_b', 2), (6, 'ab\n', 3), (7, 'a\\b', 4)")
	mustExec(t, db, ctx, "INSERT INTO E VALUES (1, 'RED'), (2, 'GREEN')")

	runLikeCases(t, ctx, db, []likeCase{
		{name: "percent crosses newline", sql: "SELECT id FROM T WHERE s LIKE 'a%b' ORDER BY id", want: []int64{2, 4, 5, 7}},
		{name: "underscore matches newline", sql: "SELECT id FROM T WHERE s LIKE 'a_b' ORDER BY id", want: []int64{2, 4, 5, 7}},
		{name: "no trailing newline tolerance", sql: "SELECT id FROM T WHERE s LIKE 'ab' ORDER BY id"},
		{name: "underscore over a surrogate pair", sql: "SELECT id FROM T WHERE s LIKE '_x' ORDER BY id", want: []int64{3}},
		{name: "escaped percent", sql: `SELECT id FROM T WHERE s LIKE 'a\%b' ESCAPE '\' ORDER BY id`, want: []int64{4}},
		{name: "escaped underscore", sql: `SELECT id FROM T WHERE s LIKE 'a\_b' ESCAPE '\' ORDER BY id`, want: []int64{5}},
		{name: "escaped escape", sql: `SELECT id FROM T WHERE s LIKE 'a\\b' ESCAPE '\' ORDER BY id`, want: []int64{7}},
		{name: "escape is percent", sql: "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE '%' ORDER BY id", code: api.ErrCodeEscapeCharacterConflict, msg: likeMsgWildcard},
		{name: "escape is underscore", sql: "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE '_' ORDER BY id", code: api.ErrCodeEscapeCharacterConflict, msg: likeMsgWildcard},
		{name: "escape of two chars", sql: "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE 'ab' ORDER BY id", code: api.ErrCodeInvalidEscapeCharacter, msg: likeMsgLength},
		{name: "empty escape", sql: "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE '' ORDER BY id", code: api.ErrCodeInvalidEscapeCharacter, msg: likeMsgLength},
		{name: "supplementary escape", sql: "SELECT id FROM T WHERE s LIKE 'a%' ESCAPE '\U0001D11E' ORDER BY id", code: api.ErrCodeInvalidEscapeCharacter, msg: likeMsgLength},
		{name: "dangling escape", sql: `SELECT id FROM T WHERE s LIKE 'a\' ESCAPE '\' ORDER BY id`, code: api.ErrCodeInvalidEscapeSequence, msg: likeMsgSequence},
		{name: "escape before an ordinary char", sql: `SELECT id FROM T WHERE s LIKE 'a\b' ESCAPE '\' ORDER BY id`, code: api.ErrCodeInvalidEscapeSequence, msg: likeMsgSequence},
		{name: "quote as the escape", sql: "SELECT id FROM T WHERE s LIKE '''%' ESCAPE '''' ORDER BY id"},
		{name: "NULL pattern with a bad escape", sql: "SELECT id FROM T WHERE s LIKE NULL ESCAPE 'ab' ORDER BY id", code: api.ErrCodeInvalidEscapeCharacter, msg: likeMsgLength},
		{name: "NULL pattern", sql: "SELECT id FROM T WHERE s LIKE NULL ORDER BY id"},
		{name: "NOT LIKE NULL", sql: "SELECT id FROM T WHERE s NOT LIKE NULL ORDER BY id"},
		{name: "NOT LIKE", sql: "SELECT id FROM T WHERE s NOT LIKE 'a%' ORDER BY id", want: []int64{3}},
		{name: "numeric operand", sql: "SELECT id FROM T WHERE n LIKE '1' ORDER BY id", code: api.ErrCodeInvalidArgumentForFunction, msg: likeMsgOperands},
		{name: "numeric pattern", sql: "SELECT id FROM T WHERE s LIKE 1 ORDER BY id", code: api.ErrCodeInvalidArgumentForFunction, msg: likeMsgOperands},
		{name: "enum operand", sql: "SELECT id FROM E WHERE c LIKE 'R%' ORDER BY id", code: api.ErrCodeInvalidArgumentForFunction, msg: likeMsgOperands},
		{name: "empty pattern", sql: "SELECT id FROM T WHERE s LIKE '' ORDER BY id"},
		{name: "adjacent literal pattern", sql: "SELECT id FROM T WHERE s LIKE 'a' '%' ORDER BY id", want: []int64{1, 2, 4, 5, 6, 7}},
		// The escape is validated when a row evaluates the pattern: nothing
		// raises over an empty table or rows filtered out first, while a
		// NULL operand still evaluates the pattern and raises.
		{name: "bad escape over an empty table", sql: "SELECT id FROM U WHERE s LIKE 'a%' ESCAPE 'ab'"},
		{name: "dangling escape over an empty table", sql: `SELECT id FROM U WHERE s LIKE 'a\' ESCAPE '\'`},
		{name: "bad escape over filtered-out rows", sql: "SELECT id FROM T WHERE id < 0 AND s LIKE 'a%' ESCAPE 'ab'"},
		{name: "bad escape with a NULL operand", sql: "SELECT id FROM T WHERE CAST(NULL AS STRING) LIKE 'a%' ESCAPE 'ab'", code: api.ErrCodeInvalidEscapeCharacter, msg: likeMsgLength},
	})

	// The ESCAPE token must be a string literal.
	for _, q := range []string{
		"SELECT id FROM T WHERE s LIKE 'a' ESCAPE 4",
		"SELECT id FROM T WHERE s LIKE 'a' ESCAPE NULL",
		"SELECT id FROM T WHERE s LIKE ?",
	} {
		var apiErr *api.Error
		if _, err := likeIDs(ctx, db, q); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeSyntaxError {
			t.Errorf("%s: want a syntax error, got %v", q, err)
		}
	}

	// EXPLAIN plans a bad escape without evaluating it.
	explain, err := db.QueryContext(ctx, "EXPLAIN SELECT id FROM T WHERE s LIKE 'a%' ESCAPE 'ab'")
	if err != nil {
		t.Fatalf("EXPLAIN over a bad escape: %v", err)
	}
	n := 0
	for explain.Next() {
		n++
	}
	if err := explain.Err(); err != nil || n == 0 {
		t.Fatalf("EXPLAIN over a bad escape: %d rows, %v", n, err)
	}
	explain.Close()

	// LIKE projects a boolean.
	rows, err := db.QueryContext(ctx, "SELECT id, s LIKE 'a%' FROM T ORDER BY id")
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int64
		var b sql.NullBool
		if err := rows.Scan(&id, &b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprintf("%d:%v", id, b.Bool))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("projection rows: %v", err)
	}
	if want := []string{"1:true", "2:true", "3:false", "4:true", "5:true", "6:true", "7:true"}; !slices.Equal(got, want) {
		t.Fatalf("projection = %v, want %v", got, want)
	}
}

// TestFDB_LikeEscape_MapPath runs the escape rules over INFORMATION_SCHEMA,
// whose WHERE clause goes through the same typed predicate evaluator.
func TestFDB_LikeEscape_MapPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	setup := openTestDB(t, "/FRL/testdb_like_esc_map")
	mustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_like_esc_map")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE like_esc_map_tmpl "+
		"CREATE TABLE Z (id BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE ZQ (id BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_like_esc_map/s WITH TEMPLATE like_esc_map_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_LIKE_ESC_MAP?cluster_file=%s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// INFORMATION_SCHEMA.TABLES spans the whole cluster; scope every probe to
	// this test's catalog so the row set is deterministic.
	names := func(pred string) ([]string, error) {
		rows, err := db.QueryContext(ctx, `SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES" `+
			`WHERE TABLE_CATALOG = '/FRL/TESTDB_LIKE_ESC_MAP' AND `+pred+` ORDER BY TABLE_NAME`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	for _, c := range []struct {
		pred string
		want []string
	}{
		{"TABLE_NAME LIKE 'ZZ' ESCAPE 'Z'", []string{"Z"}},
		{"TABLE_NAME NOT LIKE 'ZZ' ESCAPE 'Z'", []string{"ZQ"}},
		{"TABLE_NAME LIKE 'Z_' ESCAPE 'Z'", nil},
		{"TABLE_NAME LIKE 'Z_'", []string{"ZQ"}},
	} {
		if got, err := names(c.pred); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s = %v (%v), want %v", c.pred, got, err, c.want)
		}
	}
	// A dangling escape is Java's 22025 (like.yamsql, NOT LIKE 'Z' ESCAPE 'Z').
	var apiErr *api.Error
	if got, err := names("TABLE_NAME NOT LIKE 'Z' ESCAPE 'Z'"); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidEscapeSequence {
		t.Errorf("dangling escape: got %v (%v), want 22025", got, err)
	}
}
