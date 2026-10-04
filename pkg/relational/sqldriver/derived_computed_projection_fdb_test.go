package sqldriver_test

// The derived-table/CTE output-column naming axis: a plain qualified-column
// passthrough (`t.a`) publishes its bare column label (`A`), while an unaliased
// computed projection (`t.a + t.b`) has an anonymous physical ordinal name
// (`_0`) and no SQL name. Exact result types carry the runtime names through the
// boundary; SQL name presence is separate. These tests pin the labels and rows,
// and require invalid references to unnamed computed outputs to fail loudly
// rather than select a wrong slot.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestFDB_DerivedComputedProjectionColumnNames(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/derivedcomputed"
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
	db, err := sql.Open("fdbsql", "fdbsql://"+strings.ToUpper(dbPath)+"?cluster_file="+clusterFilePath+"&schema=MAIN")
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
