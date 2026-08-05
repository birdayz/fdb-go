package sqldriver_test

// GROUP BY and DISTINCT on a FLOAT/DOUBLE column must answer to the SAME value
// identity, and that identity is Java's.
//
// Java's streaming aggregation decides "same group" with one line —
// StreamGrouping.java:186, `!currentGroup.equals(nextGroup)` — where the
// operands are protobuf DynamicMessages built by RecordConstructorValue
// (RecordConstructorValue.java:113-140). Protobuf message equality compares a
// double field with java.lang.Double.equals, i.e. doubleToLongBits, so:
//
//	every NaN payload is ONE value  (doubleToLongBits collapses them)
//	-0.0 and +0.0 are TWO values    (their bits differ; note this is the
//	                                 OPPOSITE of IEEE `==`)
//
// Java's value DISTINCT lands in the same place by a different route:
// RecordQueryUnorderedDistinctPlan.java:109-112 dedups through
// HashSet<Key.Evaluated>, and Key.Evaluated.equals (Key.java:519-539) compares
// the RAW List<Object> — not the tuple-normalized one — so a Double element
// again goes through Double.equals.
//
// Go's DISTINCT already agrees: executor.packedDedupKey canonicalizes every NaN
// payload to one quiet NaN per width and leaves both signed zeros distinct. Go's
// GROUP BY did not — aggregateCursor.computeGroupKey packed the float VERBATIM
// into the group tuple, so two NaN payloads became two groups. Two dedup
// authorities in one engine, disagreeing on the same column.
//
// WHAT IS NOT FIXED, AND IS NOT A BUG HERE: the AGGREGATE INDEX path. A
// maintained aggregate index stores each group as its own FDB entry keyed by the
// packed grouping prefix (AtomicMutationIndexMaintainer.java:141,158), and tuple
// packing preserves the NaN payload, so two payloads are two physical entries —
// on both sides of the port. Java is itself plan-dependent here: the same GROUP
// BY answers 1 group through RecordQueryStreamingAggregationPlan and 2 through
// RecordQueryAggregateIndexPlan. Merging them in Go would mean writing key bytes
// Java does not, which is the wire line. So the divergence is reproduced, not
// repaired, and pinned below so it cannot drift into an accident.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
)

// groupNaNSeed writes the ladder. Two DISTINCT NaN payloads are the point: a
// test with one NaN row, or two rows sharing a payload, passes with the defect
// fully present.
//
// The negative NaN is obtained by arithmetic because no literal spells it —
// `(+Inf) + (-Inf)` is the invalid operation whose result is the default quiet
// NaN with the sign bit SET. The positive one is a plain literal cast, which
// yields 0x7ff8000000000000.
func groupNaNSeed(t *testing.T, db *sql.DB, ctx context.Context, tbl string) {
	t.Helper()
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO %s (id, d, a) VALUES (20, -1.5, 1), (30, -0.0, 1), (40, 0.0, 1), "+
			"(70, 1.0e308, 1), (5, CAST('NaN' AS DOUBLE), 1)", tbl))
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"UPDATE %s SET d = (d * 10.0) + (d * -10.0) WHERE id = 70", tbl))

	// Vacuity guard. Every assertion below is meaningless if the two NaN rows
	// did not land with different bit patterns.
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT id, d FROM %s", tbl))
	if err != nil {
		t.Fatalf("ladder readback on %s: %v", tbl, err)
	}
	defer rows.Close()
	stored := map[int64]float64{}
	for rows.Next() {
		var id int64
		var d sql.NullFloat64
		if err := rows.Scan(&id, &d); err != nil {
			t.Fatalf("ladder scan on %s: %v", tbl, err)
		}
		if d.Valid {
			stored[id] = d.Float64
		}
	}
	if v, ok := stored[70]; !ok || !math.IsNaN(v) || !math.Signbit(v) {
		t.Fatalf("%s id=70 is %v (present=%v), want a NEGATIVE NaN", tbl, stored[70], ok)
	}
	if v, ok := stored[5]; !ok || !math.IsNaN(v) || math.Signbit(v) {
		t.Fatalf("%s id=5 is %v (present=%v), want a POSITIVE NaN", tbl, stored[5], ok)
	}
	if math.Float64bits(stored[70]) == math.Float64bits(stored[5]) {
		t.Fatalf("%s: both NaN rows carry bit pattern %#016x; the ladder must hold TWO "+
			"DISTINCT payloads or this test cannot express the defect",
			tbl, math.Float64bits(stored[70]))
	}
	if v, ok := stored[30]; !ok || v != 0 || !math.Signbit(v) {
		t.Fatalf("%s id=30 is %v (present=%v), want -0.0", tbl, stored[30], ok)
	}
	if v, ok := stored[40]; !ok || v != 0 || math.Signbit(v) {
		t.Fatalf("%s id=40 is %v (present=%v), want +0.0", tbl, stored[40], ok)
	}
}

