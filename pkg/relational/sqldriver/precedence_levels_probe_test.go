package sqldriver_test

// Pins that operator-precedence LEVELS are correct even though intra-level
// precedence is flat (left-to-right). The level order is:
//   arithmetic/bitwise  (tightest)  <  comparison  <  logical  (loosest)
// so `5 = a + 1` parses as `5 = (a+1)` (NOT the left-to-right `(5=a)+1`, which
// would be a bool+int error), and `a = 5 OR b = 2` parses as `(a=5) OR (b=2)`.
// Only WITHIN the arithmetic class (* vs +) and WITHIN the logical class (AND vs
// OR) is evaluation left-to-right — see arith_precedence_probe / bool_precedence_probe.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_PrecedenceLevelsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_preclvl")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_preclvl")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE preclvl CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_preclvl/s WITH TEMPLATE preclvl")
	dsn := fmt.Sprintf("fdbsql:///testdb_preclvl?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b) VALUES (1, 4, 2)") // a=4, b=2

	matches := func(where string) bool {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			t.Fatalf("query WHERE %s: %v", where, err)
		}
		defer rows.Close()
		return rows.Next()
	}
	ck := func(name, where string, want bool) {
		t.Run(name, func(t *testing.T) {
			if got := matches(where); got != want {
				t.Errorf("WHERE %s matched=%v, want %v", where, got, want)
			}
		})
	}

	// arithmetic binds tighter than comparison (else (5=a)+1 → type error).
	ck("arith_tighter_than_cmp_lhs", "a + 1 = 5", true) // (a+1)=5 = 5=5
	ck("arith_tighter_than_cmp_rhs", "5 = a + 1", true) // 5=(a+1)
	ck("arith_cmp_false", "a = 1 + 4", false)           // 4 = 5
	ck("mult_tighter_than_cmp", "8 = a * 2", true)      // 8=(a*2)=8
	// comparison binds tighter than logical.
	ck("cmp_tighter_than_or", "a = 5 OR b = 2", true)   // (a=5)F OR (b=2)T = T
	ck("cmp_tighter_than_and", "a = 4 AND b = 2", true) // (a=4)T AND (b=2)T = T
	ck("cmp_and_false", "a = 5 AND b = 2", false)       // F AND T = F
	// full chain: arithmetic + comparison + logical compose correctly.
	ck("full_level_chain", "a + 1 = 5 AND b * 2 = 4", true) // ((a+1)=5)T AND ((b*2)=4)T
}
