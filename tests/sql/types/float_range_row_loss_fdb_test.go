package sqltest

// A range predicate on an indexed FLOAT/DOUBLE column must select EXACTLY the
// logical row set — no losses, no phantoms — end to end through SQL against a
// real cluster.
//
// This is the SQL-visible half of a defect whose exactness proof otherwise
// lives entirely at the binder level (executor/float_ordered_range_exactness_test.go).
// The two orders a float key lives under disagree in exactly one region:
//
//	physical (FDB tuple):  negNaN < -Inf < … < -0.0 < +0.0 < … < +Inf < posNaN
//	logical  (Double.compare, the Record Layer's authority): every NaN GREATEST
//
// so the negative-NaN block sits at the BOTTOM of the key space while ranking
// at the TOP of the value domain. A `> x` predicate compiled to the single
// contiguous range [pack(x), END] therefore DROPS every negative-NaN row, and a
// `< x` predicate compiled to a range starting at the coordinate's type start
// ADDS them as phantoms. The binder answers both with a range SET
// (executor.decomposeOrderedFloatTail).
//
// Both failure directions are real and independent, so both are asserted here:
// each shape's row set is compared against an unindexed oracle carrying the
// identical rows AND against an outright expectation. A differential alone
// passes when both sides are wrong in the same way; an expectation alone
// passes when the oracle path is the one that broke.
//
// JAVA DIVERGES HERE, DELIBERATELY — AND ON THE NEGATIVE NaN ONLY. The
// narrowness matters, because a divergence license gets quoted later and the
// quotable sentence has to be the true one.
//
// Java builds exactly ONE TupleRange per scan: ScanComparisons.toTupleRange
// returns a single range (ScanComparisons.java:670-675) from a comparand passed
// through verbatim (toTupleItem, :332-343, which touches
// ByteString/EnumLite/FDBRecordVersion and nothing else), and IndexScanRange
// holds one TupleRange (IndexScanRange.java:33-52). No NaN, isFinite or
// normalization appears anywhere on that path — fdb-record-layer-core's entire
// NaN awareness is CastValue.java:139-173, explicit numeric CASTs, which no
// scan-range construction goes through.
//
// That single range is NOT the problem by itself. `dbl > 5.0` compiles to
// TupleRange((5.0), null, EXCLUSIVE, TREE_END) — buildEndpointTuple returns
// null for an absent endpoint (ScanComparisons.java:660-666) and TREE_END
// expands to the end of the subspace. POSITIVE NaN packs above +Inf and is
// still inside that subspace, so Java's index scan DOES return it, and Java
// agrees with its own full scan on every positive NaN.
//
// The divergence is the NEGATIVE NaN, and its cause is two encoders disagreeing
// about one bit. Tuple packing is sign-PRESERVING (TupleUtil's
// doubleToRawLongBits), so a negative NaN lands at the very bottom of the key
// space, below -Inf. The comparator is Double.compareTo (Comparisons.java
// :236-239, :757-761), which CANONICALIZES every NaN and ranks it greatest, so
// `NaN > 5.0` is TRUE. One contiguous range starting at 5.0 can never reach a
// row that sits below -Inf, so Java loses exactly the negative-NaN rows and
// cannot express the fix without a union of scans above the index.
//
// Go's range-set binder can express it, so it answers exactly. This is a
// read-side extension: nothing about which keys are WRITTEN changes, only which
// are read.
//
// The ladder is seedFloatOrderingLadder from the ordering-claim differential —
// every IEEE edge class with BOTH NaN signs carrying DISTINCT payloads. A
// ladder without a NEGATIVE NaN cannot express this defect at all: the positive
// NaNs are physically last and logically last, so a single-range binding serves
// them correctly and the test would pass with the defect fully present.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// floatRangeShape is one predicate and the ids it must select.
//
// The expectations follow the PREDICATE comparator (predicates.cmpAny, which
// checks IEEE equality first and then falls back to the total order with NaN
// greatest — Java's Comparisons semantics), NOT the sort comparator. Under it
// every NaN satisfies `> anything` and no NaN satisfies `< anything`.
type floatRangeShape struct {
	pred string
	want []int64 // sorted by id
	why  string
}

