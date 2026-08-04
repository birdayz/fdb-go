package sqldriver_test

// End-to-end proof that an ordered index scan over a FLOAT/DOUBLE column
// returns rows in the ORDER THE COMPARATOR DEFINES, not the order the FDB
// tuple encoding happens to lay them out in.
//
// FDB tuple encoding flips the sign bit of a non-negative double and every bit
// of a negative one, which lays the IEEE-754 domain out as
//
//	negNaN payloads < -Inf < … < -0.0 < +0.0 < … < +Inf < posNaN payloads
//
// while values.CompareFloat64 (faithful to java.lang.Double.compare, the
// Record Layer's ordering authority) collapses every NaN payload to ONE value
// and ranks it GREATEST. A negative NaN is therefore the physically FIRST row
// and the logically LAST one, and all NaNs are a single logical tie class split
// across two disjoint physical blocks — which is why a float coordinate must
// TERMINATE an ordering claim rather than merely be reordered.
//
// Each shape below is a DIFFERENTIAL against an oracle table carrying the
// identical rows with NO index, where the planner has no scan order to claim
// and must materialize a CompareFloat64 sort. The two paths must agree row for
// row. The expected logical order is ALSO asserted outright: a differential
// alone passes when both sides are wrong in the same way.
//
// Two properties of this test are load-bearing and easy to destroy:
//
//   - The indexed side must actually TAKE the index. A differential on
//     `ORDER BY <float>, <pk>` with no WHERE clause passes with the defect
//     fully present, because both sides full-scan and sort — it compares a
//     baseline against a copy of itself and never reaches the branch under
//     test. Every shape here binds the index with an equality on its leading
//     column and asserts via EXPLAIN that the index scan is in the plan.
//   - The ids must make the NaN tie class INTERLEAVE the two physical blocks
//     (see floatOrderingRows).
//
// Deliberately NOT used here: a range predicate on the float column itself
// (`WHERE e > 5.0`). That does bind the index, but a float range compiles to
// ONE contiguous key range while the float domain occupies TWO disjoint
// physical blocks, so the scan misses the negative-NaN block below -Inf and
// drops rows. That is a separate ROW-LOSS defect, not an ordering defect, and
// no ordering-claim fix can reach it; using it here would make this test red
// for a reason it does not own. The equality-bound prefix binds the index just
// as firmly and scans the float coordinate's FULL range.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
)

// floatOrderingRows is the probe ladder: every IEEE-754 edge class, with both
// NaN signs carrying DISTINCT payloads.
//
// The ids are chosen adversarially, and this is the property the whole test
// rests on. Physically the rows come back in the order
//
//	70(negNaN) 10(-Inf) 20(-1.5) 30(-0.0) 40(+0.0) 50(1.5) 60(+Inf) 5(posNaN)
//
// while `ORDER BY e, id` demands
//
//	10 20 30 40 50 60 then the NaN tie class by id: 5 70
//
// so the two disagree on BOTH dimensions at once: where the NaN block sits
// (physically split to the two ends, logically one block at the tail) AND the
// tie-break inside it. Giving the negative NaN the SMALLER id — the obvious
// choice, since it is physically first — would make the tie-break agree by
// accident and hide half the defect from a test that is otherwise correct.
type floatOrderingRow struct {
	id   int64
	seed string
	// makeNonFinite is an UPDATE assignment expression run afterwards, or ""
	// when the seed is already the final value.
	//
	// The two-step seeding is HISTORICAL, not required: INSERT … VALUES once
	// rejected NaN and +/-Inf with 22023 while UPDATE did not, so the ladder
	// was built around the one path that worked. Every write path now accepts
	// them (see nonfinite_float_write_symmetry_fdb_test.go). It is kept because
	// the arithmetic is how the ladder OBTAINS its two distinct NaN payloads —
	// `(+Inf) + (-Inf)` is the invalid operation that yields the sign-bit-SET
	// quiet NaN, which no literal spells.
	makeNonFinite string
	label         string
}