// floatGroup is one output row of a GROUP BY over the float column.
type floatGroup struct {
	key   float64
	valid bool
	count int64
}

func readFloatGroups(t *testing.T, db *sql.DB, ctx context.Context, q string) []floatGroup {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []floatGroup
	for rows.Next() {
		var k sql.NullFloat64
		var c int64
		if err := rows.Scan(&k, &c); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		out = append(out, floatGroup{key: k.Float64, valid: k.Valid, count: c})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", q, err)
	}
	return out
}

// summarize partitions the groups into the three classes this test reasons
// about, so a failure message can say WHICH identity rule broke.
func summarize(groups []floatGroup) (nanGroups int, nanRows int64, negZero, posZero int) {
	for _, g := range groups {
		switch {
		case !g.valid:
		case math.IsNaN(g.key):
			nanGroups++
			nanRows += g.count
		case g.key == 0 && math.Signbit(g.key):
			negZero++
		case g.key == 0 && !math.Signbit(g.key):
			posZero++
		}
	}
	return
}

func TestFDB_FloatGroupByNaNAuthority(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fgna")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fgna")
	// No index on d anywhere: this table exercises the STREAMING aggregation,
	// which is the path Java decides with DynamicMessage.equals.
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE fgna "+
		"CREATE TABLE t (id BIGINT, d DOUBLE, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fgna/s WITH TEMPLATE fgna")
	dsn := fmt.Sprintf("fdbsql:///testdb_fgna?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	groupNaNSeed(t, db, ctx, "t")

	t.Run("nan_payloads_are_one_group", func(t *testing.T) {
		q := "SELECT d, COUNT(*) FROM t GROUP BY d"
		groups := readFloatGroups(t, db, ctx, q)
		nanGroups, nanRows, negZero, posZero := summarize(groups)
		plan := floatOrderingExplain(t, db, ctx, q)
		if nanGroups != 1 || nanRows != 2 {
			t.Errorf("GROUP BY over two distinct NaN payloads produced %d NaN group(s) "+
				"covering %d row(s), want 1 group covering 2 — Java's streaming aggregation "+
				"decides sameness with DynamicMessage.equals, and Double.equals collapses "+
				"every NaN payload (doubleToLongBits)\n  groups: %+v\n  plan: %s",
				nanGroups, nanRows, groups, plan)
		}
		// The SAME assertion, in the opposite direction: the signed zeros must
		// stay two groups. Canonicalizing NaN by canonicalizing "all special
		// floats" would satisfy the check above and break this one — and would
		// also make the answer depend on whether an aggregate index exists,
		// since the index stores the two zero signs as two physical entries
		// that Java reads the same way.
		if negZero != 1 || posZero != 1 {
			t.Errorf("signed zeros produced %d -0.0 group(s) and %d +0.0 group(s), want 1 "+
				"and 1 — Double.equals compares BITS, so -0.0 != +0.0 even though IEEE `==` "+
				"says otherwise\n  groups: %+v\n  plan: %s", negZero, posZero, groups, plan)
		}
		if len(groups) != 4 {
			t.Errorf("GROUP BY produced %d groups, want 4 (-1.5, -0.0, +0.0, NaN)\n"+
				"  groups: %+v\n  plan: %s", len(groups), groups, plan)
		}
	})

	// GROUP BY and DISTINCT are two dedup paths over the same column, and an
	// engine that answers them differently is wrong however each one reads on
	// its own. Go's DISTINCT already canonicalizes NaN (packedDedupKey); this
	// asserts the two now agree rather than assuming it.
	t.Run("distinct_agrees_with_group_by", func(t *testing.T) {
		gq := "SELECT d, COUNT(*) FROM t GROUP BY d"
		dq := "SELECT DISTINCT d FROM t"
		groups := readFloatGroups(t, db, ctx, gq)
		rows, err := db.QueryContext(ctx, dq)
		if err != nil {
			t.Fatalf("query %q: %v", dq, err)
		}
		defer rows.Close()
		var distinctNaN, distinctNegZero, distinctPosZero, distinctTotal int
		for rows.Next() {
			var v sql.NullFloat64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan %q: %v", dq, err)
			}
			distinctTotal++
			switch {
			case !v.Valid:
			case math.IsNaN(v.Float64):
				distinctNaN++
			case v.Float64 == 0 && math.Signbit(v.Float64):
				distinctNegZero++
			case v.Float64 == 0 && !math.Signbit(v.Float64):
				distinctPosZero++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", dq, err)
		}
		gNaN, _, gNegZero, gPosZero := summarize(groups)
		if distinctNaN != gNaN || distinctNegZero != gNegZero || distinctPosZero != gPosZero ||
			distinctTotal != len(groups) {
			t.Errorf("DISTINCT and GROUP BY disagree on the same column: DISTINCT gave "+
				"%d rows (nan=%d, -0=%d, +0=%d), GROUP BY gave %d groups (nan=%d, -0=%d, "+
				"+0=%d). Both must answer to Double.equals",
				distinctTotal, distinctNaN, distinctNegZero, distinctPosZero,
				len(groups), gNaN, gNegZero, gPosZero)
		}
	})
}

