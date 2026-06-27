package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// idCount is one (group-key, count) row of a GROUP BY result.
type idCount struct {
	id    int64
	count int64
}

// queryIDCounts runs q and returns its (BIGINT, BIGINT) rows sorted by id.
func queryIDCounts(t *testing.T, db *sql.DB, ctx context.Context, q string) []idCount {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []idCount
	for rows.Next() {
		var ic idCount
		if err := rows.Scan(&ic.id, &ic.count); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		out = append(out, ic)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// PIN 1 (Graefe-required, GRAEFE-2 guard-relaxation DANGER case). The probe-fed
// exception in compensationSafeForYield is PER-ALIAS: a leg's outer-correlated
// residual is only kept on the leg when the leg's OWN probe binds that alias.
//
//	SELECT o.id FROM o, t, bb WHERE t.fk = o.id AND t.xb = bb.v
//
// The t-leg probes o (via t.fk = o.id), so o IS probe-bound. But the second
// predicate t.xb = bb.v is correlated to bb, which the o-probe does NOT bind.
// The per-alias guard must keep bb.v OUT of the t-leg filter and apply it where
// bb is bound (the bb-join). If the relaxation were all-or-nothing — allowing the
// bb.v residual onto the t-leg merely because the leg has SOME probe-fed
// correlation (o) — bb would be unbound there and the join would yield 0 rows.
// This is the inverse control to TestFDB_CompositeJoinDrivesProbeSide (there the
// residual's outer alias IS probe-bound → kept on the leg). Must be 2 rows.
func TestFDB_MultiOuterResidual_NotDroppedToUnboundLeg(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_multiresid")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_multiresid")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE multiresid_tmpl "+
			"CREATE TABLE o (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE t (id BIGINT NOT NULL, fk BIGINT, xb BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE bb (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_multiresid/s WITH TEMPLATE multiresid_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_multiresid?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO o VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o VALUES (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO bb VALUES (1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO bb VALUES (2, 200)")
	// t10: fk=1→o1, xb=100→bb(v=100)  MATCH (o.id=1)
	// t11: fk=2→o2, xb=200→bb(v=200)  MATCH (o.id=2)
	// t12: fk=1→o1, xb=999→no bb       no match
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (10, 1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (11, 2, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (12, 1, 999)")

	const q = "SELECT o.id FROM o, t, bb WHERE t.fk = o.id AND t.xb = bb.v"
	got := queryIDs(t, db, ctx, q)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("%q: got %v, want [1 2] (2 rows) — the bb.v residual was applied on a leg where bb is unbound → 0/wrong rows", q, got)
	}
}

// PIN 2 (Graefe-required, condition #5: the GROUP-BY COUNT-value sentinel for the
// dropped-residual bug, now resolved by conjunct-flatten). Pins the COUNT VALUE,
// not a plan string (the original false-alarm was a truncated `[N preds]` string).
//
//	SELECT o.id, COUNT(*) FROM o, t WHERE t.fk = o.id AND t.k = 5 GROUP BY o.id
//
// o1 has three t-rows (one with k=5, two without); o2 has one t-row with k=5. The
// t.k = 5 residual must survive into the join so each group counts ONLY its k=5
// rows: o1 → 1, o2 → 1. If the residual were dropped, o1 would over-count its
// non-k=5 rows (→ 3). Asserts the exact (id, count) pairs.
func TestFDB_GroupByCount_ResidualNotDropped(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gbcount")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gbcount")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gbcount_tmpl "+
			"CREATE TABLE o (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE t (id BIGINT NOT NULL, fk BIGINT, k BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gbcount/s WITH TEMPLATE gbcount_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_gbcount?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO o VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o VALUES (2)")
	// o1: three rows, only one with k=5
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (10, 1, 5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (11, 1, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (12, 1, 9)")
	// o2: one row with k=5
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (13, 2, 5)")

	const q = "SELECT o.id, COUNT(*) FROM o, t WHERE t.fk = o.id AND t.k = 5 GROUP BY o.id"
	got := queryIDCounts(t, db, ctx, q)
	want := []idCount{{1, 1}, {2, 1}}
	if len(got) != len(want) {
		t.Fatalf("%q: got %v, want %v", q, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%q: row %d got (id=%d,count=%d), want (id=%d,count=%d) — a dropped t.k=5 residual over-counts the non-k=5 rows",
				q, i, got[i].id, got[i].count, want[i].id, want[i].count)
		}
	}
}