var floatOrderingRows = []floatOrderingRow{
	{id: 10, seed: "1.0e308", makeNonFinite: "e * -10.0", label: "-Inf"},
	{id: 20, seed: "-1.5", label: "-1.5"},
	{id: 30, seed: "-0.0", label: "-0.0"},
	{id: 40, seed: "0.0", label: "+0.0"},
	{id: 50, seed: "1.5", label: "1.5"},
	{id: 60, seed: "1.0e308", makeNonFinite: "e * 10.0", label: "+Inf"},
	// Inf + (-Inf) yields the sign-bit-SET quiet NaN 0xfff8000000000000, which
	// packs BEFORE -Inf — the physically first row in the table.
	{id: 70, seed: "1.0e308", makeNonFinite: "(e * 10.0) + (e * -10.0)", label: "negNaN"},
	// A DIFFERENT payload from the negative one (0x7ff8000000000001), so the
	// test also covers "two distinct bit patterns are one logical value".
	{id: 5, seed: "0.0", makeNonFinite: "CAST('NaN' AS DOUBLE)", label: "posNaN"},
}

// floatOrderingNaNIDs is the logical tie class — one value under
// CompareFloat64, two rows, two payloads, two physical blocks.
var floatOrderingNaNIDs = map[int64]bool{70: true, 5: true}

// floatOrderingLogicalASC is the full `ORDER BY e, id` answer.
var floatOrderingLogicalASC = []int64{10, 20, 30, 40, 50, 60, 5, 70}

// seedFloatOrderingLadder writes the ladder into tbl with a constant `a` so an
// equality on the index's leading column binds and leaves the float as the
// leading SORTED coordinate.
func seedFloatOrderingLadder(t *testing.T, db *sql.DB, ctx context.Context, tbl string) {
	t.Helper()
	var vals []string
	for _, r := range floatOrderingRows {
		vals = append(vals, fmt.Sprintf("(%d, %s, 1)", r.id, r.seed))
	}
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO %s (id, e, a) VALUES %s", tbl, strings.Join(vals, ", ")))
	for _, r := range floatOrderingRows {
		if r.makeNonFinite == "" {
			continue
		}
		mwjoMustExec(t, db, ctx, fmt.Sprintf(
			"UPDATE %s SET e = %s WHERE id = %d", tbl, r.makeNonFinite, r.id))
	}
}

// assertFloatLadderStored fails loudly if the ladder did not land as intended.
// Every assertion below is meaningless without it: if the write path silently
// rejected the non-finite values (or started rejecting them after a guard
// change), the test would be comparing two tables of ordinary finite doubles
// and would pass with the defect fully present.
func assertFloatLadderStored(t *testing.T, db *sql.DB, ctx context.Context, tbl string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT id, e FROM %s", tbl))
	if err != nil {
		t.Fatalf("ladder readback on %s: %v", tbl, err)
	}
	defer rows.Close()
	got := map[int64]float64{}
	for rows.Next() {
		var id int64
		var e sql.NullFloat64
		if err := rows.Scan(&id, &e); err != nil {
			t.Fatalf("ladder scan on %s: %v", tbl, err)
		}
		if !e.Valid {
			t.Fatalf("%s id=%d stored NULL, want a float", tbl, id)
		}
		got[id] = e.Float64
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ladder rows on %s: %v", tbl, err)
	}
	if len(got) != len(floatOrderingRows) {
		t.Fatalf("%s has %d rows, want %d", tbl, len(got), len(floatOrderingRows))
	}
	check := func(id int64, ok func(float64) bool, want string) {
		t.Helper()
		v, present := got[id]
		if !present {
			t.Fatalf("%s: id %d missing", tbl, id)
		}
		if !ok(v) {
			t.Fatalf("%s: id %d stored %v (bits %#016x), want %s — the ladder did not "+
				"land, so every ordering assertion in this test is vacuous",
				tbl, id, v, math.Float64bits(v), want)
		}
	}
	check(10, func(v float64) bool { return math.IsInf(v, -1) }, "-Inf")
	check(60, func(v float64) bool { return math.IsInf(v, +1) }, "+Inf")
	check(30, func(v float64) bool { return v == 0 && math.Signbit(v) }, "-0.0")
	check(40, func(v float64) bool { return v == 0 && !math.Signbit(v) }, "+0.0")
	check(70, func(v float64) bool { return math.IsNaN(v) && math.Signbit(v) }, "a NEGATIVE NaN")
	check(5, func(v float64) bool { return math.IsNaN(v) && !math.Signbit(v) }, "a POSITIVE NaN")
	if math.Float64bits(got[70]) == math.Float64bits(got[5]) {
		t.Fatalf("%s: the two NaN rows share bit pattern %#016x; the ladder must carry "+
			"two DISTINCT payloads so 'two bit patterns, one logical value' is exercised",
			tbl, math.Float64bits(got[70]))
	}
}

