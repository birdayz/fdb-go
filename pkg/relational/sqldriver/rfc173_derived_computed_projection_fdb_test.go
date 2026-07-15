package sqldriver_test

// RFC-173 item C — the derived-table/CTE output-column naming axis. The qualifier
// strip in derivedOutputColumns / projectionOutputNames (`t.a` → `A`) fires ONLY
// on a plain qualified-column passthrough: those carry a DOTTED projection name.
// A COMPUTED projection (`t.a + t.b`) carries an anonymous ordinal name (`_0`),
// no dot, so the strip never touches it and the output column is NOT mangled to
// the last dotted leaf. This pins that axis (the strip is scoped by the naming,
// not by a name-text heuristic that could shred a computed expression): computed
// derived projections keep their anonymous name and flow correct rows, while the
// plain passthrough correctly bares its qualifier. An invalid reference to the
// unnamed computed column is LOUD (correct-or-loud), never a silent wrong slot.

import (
	"context"
	"database/sql"
	"testing"
)

func TestFDB_RFC173C_DerivedComputedProjectionColumnNames(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173_derivedcomputed"
	setup := openTestDB(t, dbPath)
	must := func(q string) {
		if _, err := setup.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup: %v\n  %s", err, q)
		}
	}
	must("CREATE DATABASE " + dbPath)
	must("CREATE SCHEMA TEMPLATE derivedcomputed_tmpl" +
		" CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	must("CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE derivedcomputed_tmpl")
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1, 10, 100), (2, 20, 200)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// wantScalar runs a single-column query and asserts the sorted int64 values —
	// pinning that the row VALUES are correct (the computed column is not shredded
	// to a wrong slot by the strip).
	wantScalar := func(label, q string, want []int64) {
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: query errored: %v\n  sql: %s", label, err, q)
		}
		defer r.Close()
		var got []int64
		for r.Next() {
			var v int64
			if err := r.Scan(&v); err != nil {
				t.Fatalf("%s: scan: %v", label, err)
			}
			got = append(got, v)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v\n  sql: %s", label, got, want, q)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v\n  sql: %s", label, got, want, q)
			}
		}
	}

	// (A) Derived table, unaliased COMPUTED projection with qualified columns:
	// the output column keeps its anonymous name (NOT stripped to "b") and flows
	// a + b — the strip did not shred the expression.
	wantScalar("A/derived computed t.a+t.b", "SELECT * FROM (SELECT t.a + t.b FROM t) sub", []int64{110, 220})
	// (B) CTE variant, computed t.a + 1.
	wantScalar("B/cte computed t.a+1", "WITH sub AS (SELECT t.a + 1 FROM t) SELECT * FROM sub", []int64{11, 21})
	// (C) Explicitly aliased computed projection — the alias wins over any strip.
	wantScalar("C/derived aliased computed", "SELECT sub.x FROM (SELECT t.a + t.b AS x FROM t) sub", []int64{110, 220})
	// (D) Plain qualified PASSTHROUGH — the case the strip legitimately serves:
	// `t.a` flows under the BARE name A (Java getColumnName/clearQualifier).
	wantScalar("D/derived plain passthrough t.a", "SELECT sub.a FROM (SELECT t.a FROM t) sub", []int64{10, 20})

	// (E) A reference to the unnamed computed column by a fabricated name must be
	// LOUD (the column is anonymous, not "b") — never a silent wrong-slot read.
	rE, errE := db.QueryContext(ctx, "SELECT * FROM (SELECT t.a + t.b FROM t) sub WHERE sub.b > 0")
	if errE == nil {
		for rE.Next() {
		}
		errE = rE.Err()
		rE.Close()
	}
	if errE == nil {
		t.Fatal("E: referencing the unnamed computed column as sub.b must error (loud), got no error")
	}
}
