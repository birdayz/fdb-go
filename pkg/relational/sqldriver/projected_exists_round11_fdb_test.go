package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// TestFDB_ProjectedExistsRound11 pins RFC-141 R4 round-11, the silent-wrong bug
// review found in the round-10 predicate-routing fix.
//
// Round-10 routed a non-EXISTS predicate BELOW the FirstOrDefault iff it
// referenced ANY non-outer correlation. But an UNCORRELATED SCALAR SUBQUERY in a
// predicate (`price > (SELECT MAX(x) FROM t2)`) carries its own
// ScalarSubqueryValue alias — a non-outer correlation that is NOT an inner table
// leg (it is a pre-evaluated external binding). The absence test pushed the
// scalar predicate below the FOD; alongside an EMPTY NOT-EXISTS the FOD yields
// NULL, its IS-NULL residual admits every outer row, and the scalar comparison
// (pushed below) never runs → the scalar predicate is silently dropped and rows
// that should fail `price > MAX(x)` are returned anyway.
//
// Root fix: route by POSITIVE membership in the existential inner's
// FROM-source-alias set (innerLegs). A scalar-subquery / parameter alias is not
// an inner leg, so it stays OUTER and the comparison actually filters the outer
// row; the round-10 multi-table fix is preserved (all inner legs route below).
//
// Data:
//
//	t1: (id, price). prices 10,20,30,40.
//	t2: x in {15}            → (SELECT MAX(x) FROM t2) = 15.
//	t3: fk in {1,3}          → NOT EXISTS (t3.fk = t1.id) is TRUE for ids {2,4}
//	                           (empty inner) and FALSE for ids {1,3}.
//
// `price > 15 AND NOT EXISTS(...)` :
//
//	id 1: price 10, t3 match     → price>15 FALSE, NOT EXISTS FALSE → excluded
//	id 2: price 20, no t3 match  → price>15 TRUE,  NOT EXISTS TRUE  → INCLUDED
//	id 3: price 30, t3 match     → price>15 TRUE,  NOT EXISTS FALSE → excluded
//	id 4: price 40, no t3 match  → price>15 TRUE,  NOT EXISTS TRUE  → INCLUDED
//	⇒ {2,4}.
//
// Before the fix the scalar predicate was dropped, so the result was every row
// whose NOT EXISTS was TRUE: {2,4} happens to coincide here — so the dataset is
// designed so the scalar ALSO excludes one of those (id with price <= 15 AND no
// t3 match). See p1_scalar_excludes_a_notexists_true below: id 0 (price 10, no t3
// match) is NOT-EXISTS-TRUE but price 10 <= 15, so it must be EXCLUDED. A dropped
// scalar would wrongly INCLUDE it.
func TestFDB_ProjectedExistsRound11(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_pexr11")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_pexr11")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pexr11_tmpl "+
		"CREATE TABLE t1 (id BIGINT NOT NULL, price BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t2 (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t3 (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pexr11/s WITH TEMPLATE pexr11_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_pexr11?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// id 0 (price 10, NO t3 match) is the discriminator: NOT-EXISTS TRUE but
	// price 10 <= 15 → must be excluded by the scalar. A dropped scalar includes it.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (0, 10), (1, 10), (2, 20), (3, 30), (4, 40)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 15)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (1000, 1), (3000, 3)")

	queryInts := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eqInts := func(got, want []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	type idBool struct {
		id int64
		e  bool
	}
	queryIDBool := func(t *testing.T, q string) []idBool {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []idBool
		for rows.Next() {
			var r idBool
			if err := rows.Scan(&r.id, &r.e); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
	}

	// ---- THE ROUND-11 BUG: scalar subquery predicate alongside NOT-EXISTS ----

	// Discriminator query. The scalar excludes id 0 (price 10 <= 15) which is
	// NOT-EXISTS-TRUE. A dropped scalar would wrongly include id 0.
	t.Run("p1_scalar_predicate_with_not_exists_empty_inner", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 "+
			"WHERE price > (SELECT MAX(x) FROM t2) "+
			"AND NOT EXISTS (SELECT 1 FROM t3 WHERE t3.fk = t1.id)")
		if want := []int64{2, 4}; !eqInts(got, want) {
			t.Errorf("scalar predicate was dropped (round-11 bug): got %v, want %v", got, want)
		}
	})

	// Same shape with EXISTS (not NOT-EXISTS). The NOT-EXISTS path is the one that
	// silently drops the scalar (empty FOD → IS-NULL residual admits the row before
	// the below-FOD scalar runs), but the EXISTS variant must also apply the scalar.
	//	price>15 AND EXISTS(t3.fk=t1.id):
	//	  id 1: price 10, t3 match  → price>15 FALSE → excluded
	//	  id 3: price 30, t3 match  → price>15 TRUE  → INCLUDED
	//	⇒ {3}.
	t.Run("p1_scalar_predicate_with_exists", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 "+
			"WHERE price > (SELECT MAX(x) FROM t2) "+
			"AND EXISTS (SELECT 1 FROM t3 WHERE t3.fk = t1.id)")
		if want := []int64{3}; !eqInts(got, want) {
			t.Errorf("scalar predicate was dropped (round-11 bug): got %v, want %v", got, want)
		}
	})

	// Scalar predicate alongside a MULTI-TABLE NOT-EXISTS inner (round-10 path):
	// the scalar must stay outer while the multi-table correlation routes below.
	//	price>15 AND NOT EXISTS(SELECT 1 FROM t3, t2 WHERE t3.fk = t1.id):
	//	  t3,t2 cross-join non-empty when t3.fk matches → NOT EXISTS TRUE for {0,2,4}.
	//	  price>15 narrows to {2,4}.
	t.Run("p1_scalar_predicate_with_multitable_not_exists", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 "+
			"WHERE price > (SELECT MAX(x) FROM t2) "+
			"AND NOT EXISTS (SELECT 1 FROM t3, t2 WHERE t3.fk = t1.id)")
		if want := []int64{2, 4}; !eqInts(got, want) {
			t.Errorf("scalar predicate was dropped (round-11 bug): got %v, want %v", got, want)
		}
	})

	// Scalar predicate alongside a single-table EXISTS, projected EXISTS boolean.
	// The projection must read the correct boolean AND the scalar must filter the
	// outer. price>15 AND projected EXISTS over t3:
	//	  id 2: price 20, no t3 match → price>15 TRUE,  EXISTS FALSE
	//	  id 3: price 30, t3 match    → price>15 TRUE,  EXISTS TRUE
	//	  id 4: price 40, no t3 match → price>15 TRUE,  EXISTS FALSE
	//	(ids 0,1 fail price>15) ⇒ {(2,false),(3,true),(4,false)}.
	t.Run("p1_scalar_predicate_with_projected_exists", func(t *testing.T) {
		got := queryIDBool(t, "SELECT id, EXISTS (SELECT 1 FROM t3 WHERE t3.fk = t1.id) AS e FROM t1 "+
			"WHERE price > (SELECT MAX(x) FROM t2)")
		want := []idBool{{2, false}, {3, true}, {4, false}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Parameter marker in a predicate alongside NOT-EXISTS: the parameter binding
	// is a non-outer, non-inner-leg correlation analog (an external bind). It must
	// stay outer and filter, exactly like the scalar subquery.
	//	price > ? (?=15) AND NOT EXISTS(t3.fk=t1.id) ⇒ {2,4}.
	t.Run("p1_parameter_marker_with_not_exists", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT id FROM t1 WHERE price > ? AND NOT EXISTS (SELECT 1 FROM t3 WHERE t3.fk = t1.id)",
			int64(15))
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		if want := []int64{2, 4}; !eqInts(out, want) {
			t.Errorf("parameter predicate dropped: got %v, want %v", out, want)
		}
	})

	// ---- Audit controls: prove the fix is narrow (each correct, none rejected) ----

	// Plain single-table NOT-EXISTS (no scalar) — unchanged.
	t.Run("audit_plain_not_exists", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t3 WHERE t3.fk = t1.id)")
		if want := []int64{0, 2, 4}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Plain single-table EXISTS (no scalar) — unchanged.
	t.Run("audit_plain_exists", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t3 WHERE t3.fk = t1.id)")
		if want := []int64{1, 3}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Outer-only column predicate alongside NOT-EXISTS — the outer column predicate
	// must still filter the outer (not routed below): price < 35 AND NOT EXISTS ⇒
	// {0,2} (ids 0,2 NOT-EXISTS-TRUE and price<35; id 4 price 40 excluded).
	t.Run("audit_outer_col_pred_with_not_exists", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE price < 35 AND NOT EXISTS (SELECT 1 FROM t3 WHERE t3.fk = t1.id)")
		if want := []int64{0, 2}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// Scalar predicate WITHOUT any EXISTS — the plain baseline that the scalar must
	// always satisfy: price > 15 ⇒ {2,3,4}.
	t.Run("audit_scalar_predicate_no_exists", func(t *testing.T) {
		got := queryInts(t, "SELECT id FROM t1 WHERE price > (SELECT MAX(x) FROM t2)")
		if want := []int64{2, 3, 4}; !eqInts(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}