func floatOrderingIDs(t *testing.T, db *sql.DB, ctx context.Context, q string) []int64 {
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
		t.Fatalf("rows %q: %v", q, err)
	}
	return out
}

func floatOrderingExplain(t *testing.T, db *sql.DB, ctx context.Context, q string) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	return plan
}

func floatOrderingSameOrder(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func floatOrderingReversed(in []int64) []int64 {
	out := make([]int64, len(in))
	for i := range in {
		out[i] = in[len(in)-1-i]
	}
	return out
}

func TestFDB_FloatOrderingClaim_Differential(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_focd")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_focd")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE focd "+
		// fi: the shape under test — a compound index whose leading column is
		// equality-bound, leaving the DOUBLE as the leading sorted coordinate.
		"CREATE TABLE fi (id BIGINT, e DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		// fo: the oracle — identical rows, NO index at all, so the planner has
		// no claimed scan order to elide onto and must sort with CompareFloat64.
		"CREATE TABLE fo (id BIGINT, e DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX fi_ae ON fi (a, e)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_focd/s WITH TEMPLATE focd")
	dsn := fmt.Sprintf("fdbsql:///testdb_focd?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	seedFloatOrderingLadder(t, db, ctx, "fi")
	seedFloatOrderingLadder(t, db, ctx, "fo")
	assertFloatLadderStored(t, db, ctx, "fi")
	assertFloatLadderStored(t, db, ctx, "fo")

	// differential runs the same logical query on the indexed table and on the
	// unindexed oracle and requires: the indexed plan really uses the index,
	// the rows match the logical order, and the two paths agree.
	differential := func(name, orderBy string, want []int64) {
		t.Run(name, func(t *testing.T) {
			idxQ := "SELECT id FROM fi WHERE a = 1 ORDER BY " + orderBy
			refQ := "SELECT id FROM fo WHERE a = 1 ORDER BY " + orderBy
			idxPlan := floatOrderingExplain(t, db, ctx, idxQ)
			if !strings.Contains(strings.ToUpper(idxPlan), "FI_AE") {
				t.Fatalf("the indexed side did not take index FI_AE, so this shape never "+
					"reaches the ordering-claim branch it exists to test and would pass "+
					"against a copy of the oracle.\n  query: %s\n  plan:  %s", idxQ, idxPlan)
			}
			got := floatOrderingIDs(t, db, ctx, idxQ)
			ref := floatOrderingIDs(t, db, ctx, refQ)
			if !floatOrderingSameOrder(got, want) {
				t.Errorf("indexed path returned %v, want %v — an ordered scan of a DOUBLE "+
					"column claimed the FDB tuple KEY order as the column's VALUE order "+
					"(a negative NaN packs before -Inf but compares GREATEST)\n  query: %s\n  plan:  %s",
					got, want, idxQ, idxPlan)
			}
			if !floatOrderingSameOrder(got, ref) {
				t.Errorf("DIFFERENTIAL MISMATCH: indexed=%v unindexed oracle=%v\n"+
					"  idx query: %s\n  idx plan:  %s\n  ref query: %s\n  ref plan:  %s",
					got, ref, idxQ, idxPlan, refQ, floatOrderingExplain(t, db, ctx, refQ))
			}
		})
	}

	// ASC, tie broken by the primary key — the shape where the ids make the
	// NaN tie class interleave both physical blocks.
	differential("float_then_pk_asc", "e, id", floatOrderingLogicalASC)
	// DESC. NaN is greatest, so the two NaN rows lead, ordered by id DESC.
	differential("float_then_pk_desc", "e DESC, id DESC", floatOrderingReversed(floatOrderingLogicalASC))
	// A NULLS clause must not talk the planner back into the elision.
	differential("float_then_pk_nulls_first", "e NULLS FIRST, id", floatOrderingLogicalASC)
	differential("float_then_pk_nulls_last", "e NULLS LAST, id", floatOrderingLogicalASC)
	// Tie broken by a non-key column that is constant here: the NaN pair may
	// come back in either internal order, so only the block position is
	// asserted (below), not a total order.

	// `ORDER BY e` alone leaves the NaN pair tied, so the internal order is
	// free. What is NOT free is that every NaN follows every non-NaN.
	t.Run("float_alone_nan_block_is_last", func(t *testing.T) {
		idxQ := "SELECT id FROM fi WHERE a = 1 ORDER BY e"
		refQ := "SELECT id FROM fo WHERE a = 1 ORDER BY e"
		idxPlan := floatOrderingExplain(t, db, ctx, idxQ)
		if !strings.Contains(strings.ToUpper(idxPlan), "FI_AE") {
			t.Fatalf("the indexed side did not take index FI_AE\n  query: %s\n  plan: %s", idxQ, idxPlan)
		}
		got := floatOrderingIDs(t, db, ctx, idxQ)
		ref := floatOrderingIDs(t, db, ctx, refQ)
		var body []int64
		firstNaN, lastNonNaN := -1, -1
		for i, v := range got {
			if floatOrderingNaNIDs[v] {
				if firstNaN < 0 {
					firstNaN = i
				}
				continue
			}
			body = append(body, v)
			lastNonNaN = i
		}
		wantBody := []int64{10, 20, 30, 40, 50, 60}
		if !floatOrderingSameOrder(body, wantBody) {
			t.Errorf("non-NaN rows came back as %v, want %v\n  query: %s\n  plan: %s",
				body, wantBody, idxQ, idxPlan)
		}
		if firstNaN >= 0 && firstNaN < lastNonNaN {
			t.Errorf("a NaN landed at position %d, before the last non-NaN at position %d; "+
				"CompareFloat64 ranks every NaN GREATEST, but the negative-NaN payload packs "+
				"BELOW -Inf\n  rows: %v\n  query: %s\n  plan: %s",
				firstNaN, lastNonNaN, got, idxQ, idxPlan)
		}
		if len(got) != len(ref) {
			t.Errorf("DIFFERENTIAL ROW-COUNT MISMATCH: indexed=%v unindexed oracle=%v", got, ref)
		}
	})

	// NEGATIVE RESULT, pinned: the divergence is about NaN and ONLY NaN.
	// -0.0 packs immediately before +0.0 and CompareFloat64 also ranks -0.0
	// below +0.0; -Inf is the least non-NaN and +Inf the greatest. So a ladder
	// with every non-NaN edge value present is served correctly by the index
	// path even with the ordering claim intact. This is what makes "NaN
	// specifically" a measured claim rather than an assumption — if it ever
	// goes red, the defect is wider than NaN and the analysis must be redone.
	t.Run("negative_result_non_nan_edges_order_identically", func(t *testing.T) {
		q := "SELECT id FROM fi WHERE a = 1 AND id <> 70 AND id <> 5 ORDER BY e, id"
		plan := floatOrderingExplain(t, db, ctx, q)
		got := floatOrderingIDs(t, db, ctx, q)
		want := []int64{10, 20, 30, 40, 50, 60}
		if !floatOrderingSameOrder(got, want) {
			t.Fatalf("signed zeros and +/-Inf are NOT ordered identically by the tuple "+
				"encoding and by CompareFloat64: got %v, want %v. The divergence is WIDER "+
				"than NaN and the ordering-claim analysis must be redone.\n  plan: %s",
				got, want, plan)
		}
		// And the two signed zeros must remain two DISTINCT keys in the order
		// -0.0 then +0.0.
		zq := "SELECT id FROM fi WHERE a = 1 AND e >= -0.0 AND e <= 0.0 ORDER BY e, id"
		z := floatOrderingIDs(t, db, ctx, zq)
		if !floatOrderingSameOrder(z, []int64{30, 40}) {
			t.Fatalf("signed-zero span returned %v, want [30 40] (-0.0 then +0.0, two "+
				"distinct keys)\n  query: %s\n  plan: %s", z, zq, floatOrderingExplain(t, db, ctx, zq))
		}
	})
}