// Ladder recap (from floatOrderingRows):
//
//	10 = -Inf   20 = -1.5   30 = -0.0   40 = +0.0
//	50 = +1.5   60 = +Inf   70 = negNaN 5 = posNaN
var floatRangeShapes = []floatRangeShape{
	{
		pred: "e > -2.0",
		want: []int64{5, 20, 30, 40, 50, 60, 70},
		why:  "both NaN rows qualify (NaN is logically greatest); id 70 is the negative NaN, physically BELOW -Inf, and is the row a single contiguous range loses",
	},
	{
		pred: "e >= -2.0",
		want: []int64{5, 20, 30, 40, 50, 60, 70},
		why:  "same set — the inclusive endpoint changes nothing at a threshold no row sits on",
	},
	{
		pred: "e > 1.0e308",
		want: []int64{5, 60, 70},
		why:  "a threshold above every finite row: only +Inf and the two NaNs, so the lost row is 3/7ths of the answer rather than a corner of it",
	},
	{
		pred: "e < 2.0",
		want: []int64{10, 20, 30, 40, 50},
		why:  "no NaN qualifies; a low bound left at the coordinate's type start would sweep the negative-NaN block in as a PHANTOM (the opposite failure direction)",
	},
	{
		pred: "e <= 2.0",
		want: []int64{10, 20, 30, 40, 50},
		why:  "same set, inclusive endpoint",
	},
	{
		pred: "e < -1.0e308",
		want: []int64{10},
		why:  "-Inf alone — the tightest window against the negative-NaN block, one key away from it",
	},
	{
		pred: "e > -2.0 AND e < 2.0",
		want: []int64{20, 30, 40, 50},
		why:  "two-sided: one range, no NaN inside it either way — the arm that must NOT decompose",
	},
}

