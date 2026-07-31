package sqldriver_test

// A PROJECTED EXISTS whose correlated read names a NON-FIRST leg of a 3-way
// join. The existential's table is EMPTY, so the only correct answer is FALSE
// for every row — there is no data anywhere that could make an EXISTS true, and
// therefore no reading of the correlation, right or wrong, that justifies TRUE.
//
// The same query correlated to the FIRST leg answers FALSE, and the same
// correlation over a TWO-leg join answers FALSE. The three differ on one axis.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_ProjectedExistsCorrelatedToNonFirstLeg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4l_pxnfl"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE w4l_pxnfl_tmpl"+
		" CREATE TABLE pxa (id BIGINT, partner BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE pxb (did BIGINT, dpartner BIGINT, dk BIGINT, PRIMARY KEY (did))"+
		" CREATE TABLE pxq (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE pxe (pid BIGINT, fk BIGINT, PRIMARY KEY (pid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE w4l_pxnfl_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO pxa VALUES (1, 2, 100), (2, 1, 200)",
		"INSERT INTO pxb VALUES (1, 2, 100), (2, 1, 200)",
		"INSERT INTO pxq VALUES (1), (2)",
		// pxe is deliberately EMPTY.
	} {
		if _, sErr := db.ExecContext(ctx, stmt); sErr != nil {
			t.Fatalf("seed %q: %v", stmt, sErr)
		}
	}

	run := func(q string) []string {
		r, qErr := db.QueryContext(ctx, q)
		if qErr != nil {
			t.Fatalf("query must ANSWER: %v\n  sql: %s", qErr, q)
		}
		defer r.Close()
		var out []string
		for r.Next() {
			var id int64
			var e sql.NullBool
			if sErr := r.Scan(&id, &e); sErr != nil {
				t.Fatalf("scan: %v\n  sql: %s", sErr, q)
			}
			out = append(out, fmt.Sprintf("(%d,%v)", id, e.Valid && e.Bool))
		}
		if rErr := r.Err(); rErr != nil {
			t.Fatalf("rows: %v\n  sql: %s", rErr, q)
		}
		sort.Strings(out)
		return out
	}

	explain := func(q string) string {
		var plan string
		_ = db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan)
		return plan
	}

	const wantAllFalse = "[(1,false) (2,false)]"
	shapes := []struct{ name, sql string }{
		{
			"three-leg-second-leg-correlation",
			"SELECT a.id, EXISTS (SELECT 1 FROM pxe WHERE pxe.fk = b.dk) " +
				"FROM pxa AS a JOIN pxb AS b ON b.did = a.partner JOIN pxq ON pxq.qid = a.id",
		},
		{
			"three-leg-first-leg-correlation",
			"SELECT a.id, EXISTS (SELECT 1 FROM pxe WHERE pxe.fk = a.k) " +
				"FROM pxa AS a JOIN pxb AS b ON b.did = a.partner JOIN pxq ON pxq.qid = a.id",
		},
		{
			"two-leg-second-leg-correlation",
			"SELECT a.id, EXISTS (SELECT 1 FROM pxe WHERE pxe.fk = b.dk) " +
				"FROM pxa AS a JOIN pxb AS b ON b.did = a.partner",
		},
		{
			"three-leg-uncorrelated",
			"SELECT a.id, EXISTS (SELECT 1 FROM pxe) " +
				"FROM pxa AS a JOIN pxb AS b ON b.did = a.partner JOIN pxq ON pxq.qid = a.id",
		},
	}
	for _, tc := range shapes {
		if got := fmt.Sprint(run(tc.sql)); got != wantAllFalse {
			t.Errorf("[%s] rows = %s, want %s\n  sql: %s\n  plan: %s\n\n"+
				"  PXE IS EMPTY. An EXISTS over an empty table is FALSE for every row\n"+
				"  no matter which column the correlation reads, so a TRUE here is not a\n"+
				"  wrong-leg read — it is the existential's emptiness not reaching the\n"+
				"  fold at all.",
				tc.name, got, wantAllFalse, tc.sql, explain(tc.sql))
		}
	}

	// SEEDED TRUE CONTROLS. Every row above expects FALSE, so an executor that
	// answered FALSE unconditionally would pass all four — and "the existential's
	// emptiness reaches the fold" would be indistinguishable from "the fold is
	// stuck". The all-FALSE half only means something beside a half that can be
	// TRUE.
	//
	// One row of PXE at fk = 100 makes each shape answer DIFFERENTLY, and that is
	// the point of doing it per shape rather than once:
	//
	//   a.id=1 pairs with b.did=2 (dk 200) and carries a.k 100
	//   a.id=2 pairs with b.did=1 (dk 100) and carries a.k 200
	//
	// so the first-leg and second-leg correlations answer COMPLEMENTS of each
	// other. A read that resolves the correlation against the wrong leg does not
	// merely return the wrong count — it returns the exact inverse, on the same
	// rows, which the all-FALSE half cannot see and a single all-TRUE control
	// could not see either.
	if _, sErr := db.ExecContext(ctx, "INSERT INTO pxe VALUES (1, 100)"); sErr != nil {
		t.Fatalf("seed pxe: %v", sErr)
	}
	wantSeeded := map[string]string{
		// a=2 pairs with b.dk=100 — TRUE there and only there.
		"three-leg-second-leg-correlation": "[(1,false) (2,true)]",
		// a=1 carries k=100 — the COMPLEMENT of the row above.
		"three-leg-first-leg-correlation": "[(1,true) (2,false)]",
		// Same correlation as the first shape, one leg fewer: the answer must not
		// depend on how many legs the merged row happens to carry.
		"two-leg-second-leg-correlation": "[(1,false) (2,true)]",
		// No correlation at all: PXE is non-empty, so every row is TRUE. This is
		// the row an unconditional FALSE cannot survive.
		"three-leg-uncorrelated": "[(1,true) (2,true)]",
	}
	for _, tc := range shapes {
		want := wantSeeded[tc.name]
		if got := fmt.Sprint(run(tc.sql)); got != want {
			t.Errorf("[%s seeded] rows = %s, want %s\n  sql: %s\n  plan: %s\n\n"+
				"  PXE now holds one row at fk = 100. The four shapes answer four\n"+
				"  different things, and the two correlated three-leg shapes answer\n"+
				"  COMPLEMENTS — so a correlation resolved against the wrong leg returns\n"+
				"  the exact inverse on the same rows. If this row is all-FALSE while the\n"+
				"  half above passed, the existential fold is stuck rather than reading\n"+
				"  the wrong leg.",
				tc.name, got, want, tc.sql, explain(tc.sql))
		}
	}
}
