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

	const wantAllFalse = "[(1,false) (2,false)]"
	for _, tc := range []struct{ name, sql string }{
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
	} {
		if got := fmt.Sprint(run(tc.sql)); got != wantAllFalse {
			var plan string
			_ = db.QueryRowContext(ctx, "EXPLAIN "+tc.sql).Scan(&plan)
			t.Errorf("[%s] rows = %s, want %s\n  sql: %s\n  plan: %s\n\n"+
				"  PXE IS EMPTY. An EXISTS over an empty table is FALSE for every row\n"+
				"  no matter which column the correlation reads, so a TRUE here is not a\n"+
				"  wrong-leg read — it is the existential's emptiness not reaching the\n"+
				"  fold at all.",
				tc.name, got, wantAllFalse, tc.sql, plan)
		}
	}
}