func floatRangeSorted(in []int64) []int64 {
	out := append([]int64(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func TestFDB_FloatRangePredicate_IsExactThroughSQL(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_frrl")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_frrl")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE frrl "+
		// fi: indexed on (a, e), so an equality on `a` binds the index and
		// leaves `e` as the coordinate the range predicate compiles onto.
		"CREATE TABLE fi (id BIGINT, e DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		// fo: the oracle — identical rows, no index, so the predicate is a
		// residual filter evaluated row by row and never becomes a key range.
		"CREATE TABLE fo (id BIGINT, e DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX fi_ae ON fi (a, e)")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_frrl/s WITH TEMPLATE frrl")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_FRRL?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	testkit.SeedFloatOrderingLadder(t, db, ctx, "fi")
	testkit.SeedFloatOrderingLadder(t, db, ctx, "fo")
	// Without this the whole file is vacuous: if the write path ever rejects
	// the non-finite values, both tables hold ordinary finite doubles and every
	// assertion below passes with the defect fully present.
	testkit.AssertFloatLadderStored(t, db, ctx, "fi")
	testkit.AssertFloatLadderStored(t, db, ctx, "fo")

	for _, shape := range floatRangeShapes {
		t.Run(strings.NewReplacer(" ", "_", ">", "gt", "<", "lt", "=", "eq", ".", "_").Replace(shape.pred), func(t *testing.T) {
			// ORDER BY id, not by e: this test is about WHICH rows come back,
			// and ordering on the float column would drag the (separate,
			// already-fixed) ordering-claim question into the failure message.
			idxQ := fmt.Sprintf("SELECT id FROM fi WHERE a = 1 AND %s ORDER BY id", shape.pred)
			refQ := fmt.Sprintf("SELECT id FROM fo WHERE a = 1 AND %s ORDER BY id", shape.pred)
			idxPlan := testkit.FloatOrderingExplain(t, db, ctx, idxQ)
			if !strings.Contains(strings.ToUpper(idxPlan), "FI_AE") {
				t.Fatalf("the indexed side did not take index FI_AE, so the predicate never "+
					"compiled to a key range and this shape proves nothing.\n  query: %s\n  plan:  %s",
					idxQ, idxPlan)
			}
			got := testkit.FloatOrderingIDs(t, db, ctx, idxQ)
			ref := testkit.FloatOrderingIDs(t, db, ctx, refQ)
			want := floatRangeSorted(shape.want)
			if !testkit.FloatOrderingSameOrder(got, want) {
				t.Errorf("indexed scan of %q returned %v, want %v — %s\n  query: %s\n  plan:  %s",
					shape.pred, got, want, shape.why, idxQ, idxPlan)
			}
			if !testkit.FloatOrderingSameOrder(ref, want) {
				t.Errorf("the UNINDEXED oracle for %q returned %v, want %v — the residual "+
					"predicate comparator disagrees with the expectation, so the differential "+
					"below is measuring the wrong thing\n  query: %s", shape.pred, ref, want, refQ)
			}
			if !testkit.FloatOrderingSameOrder(got, ref) {
				t.Errorf("DIFFERENTIAL MISMATCH on %q: indexed=%v unindexed oracle=%v\n"+
					"  idx query: %s\n  idx plan:  %s", shape.pred, got, ref, idxQ, idxPlan)
			}
		})
	}

	// A DESCENDING scan takes the other branch of the decomposition: the range
	// set is emitted in LOGICAL order for the scan direction, so the
	// negative-NaN block leads rather than trails. Reversing the concatenation
	// is a distinct thing to get wrong from producing it at all, and a test
	// that only ever scans ascending cannot tell them apart.
	t.Run("descending_scan_keeps_the_whole_row_set", func(t *testing.T) {
		q := "SELECT id FROM fi WHERE a = 1 AND e > -2.0 ORDER BY e DESC, id DESC"
		plan := testkit.FloatOrderingExplain(t, db, ctx, q)
		got := testkit.FloatOrderingIDs(t, db, ctx, q)
		want := floatRangeSorted([]int64{5, 20, 30, 40, 50, 60, 70})
		if !testkit.FloatOrderingSameOrder(floatRangeSorted(got), want) {
			t.Errorf("descending scan returned the row SET %v, want %v\n  query: %s\n  plan: %s",
				floatRangeSorted(got), want, q, plan)
		}
		// The NaN tie class is logically greatest, so descending it must lead.
		// Its internal order is free (one logical value, two payloads); its
		// POSITION is not.
		lastNaN, firstNonNaN := -1, -1
		for i, v := range got {
			if v == 5 || v == 70 {
				lastNaN = i
				continue
			}
			if firstNonNaN < 0 {
				firstNonNaN = i
			}
		}
		if firstNonNaN >= 0 && lastNaN > firstNonNaN {
			t.Errorf("descending, a NaN landed at position %d AFTER the first non-NaN at %d; "+
				"every NaN is logically greatest so the block must lead\n  rows: %v\n  plan: %s",
				lastNaN, firstNonNaN, got, plan)
		}
	})
}

// The 32-bit FLOAT carrier uses a different tuple type code (0x20 vs DOUBLE's
// 0x21) and its own NaN/Inf encodings, so a decomposition that consulted only
// the DOUBLE carrier would pass every assertion above. The ladder is built the
// way the ordering-claim float32 differential builds it (see that file for why
// the obvious `g * -1.0` route yields a POSITIVE NaN and does not work).
func TestFDB_FloatRangePredicate_IsExactThroughSQL_Float32(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_frrl32")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_frrl32")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE frrl32 "+
		"CREATE TABLE gi (id BIGINT, g FLOAT, h DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE go_ (id BIGINT, g FLOAT, h DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX gi_ag ON gi (a, g)")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_frrl32/s WITH TEMPLATE frrl32")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_FRRL32?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// A FLOAT column takes only a FLOAT (no DOUBLE_TO_FLOAT promotion, and CAST
	// of a DOUBLE NaN to FLOAT is refused), so the negative NaN is computed in
	// the FLOAT lane: 3e38*10 saturates to +Inf in float32, 3e38*-10 to -Inf,
	// and their sum is the default quiet NaN with the sign bit SET.
	for _, tbl := range []string{"gi", "go_"} {
		testkit.MustExecCtx(t, db, ctx, fmt.Sprintf(
			"INSERT INTO %s (id, g, h, a) VALUES (20, CAST(-1.5 AS FLOAT), 1.0e308, 1), (30, CAST(-0.0 AS FLOAT), 1.0e308, 1), "+
				"(40, CAST(0.0 AS FLOAT), 1.0e308, 1), (50, CAST(1.5 AS FLOAT), 1.0e308, 1), (70, CAST(1.0 AS FLOAT), 1.0e308, 1), (5, CAST(0.0 AS FLOAT), 1.0e308, 1)", tbl))
		testkit.MustExecCtx(t, db, ctx, fmt.Sprintf("UPDATE %s SET g = CAST('NaN' AS FLOAT) WHERE id = 5", tbl))
		testkit.MustExecCtx(t, db, ctx, fmt.Sprintf(
			"UPDATE %s SET g = (CAST(3.0E38 AS FLOAT) * CAST(10.0 AS FLOAT)) + (CAST(3.0E38 AS FLOAT) * CAST(-10.0 AS FLOAT)) WHERE id = 70", tbl))

		// Vacuity guard: without a stored NEGATIVE NaN the physically-first
		// block is empty and every assertion below is served by a single range.
		rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT id, g FROM %s", tbl))
		if err != nil {
			t.Fatalf("readback on %s: %v", tbl, err)
		}
		stored := map[int64]float64{}
		for rows.Next() {
			var id int64
			var g sql.NullFloat64
			if err := rows.Scan(&id, &g); err != nil {
				rows.Close()
				t.Fatalf("scan on %s: %v", tbl, err)
			}
			if g.Valid {
				stored[id] = g.Float64
			}
		}
		rows.Close()
		if v, ok := stored[70]; !ok || !math.IsNaN(v) || !math.Signbit(v) {
			t.Fatalf("%s id=70 is %v (present=%v), want a NEGATIVE NaN — this test is "+
				"vacuous without it", tbl, stored[70], ok)
		}
		if v, ok := stored[5]; !ok || !math.IsNaN(v) || math.Signbit(v) {
			t.Fatalf("%s id=5 is %v (present=%v), want a POSITIVE NaN", tbl, stored[5], ok)
		}
	}

	for _, shape := range []floatRangeShape{
		{pred: "g > -2.0", want: []int64{5, 20, 30, 40, 50, 70}, why: "both NaNs qualify; 70 is the negative one"},
		{pred: "g < 2.0", want: []int64{20, 30, 40, 50}, why: "no NaN qualifies — phantom direction"},
	} {
		t.Run(strings.NewReplacer(" ", "_", ">", "gt", "<", "lt", ".", "_").Replace(shape.pred), func(t *testing.T) {
			idxQ := fmt.Sprintf("SELECT id FROM gi WHERE a = 1 AND %s ORDER BY id", shape.pred)
			refQ := fmt.Sprintf("SELECT id FROM go_ WHERE a = 1 AND %s ORDER BY id", shape.pred)
			idxPlan := testkit.FloatOrderingExplain(t, db, ctx, idxQ)
			if !strings.Contains(strings.ToUpper(idxPlan), "GI_AG") {
				t.Fatalf("the indexed side did not take index GI_AG\n  query: %s\n  plan: %s", idxQ, idxPlan)
			}
			got := testkit.FloatOrderingIDs(t, db, ctx, idxQ)
			ref := testkit.FloatOrderingIDs(t, db, ctx, refQ)
			want := floatRangeSorted(shape.want)
			if !testkit.FloatOrderingSameOrder(got, want) {
				t.Errorf("FLOAT (32-bit) indexed scan of %q returned %v, want %v — %s\n  plan: %s",
					shape.pred, got, want, shape.why, idxPlan)
			}
			if !testkit.FloatOrderingSameOrder(got, ref) {
				t.Errorf("FLOAT (32-bit) DIFFERENTIAL MISMATCH on %q: indexed=%v oracle=%v\n  plan: %s",
					shape.pred, got, ref, idxPlan)
			}
		})
	}
}