// The 32-bit FLOAT path uses a different tuple type code (0x20) than DOUBLE
// (0x21) but the same sign-bit transform, so it has the same defect and needs
// its own proof — a fix that consulted only TypeCodeDouble would pass every
// assertion above.
//
// The seeding differs, and the difference is worth stating because the obvious
// route does not work. A FLOAT column cannot hold +/-Inf (the write path's
// range check rejects it), so the negative NaN cannot be built out of the
// column's own value. Nor can it be made by negating a positive NaN with
// `g * -1.0`: IEEE 754 multiplication with a NaN operand RETURNS THAT NaN
// (quieted) and does not apply the other operand's sign, so the sign bit
// survives untouched and the column ends up holding a second POSITIVE NaN.
// (MEASURED — that seeding produced `want a NEGATIVE NaN` from the assertion
// below, which is the whole reason the assertion is there.)
//
// What does work is computing the value in DOUBLE arithmetic and letting the
// assignment narrow it: `Inf + (-Inf)` is an invalid operation and yields the
// default quiet NaN, whose sign bit is SET, and float64→float32 narrowing
// preserves the sign. Hence the helper DOUBLE column `h`.
func TestFDB_FloatOrderingClaim_Differential_Float32(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_focd32")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_focd32")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE focd32 "+
		"CREATE TABLE gi (id BIGINT, g FLOAT, h DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE go_ (id BIGINT, g FLOAT, h DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX gi_ag ON gi (a, g)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_focd32/s WITH TEMPLATE focd32")
	dsn := fmt.Sprintf("fdbsql:///testdb_focd32?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Same adversarial id assignment as the DOUBLE ladder: the negative NaN
	// gets the LARGER id so the tie-break disagrees with the physical order.
	seed := func(tbl string) {
		mwjoMustExec(t, db, ctx, fmt.Sprintf(
			"INSERT INTO %s (id, g, h, a) VALUES (20, -1.5, 1.0e308, 1), (30, -0.0, 1.0e308, 1), "+
				"(40, 0.0, 1.0e308, 1), (50, 1.5, 1.0e308, 1), (70, 1.0, 1.0e308, 1), (5, 0.0, 1.0e308, 1)", tbl))
		mwjoMustExec(t, db, ctx, fmt.Sprintf("UPDATE %s SET g = CAST('NaN' AS DOUBLE) WHERE id = 5", tbl))
		// h*10 = +Inf, h*-10 = -Inf; their sum is the default quiet NaN with
		// the sign bit SET, narrowed into the FLOAT column on assignment.
		mwjoMustExec(t, db, ctx, fmt.Sprintf(
			"UPDATE %s SET g = (h * 10.0) + (h * -10.0) WHERE id = 70", tbl))
	}
	seed("gi")
	seed("go_")

	// The seeding is only as good as what actually landed.
	rows, err := db.QueryContext(ctx, "SELECT id, g FROM gi")
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	stored := map[int64]float64{}
	for rows.Next() {
		var id int64
		var g sql.NullFloat64
		if err := rows.Scan(&id, &g); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		if g.Valid {
			stored[id] = g.Float64
		}
	}
	rows.Close()
	if v, ok := stored[70]; !ok || !math.IsNaN(v) || !math.Signbit(v) {
		t.Fatalf("FLOAT column id=70 is %v (present=%v), want a NEGATIVE NaN — without it "+
			"the physically-first block is empty and this test is vacuous", stored[70], ok)
	}
	if v, ok := stored[5]; !ok || !math.IsNaN(v) || math.Signbit(v) {
		t.Fatalf("FLOAT column id=5 is %v (present=%v), want a POSITIVE NaN", stored[5], ok)
	}

	q := "SELECT id FROM gi WHERE a = 1 ORDER BY g, id"
	refQ := "SELECT id FROM go_ WHERE a = 1 ORDER BY g, id"
	plan := floatOrderingExplain(t, db, ctx, q)
	if !strings.Contains(strings.ToUpper(plan), "GI_AG") {
		t.Fatalf("the indexed side did not take index GI_AG, so this shape never reaches "+
			"the ordering-claim branch\n  query: %s\n  plan: %s", q, plan)
	}
	got := floatOrderingIDs(t, db, ctx, q)
	ref := floatOrderingIDs(t, db, ctx, refQ)
	want := []int64{20, 30, 40, 50, 5, 70}
	if !floatOrderingSameOrder(got, want) {
		t.Errorf("FLOAT(32) indexed path returned %v, want %v — the 32-bit tuple code "+
			"(0x20) has the same sign-bit transform as DOUBLE (0x21) and the same "+
			"physical/logical divergence\n  query: %s\n  plan: %s", got, want, q, plan)
	}
	if !floatOrderingSameOrder(got, ref) {
		t.Errorf("FLOAT(32) DIFFERENTIAL MISMATCH: indexed=%v unindexed oracle=%v", got, ref)
	}
}

