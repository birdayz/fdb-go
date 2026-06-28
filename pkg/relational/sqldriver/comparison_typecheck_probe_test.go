package sqldriver_test

// Probes type checking in comparisons. INT vs STRING is rejected 42804 ("operands
// not compatible") with NO implicit string↔numeric coercion (even `n = '5'`), in
// both literal and column-column forms. INT vs DOUBLE across columns evaluates
// correctly via the residual path (n=5 = d=5.0 → match — the residual of the
// documented cross-type-SARG gap; result is correct, only the index SARG is
// affected). BOOLEAN vs INT (`flag = 1`) neither errors nor coerces 1≡TRUE — it
// returns no match (distinct types); `flag = TRUE` is the correct form.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ComparisonTypecheckProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ctc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ctc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE ctc CREATE TABLE t (id BIGINT NOT NULL, n BIGINT, s STRING, flag BOOLEAN, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ctc/s WITH TEMPLATE ctc")
	dsn := fmt.Sprintf("fdbsql:///testdb_ctc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, n, s, flag, d) VALUES (1, 5, '5', true, 5.0)")

	count := func(where string) (int, error) {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE "+where)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n, nil
	}
	rejected := func(name, where string) {
		t.Run(name, func(t *testing.T) {
			if _, err := count(where); err == nil || !strings.Contains(err.Error(), "42804") {
				t.Errorf("%s error = %v, want 42804 (incompatible comparison operands)", where, err)
			}
		})
	}
	rejected("int_vs_nonnumeric_string", "n = 'abc'")
	rejected("int_vs_numeric_string", "n = '5'") // no implicit string→int coercion
	rejected("string_vs_int", "s = 5")
	rejected("string_vs_int_columns", "s = n")

	t.Run("int_vs_double_columns_residual_ok", func(t *testing.T) {
		// n=5, d=5.0 → residual cmpAny coerces → match (correct result; SARG gap is
		// about the index probe, not correctness — see TODO cross-type SARG).
		c, err := count("n = d")
		if err != nil {
			t.Fatalf("n = d: %v", err)
		}
		if c != 1 {
			t.Errorf("n = d matched %d, want 1 (5 = 5.0)", c)
		}
	})
	t.Run("bool_vs_bool_ok", func(t *testing.T) {
		if c, _ := count("flag = TRUE"); c != 1 {
			t.Errorf("flag = TRUE matched %d, want 1", c)
		}
	})
	t.Run("bool_vs_int_no_coercion", func(t *testing.T) {
		// CURRENT behavior: bool vs int neither errors nor coerces 1≡TRUE — no match.
		// (Asymmetric with int/string's 42804; use `flag = TRUE`.)
		c, err := count("flag = 1")
		if err != nil {
			// if it ever starts rejecting (42804), that's a consistency improvement —
			// update this assertion.
			t.Logf("flag = 1 now errors (%v) — bool/int rejected like int/string", err)
			return
		}
		if c != 0 {
			t.Errorf("flag = 1 matched %d; current behavior is no-match (no 1≡TRUE coercion)", c)
		}
	})
}