// The AGGREGATE INDEX path answers differently, and that is Java's answer too.
//
// This is a pinned NEGATIVE RESULT, not an aspiration. The grouping prefix IS
// the index key; tuple packing keeps every NaN payload, so two payloads are two
// entries in Go and in Java alike (AtomicMutationIndexMaintainer.java:141,158,
// read back one-row-per-entry by RecordQueryAggregateIndexPlan.java:128-144
// with no StreamGrouping in the path). Making Go merge them would mean writing
// key bytes Java does not — the wire line.
//
// If this test ever goes RED, the aggregate index has started merging NaN
// groups, and that is a WIRE change that needs the same argument #604 needed for
// vacated groups, not a quiet fix.
//
// IT MUST BE A **COUNT** INDEX, and that is a load-bearing detail rather than an
// arbitrary choice. RFC-209 made a grouped aggregate read prove the group is
// LIVE. A COUNT index proves that from its own entry — a stored count is the
// existence fact — so it is read directly (`live_groups_only`) with no extra
// stream and therefore no ordering requirement. A SUM index cannot, so it is
// companion-joined to a COUNT index, and that join is a MERGE which needs both
// streams ordered congruently with the comparison. An UNBOUND raw DOUBLE
// grouping coordinate is exactly where that fails — its tuple key order is not
// its value order — so a SUM-over-DOUBLE declines the index entirely and falls
// back to streaming aggregation.
//
// MEASURED on this schema:
//
//	SELECT d, COUNT(*) … GROUP BY d -> AggregateIndex(COUNT, …, live_groups_only)
//	SELECT d, SUM(a)   … GROUP BY d -> StreamingAgg(InMemorySort(Scan))
//
// This test used the SUM form until RFC-209 landed, at which point it stopped
// reaching the producer it exists to pin — and said so LOUDLY rather than
// skipping, which is the only reason the gap was caught instead of silently
// becoming a test that proves nothing.
func TestFDB_FloatAggregateIndexSplitsNaNPayloads(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fgnaidx")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fgnaidx")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE fgnaidx "+
		"CREATE TABLE t (id BIGINT, d DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX cnt_by_d AS SELECT COUNT(*) FROM t GROUP BY d")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fgnaidx/s WITH TEMPLATE fgnaidx")
	dsn := fmt.Sprintf("fdbsql:///testdb_fgnaidx?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	groupNaNSeed(t, db, ctx, "t")

	q := "SELECT d, COUNT(*) FROM t GROUP BY d"
	plan := floatOrderingExplain(t, db, ctx, q)
	groups := readFloatGroups(t, db, ctx, q)
	nanGroups, _, _, _ := summarize(groups)
	// The plan assertion comes FIRST and is fatal. This test only says anything
	// if the aggregate index actually served the query: answered by the
	// streaming path it would (correctly) report ONE NaN group, and a test that
	// quietly returned on that reading would go silent at precisely the moment
	// its subject stopped being reachable — the failure mode that lets a pinned
	// divergence rot into an untested assumption.
	if !strings.Contains(strings.ToUpper(strings.ReplaceAll(plan, " ", "")), "AGGREGATEINDEX") {
		t.Fatalf("this query was NOT served by the aggregate index, so the payload split "+
			"this test exists to pin is not exercised at all\n  query: %s\n  plan: %s", q, plan)
	}
	if nanGroups == 1 {
		t.Fatalf("the aggregate index reported ONE NaN group. Either the index has started "+
			"MERGING NaN payloads — a WIRE change, since the grouping prefix is the packed "+
			"index key and Java splits them (AtomicMutationIndexMaintainer.java:141,158) — "+
			"or the ladder no longer carries two distinct payloads\n  groups: %+v\n  plan: %s",
			groups, plan)
	}
	if nanGroups != 2 {
		t.Errorf("the aggregate index reported %d NaN groups, want 2 — the grouping prefix "+
			"is the packed index key and tuple encoding preserves the NaN payload, in Go and "+
			"in Java alike. Merging them is a WIRE change, not a fix\n  groups: %+v\n  plan: %s",
			nanGroups, groups, plan)
	}
}