// The equality-bound float axis, end to end.
//
// Every shape above binds the index with an equality on an INTEGER column and
// leaves the float as the leading SORTED coordinate. That leaves the opposite
// arrangement — the FLOAT ITSELF equality-bound, with the primary key as the
// sorted suffix — completely unprobed, and it is a different question with a
// different answer:
//
//   - A NONZERO float equality pins ONE tuple-encoded key prefix. Every row
//     under it carries identical float bits, so the primary-key suffix really
//     is in key order and the sort must be elided. NaN never enters: a scan
//     range built from a finite literal cannot reach either NaN block.
//   - A ZERO float equality pins NOTHING. -0.0 and +0.0 are IEEE-equal but pack
//     to two distinct adjacent keys, so the scan spans TWO physical prefixes
//     and the primary-key suffix restarts at the boundary.
//
// Both directions are asserted here on ROWS, and the plan is asserted too: the
// nonzero case returns correct rows whether or not the sort is elided, so rows
// alone cannot tell a working elision from a lost one.
func TestFDB_FloatOrderingClaim_EqualityBoundFloat(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_foceq")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_foceq")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE foceq "+
		// The float is the index's LEADING column, so an equality on it is the
		// fixed prefix and the primary key is the whole sorted suffix.
		"CREATE TABLE fq (id BIGINT, e DOUBLE, PRIMARY KEY (id)) "+
		"CREATE INDEX fq_e ON fq (e)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_foceq/s WITH TEMPLATE foceq")
	dsn := fmt.Sprintf("fdbsql:///testdb_foceq?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The ids are adversarial in both groups.
	//
	// For 2.5 the ids are inserted out of order, so a path that returned
	// INSERTION order rather than key order would be caught.
	//
	// For the zeros the assignment is what makes the signed-zero case
	// detectable at all: -0.0 packs BEFORE +0.0, so giving the -0.0 row the
	// LARGER id (9) makes the physical order [9 1] disagree with `ORDER BY id`
	// = [1 9]. Handing the -0.0 row the smaller id would make both orders agree
	// by accident and the test would pass with the elision wrongly applied.
	mwjoMustExec(t, db, ctx, "INSERT INTO fq (id, e) VALUES "+
		"(40, 2.5), (10, 2.5), (30, 2.5), (9, 0.0), (1, 0.0), (7, 1.5)")
	mwjoMustExec(t, db, ctx, "UPDATE fq SET e = -0.0 WHERE id = 9")

	// Without this the zero half is vacuous: if the -0.0 did not land, both
	// zero rows share a key and any order looks correct.
	var z float64
	if err := db.QueryRowContext(ctx, "SELECT e FROM fq WHERE id = 9").Scan(&z); err != nil {
		t.Fatalf("zero readback: %v", err)
	}
	if z != 0 || !math.Signbit(z) {
		t.Fatalf("id=9 stored %v (bits %#016x), want -0.0 — without a NEGATIVE zero the "+
			"two zero rows share one key and the signed-zero assertion below is vacuous",
			z, math.Float64bits(z))
	}

	// DESCENDING deliberately. The ASCENDING form of this query keeps its
	// elision whether or not an equality-pinned float can extend the claim —
	// the plan-side derivation reaches that answer without consulting the
	// candidate's matched ordering parts — so it passes with the gate broken
	// and proves nothing. A reverse scan is servable only if the candidate
	// itself can prove the primary-key suffix is ordered, so this shape moves
	// when the gate does. (MEASURED: with the equality exemption removed, the
	// ascending form stays green and this one goes red.)
	for _, tc := range []struct {
		name string
		q    string
		want []int64
	}{
		{
			"nonzero_equality_keeps_its_elision_and_rows_desc",
			"SELECT id FROM fq WHERE e = 2.5 ORDER BY id DESC",
			[]int64{40, 30, 10},
		},
		{
			"nonzero_equality_rows_ascending",
			"SELECT id FROM fq WHERE e = 2.5 ORDER BY id",
			[]int64{10, 30, 40},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := floatOrderingExplain(t, db, ctx, tc.q)
			if !strings.Contains(strings.ToUpper(plan), "FQ_E") {
				t.Fatalf("the equality did not bind index FQ_E, so this shape never reaches "+
					"the ordering-claim branch\n  query: %s\n  plan: %s", tc.q, plan)
			}
			if strings.Contains(plan, "Sort") {
				t.Errorf("a NONZERO float equality pins ONE physical key prefix, so the "+
					"primary-key suffix under it is already in key order and this sort is "+
					"dead weight. The ordering claim terminated at a coordinate that is FIXED, "+
					"not sorted.\n  query: %s\n  plan: %s", tc.q, plan)
			}
			if got := floatOrderingIDs(t, db, ctx, tc.q); !floatOrderingSameOrder(got, tc.want) {
				t.Errorf("got %v, want %v\n  query: %s\n  plan: %s", got, tc.want, tc.q, plan)
			}
		})
	}

	t.Run("zero_equality_must_not_elide_and_rows_stay_sorted", func(t *testing.T) {
		q := "SELECT id FROM fq WHERE e = 0.0 ORDER BY id"
		plan := floatOrderingExplain(t, db, ctx, q)
		got := floatOrderingIDs(t, db, ctx, q)
		want := []int64{1, 9}
		if !floatOrderingSameOrder(got, want) {
			t.Errorf("got %v, want %v — `e = 0.0` spans BOTH signed zeros, which are two "+
				"distinct adjacent keys, so the scan covers two physical prefixes and the "+
				"primary-key suffix restarts at the boundary. Physically the rows arrive "+
				"[9 1]; eliding the sort hands that back.\n  query: %s\n  plan: %s",
				got, want, q, plan)
		}
		if !strings.Contains(plan, "Sort") {
			t.Errorf("a ZERO float equality pins NO single key, so the primary-key suffix "+
				"claims an order the scan does not deliver and the sort must survive.\n"+
				"  query: %s\n  plan: %s", q, plan)
		}
		// Both zero rows must be found. A widening that covered only one signed
		// zero would return a correctly-ordered but SHORT answer.
		if len(got) != 2 {
			t.Errorf("`e = 0.0` matched %d rows (%v), want both signed zeros", len(got), got)
		}
	})
}
