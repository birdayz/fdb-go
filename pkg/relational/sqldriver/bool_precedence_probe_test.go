package sqldriver_test

// Pins that logical operators have NO precedence either — like arithmetic,
// `AND XOR OR` are one left-associative `logicalOperator` grammar alternative, so
// `a OR b AND c` evaluates as `(a OR b) AND c`, NOT the standard-SQL
// `a OR (b AND c)`. CONFORMANT with Java (shared grammar) — explicitly
// cross-engine-pinned by the plandiff corpus entry "or_and_precedence_left_to_right"
// (Go == Java on the distinguishing row). Use parentheses for AND-before-OR.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_BoolPrecedenceProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_boolprec")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_boolprec")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE boolprec CREATE TABLE t (id BIGINT NOT NULL, a BOOLEAN, b BOOLEAN, c BOOLEAN, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_boolprec/s WITH TEMPLATE boolprec")
	dsn := fmt.Sprintf("fdbsql:///testdb_boolprec?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a=T, b=F, c=F
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, c) VALUES (1, true, false, false)")

	matches := func(where string) bool {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			t.Fatalf("query WHERE %s: %v", where, err)
		}
		defer rows.Close()
		return rows.Next()
	}

	t.Run("or_and_is_left_to_right", func(t *testing.T) {
		// (a OR b) AND c = (T OR F) AND F = F → no match. (a OR (b AND c) would be T.)
		if matches("a OR b AND c") {
			t.Errorf("a OR b AND c matched; left-to-right (a OR b) AND c must be FALSE (a=T,b=F,c=F)")
		}
	})
	t.Run("parens_force_and_first", func(t *testing.T) {
		// a OR (b AND c) = T OR F = T → match.
		if !matches("a OR (b AND c)") {
			t.Errorf("a OR (b AND c) did not match; should be TRUE (a=T)")
		}
	})
	t.Run("and_then_or_unaffected", func(t *testing.T) {
		// b AND c OR a = ((b AND c) OR a) = (F OR T) = T → match (AND textually first).
		if !matches("b AND c OR a") {
			t.Errorf("b AND c OR a did not match; (F AND F) OR T = T")
		}
	})
	t.Run("not_binds_to_atom", func(t *testing.T) {
		// NOT b AND a = (NOT b) AND a = T AND T = T → match.
		if !matches("NOT b AND a") {
			t.Errorf("NOT b AND a did not match; (NOT F) AND T = T")
		}
	})
}