// A 32-bit FLOAT column groups by the SAME identity, and this test exists
// because of HOW it gets there rather than merely THAT it does.
//
// There is no float32 arm in the group-key encoder. A FLOAT column arrives at
// the executor already WIDENED to float64, so `GROUP BY <float>` is served by
// the float64 canonicalization — MEASURED: a panic in a float32 arm passes the
// entire SQL-level gate untouched. A specialized float32 arm would therefore be
// dead code, and dead code cannot be mutation-tested: it is a claim the suite
// can never check.
//
// So the widening is what carries correctness here, and the widening is what
// this test pins. It goes RED if the float64 canonicalization is removed
// (proving the FLOAT carrier really does route through it) and it would also go
// red if the SQL layer ever stopped widening — which is the signal that a
// float32 arm has become necessary.
func TestFDB_FloatGroupByNaNAuthority_Float32(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fgna32")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fgna32")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE fgna32 "+
		"CREATE TABLE t (id BIGINT, g FLOAT, h DOUBLE, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fgna32/s WITH TEMPLATE fgna32")
	dsn := fmt.Sprintf("fdbsql:///testdb_fgna32?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// A FLOAT column holds no ±Inf (the narrowing range check rejects it), so
	// the NEGATIVE NaN is computed in DOUBLE arithmetic in helper column h and
	// narrowed on assignment, which preserves the sign bit. Narrowing also maps
	// the two payloads to the 32-bit quiet NaNs 0x7fc00000 / 0xffc00000 — two
	// DISTINCT float32 bit patterns, which is what this test needs.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, g, h, a) VALUES "+
		"(20, -1.5, 1.0e308, 1), (30, -0.0, 1.0e308, 1), (40, 0.0, 1.0e308, 1), "+
		"(70, 1.0, 1.0e308, 1), (5, CAST('NaN' AS DOUBLE), 1.0e308, 1)")
	mwjoMustExec(t, db, ctx, "UPDATE t SET g = (h * 10.0) + (h * -10.0) WHERE id = 70")

	// Vacuity guard: two NaNs with the SAME payload would make the merge
	// trivially true and the test would pass with the canonicalization gone.
	var negBits, posBits uint64
	rows, err := db.QueryContext(ctx, "SELECT id, g FROM t WHERE id IN (5, 70)")
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	for rows.Next() {
		var id int64
		var g sql.NullFloat64
		if err := rows.Scan(&id, &g); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		if !g.Valid || !math.IsNaN(g.Float64) {
			rows.Close()
			t.Fatalf("id=%d is %v, want a NaN", id, g.Float64)
		}
		if id == 70 {
			negBits = math.Float64bits(g.Float64)
			if !math.Signbit(g.Float64) {
				rows.Close()
				t.Fatalf("id=70 is a POSITIVE NaN; the ladder needs a negative one")
			}
		} else {
			posBits = math.Float64bits(g.Float64)
		}
	}
	rows.Close()
	if negBits == posBits {
		t.Fatalf("both FLOAT NaN rows widened to bit pattern %#016x; the test cannot "+
			"express the defect without two DISTINCT payloads", negBits)
	}

	q := "SELECT g, COUNT(*) FROM t GROUP BY g"
	groups := readFloatGroups(t, db, ctx, q)
	nanGroups, nanRows, negZero, posZero := summarize(groups)
	plan := floatOrderingExplain(t, db, ctx, q)
	if nanGroups != 1 || nanRows != 2 {
		t.Errorf("GROUP BY on a FLOAT column over two distinct NaN payloads produced %d NaN "+
			"group(s) covering %d row(s), want 1 covering 2. The FLOAT carrier reaches the "+
			"group-key encoder WIDENED to float64, so it is the float64 canonicalization "+
			"that must serve it\n  groups: %+v\n  plan: %s", nanGroups, nanRows, groups, plan)
	}
	if negZero != 1 || posZero != 1 {
		t.Errorf("FLOAT signed zeros produced %d -0.0 and %d +0.0 group(s), want 1 and 1\n"+
			"  groups: %+v\n  plan: %s", negZero, posZero, groups, plan)
	}
	if len(groups) != 4 {
		t.Errorf("GROUP BY on a FLOAT produced %d groups, want 4 (-1.5, -0.0, +0.0, NaN)\n"+
			"  groups: %+v\n  plan: %s", len(groups), groups, plan)
	}
}
