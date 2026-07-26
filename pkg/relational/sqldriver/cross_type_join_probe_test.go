package sqldriver_test

// Probes cross-type correlated index joins: join columns of different numeric
// types (INTEGER vs BIGINT, DOUBLE vs BIGINT, FLOAT vs BIGINT) with an index on
// the inner column, so the equi-join lowers to an index probe whose comparand
// is the outer column of a different type. A type-coercion bug in the SARG
// comparand would match the wrong rows.
//
// BIGINT=DOUBLE and BIGINT=FLOAT (column-vs-column — NEITHER side a
// compile-time constant) used to return EMPTY: the int comparand packed
// without promotion, missing the DOUBLE/FLOAT index entries entirely. Fixed
// by expr.promoteColumnColumnNumeric, which wraps the narrower-typed
// correlated operand in a values.PromoteValue toward the pair's
// values.MaximumType (Java's RelOpValue.encapsulate does the same via
// PromoteValue.inject for both sides of a RelOpValue) — the col-vs-col
// sibling of the constant-vs-column widen/narrow fixes, needed because
// neither operand's concrete value is known until each outer row is read, so
// there is no compile-time literal to retype in place.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_CrossTypeJoinProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_xtype")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_xtype")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE xtype "+
			"CREATE TABLE a (id BIGINT NOT NULL, xbig BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE bi (id BIGINT NOT NULL, yint INTEGER, PRIMARY KEY (id)) "+
			"CREATE TABLE bd (id BIGINT NOT NULL, ydbl DOUBLE, PRIMARY KEY (id)) "+
			"CREATE TABLE bf (id BIGINT NOT NULL, yflt FLOAT, PRIMARY KEY (id)) "+
			"CREATE INDEX bi_y ON bi (yint) CREATE INDEX bd_y ON bd (ydbl) CREATE INDEX bf_y ON bf (yflt)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_xtype/s WITH TEMPLATE xtype")
	dsn := fmt.Sprintf("fdbsql:///testdb_xtype?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, xbig) VALUES (1, 5), (2, 10), (3, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO bi (id, yint) VALUES (50, 5), (51, 10), (52, 99)")
	mwjoMustExec(t, db, ctx, "INSERT INTO bd (id, ydbl) VALUES (60, 5.0), (61, 7.0), (62, 99.0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO bf (id, yflt) VALUES (70, 5.0), (71, 7.0), (72, 99.0)")

	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return siScanRows(t, rows)
	}

	// BIGINT = INTEGER, index on the INTEGER side: a1(5)=bi50, a2(10)=bi51, a3(7) none.
	t.Run("bigint_eq_integer", func(t *testing.T) {
		got := pairs("SELECT a.id, bi.id FROM a JOIN bi ON a.xbig = bi.yint")
		want := []string{"1|50", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("BIGINT=INTEGER join = %v, want %v", got, want)
		}
	})

	// FIXED: BIGINT = INTEGER must still probe the index. Unlike the
	// DOUBLE/FLOAT pairs above, INT and LONG pack to IDENTICAL wire bytes
	// (pkg/fdbgo/fdb/tuple's encodeInt takes any width as int64), so
	// promoteColumnColumnNumeric deliberately does NOT wrap the INTEGER side
	// in PromoteValue (sharesIntegerWireEncoding) — wrapping it used to make
	// the SARG matcher's AccessorNamePath walk stop at the PromoteValue node
	// (it only unwraps *FieldValue chains), so valuesMatchColumn could no
	// longer identify the wrapped column against the index placeholder and
	// the point lookup degraded to a residual full scan. The plain
	// "bigint_eq_integer" subtest above would stay green through that
	// degradation (a full scan still returns correct rows) — this is the
	// one that actually proves the index fires.
	t.Run("bigint_eq_integer_uses_index", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT a.id, bi.id FROM a JOIN bi ON a.xbig = bi.yint").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "IndexScan(BI_Y") {
			t.Fatalf("expected an IndexScan(BI_Y ...) probe for a.xbig=bi.yint, got: %s", plan)
		}
	})

	// FIXED: BIGINT = DOUBLE via an index probe (index on the DOUBLE side) —
	// column-vs-column, so promoteColumnColumnNumeric wraps the outer BIGINT
	// comparand in PromoteValue(DOUBLE); the executor's coerceTupleElement
	// isn't even needed here (DOUBLE stays float64 in both the row and wire
	// domains), only PromoteValue.Evaluate's numeric coercion.
	t.Run("bigint_eq_double_uses_index", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT a.id, bd.id FROM a JOIN bd ON a.xbig = bd.ydbl").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "IndexScan(BD_Y") {
			t.Fatalf("expected an IndexScan(BD_Y ...) probe for a.xbig=bd.ydbl, got: %s", plan)
		}
	})
	t.Run("bigint_eq_double", func(t *testing.T) {
		got := pairs("SELECT a.id, bd.id FROM a JOIN bd ON a.xbig = bd.ydbl")
		want := []string{"1|60", "3|61"}
		if !eqStrSlices(got, want) {
			t.Errorf("BIGINT=DOUBLE join = %v, want %v", got, want)
		}
	})
	t.Run("double_eq_bigint_reversed", func(t *testing.T) {
		got := pairs("SELECT a.id, bd.id FROM bd JOIN a ON bd.ydbl = a.xbig")
		want := []string{"1|60", "3|61"}
		if !eqStrSlices(got, want) {
			t.Errorf("DOUBLE=BIGINT reversed join = %v, want %v", got, want)
		}
	})
	t.Run("bigint_gt_double", func(t *testing.T) {
		// a.xbig ∈ {1:5, 2:10, 3:7}; bd.ydbl ∈ {60:5.0, 61:7.0, 62:99.0}.
		// xbig > ydbl: 10>5.0, 10>7.0, 7>5.0 (99.0 excluded — nothing exceeds it;
		// 7>7.0 is false, not a strict inequality).
		got := pairs("SELECT a.id, bd.id FROM a JOIN bd ON a.xbig > bd.ydbl")
		want := []string{"2|60", "2|61", "3|60"}
		if !eqStrSlices(got, want) {
			t.Errorf("BIGINT>DOUBLE join = %v, want %v", got, want)
		}
	})

	// FIXED: BIGINT = FLOAT(32-bit) via an index probe — the same
	// column-vs-column promotion, but here coerceTupleElement's downcast to a
	// genuine Go float32 at the tuple-packing boundary IS load-bearing (the
	// row-domain convention keeps PromoteValue.Evaluate's FLOAT result as
	// float64; only the packer narrows it to match the index's 0x20 wire code).
	t.Run("bigint_eq_float_uses_index", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT a.id, bf.id FROM a JOIN bf ON a.xbig = bf.yflt").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "IndexScan(BF_Y") {
			t.Fatalf("expected an IndexScan(BF_Y ...) probe for a.xbig=bf.yflt, got: %s", plan)
		}
	})
	t.Run("bigint_eq_float", func(t *testing.T) {
		got := pairs("SELECT a.id, bf.id FROM a JOIN bf ON a.xbig = bf.yflt")
		want := []string{"1|70", "3|71"}
		if !eqStrSlices(got, want) {
			t.Errorf("BIGINT=FLOAT join = %v, want %v", got, want)
		}
	})

	// Computed correlated comparand (BIGINT arithmetic) probing an INTEGER index:
	// a.xbig + 0 = bi.yint.
	t.Run("computed_bigint_eq_integer", func(t *testing.T) {
		got := pairs("SELECT a.id, bi.id FROM a JOIN bi ON a.xbig + 0 = bi.yint")
		want := []string{"1|50", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("computed BIGINT=INTEGER join = %v, want %v", got, want)
		}
	})

	// Reverse direction (index on a side via PK is not on xbig; drive bi as outer).
	t.Run("integer_eq_bigint_reversed", func(t *testing.T) {
		got := pairs("SELECT a.id, bi.id FROM bi JOIN a ON bi.yint = a.xbig")
		want := []string{"1|50", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("INTEGER=BIGINT reversed join = %v, want %v", got, want)
		}
	})
}
