package sqldriver_test

// Arithmetic OVERFLOW inside a WHERE predicate is evaluated per-row, so whether
// a query raises 22003 depends on whether the plan evaluates the arithmetic for
// an overflowing row at all — a plan that filters a selective conjunct FIRST
// never computes `a+b` for the rows it excludes. That is acceptable SQL
// (evaluation order is not guaranteed; Postgres behaves the same). What must NOT
// happen is plan-dependent NON-DETERMINISM: the SAME query returning rows under
// one projection and 22003 under another. This test pins that determinism.
//
// (It also documents why the RFC-182 rowdiff harness generates only SUBTRACTION
// for arithmetic leaves: subtraction can't overflow over the value domain, but
// +/* can, and the harness's full-scan oracle evaluates the arithmetic for
// EVERY row — so on `g=20 AND (a+b)>0` the oracle would overflow while a
// filter-first engine plan returns rows, a false divergence. The oracle cannot
// model the engine's plan-driven evaluation order, so overflowing operators are
// out of scope for the differential by construction, not by omission.)
import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ArithOverflowInPredicate_PlanStable(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	const p62 = int64(1) << 62 // 2^62 + 2^62 = 2^63 overflows int64
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aovfp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aovfp")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE aovfp "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, g BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX idx_g ON t (g)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aovfp/s WITH TEMPLATE aovfp")
	dsn := fmt.Sprintf("fdbsql:///testdb_aovfp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO t (id,a,b,g) VALUES (1,%d,%d,10),(2,1,1,20),(3,%d,%d,30)", p62, p62, p62, p62))

	// outcome returns "22003", "ok" (rows, no overflow), or "err:<...>".
	outcome := func(q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			func() {
				defer rows.Close()
				for rows.Next() {
				}
				err = rows.Err()
			}()
		}
		switch {
		case err == nil:
			return "ok"
		case strings.Contains(err.Error(), "22003"):
			return "22003"
		default:
			return "err:" + err.Error()
		}
	}

	// SOUNDNESS INVARIANT: a query's overflow outcome must not depend on the
	// projection (which changes the plan). Whatever the engine does for
	// `g=20 AND (a+b)>0`, it must do it under EVERY projection.
	base := "FROM t WHERE g = 20 AND (a + b) > 0"
	projections := []string{"id", "*", "g", "a, b", "id, g"}
	first := outcome("SELECT " + projections[0] + " " + base)
	for _, p := range projections[1:] {
		if got := outcome("SELECT " + p + " " + base); got != first {
			t.Errorf("plan-dependent NON-DETERMINISM: `SELECT %s %s` → %q but `SELECT %s %s` → %q; "+
				"an overflow outcome must not flip with the projection",
				projections[0], base, first, p, base, got)
		}
	}
	// For today's cost model the filter-first plan wins, so the selective-
	// conjunct query avoids the overflow. Not a hard guarantee (SQL permits
	// either), but pin it so a change is noticed and reasoned about.
	if first != "ok" {
		t.Logf("NOTE: `%s` now yields %q (was \"ok\" — filter-first plan). "+
			"Acceptable per SQL, but confirm the cost-model change is intended.", base, first)
	}

	// And the un-guarded arithmetic genuinely overflows (the data really does
	// exceed int64), so the invariant above isn't vacuous.
	if got := outcome("SELECT id FROM t WHERE (a + b) > 0"); got != "22003" {
		t.Errorf("`(a + b) > 0` over 2^62 data must raise 22003, got %q", got)
	}
}
