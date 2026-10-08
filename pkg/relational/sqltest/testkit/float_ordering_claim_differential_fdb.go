package testkit

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

// seedFloatOrderingLadder writes the ladder into tbl with a constant `a` so an
// equality on the index's leading column binds and leaves the float as the
// leading SORTED coordinate.
func SeedFloatOrderingLadder(t *testing.T, db *sql.DB, ctx context.Context, tbl string) {
	t.Helper()
	var vals []string
	for _, r := range floatOrderingRows {
		vals = append(vals, fmt.Sprintf("(%d, %s, 1)", r.id, r.seed))
	}
	MustExecCtx(t, db, ctx, fmt.Sprintf(
		"INSERT INTO %s (id, e, a) VALUES %s", tbl, strings.Join(vals, ", ")))
	for _, r := range floatOrderingRows {
		if r.makeNonFinite == "" {
			continue
		}
		MustExecCtx(t, db, ctx, fmt.Sprintf(
			"UPDATE %s SET e = %s WHERE id = %d", tbl, r.makeNonFinite, r.id))
	}
}

// assertFloatLadderStored fails loudly if the ladder did not land as intended.
// Every assertion below is meaningless without it: if the write path silently
// rejected the non-finite values (or started rejecting them after a guard
// change), the test would be comparing two tables of ordinary finite doubles
// and would pass with the defect fully present.
func AssertFloatLadderStored(t *testing.T, db *sql.DB, ctx context.Context, tbl string) {
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

func FloatOrderingIDs(t *testing.T, db *sql.DB, ctx context.Context, q string) []int64 {
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

func FloatOrderingExplain(t *testing.T, db *sql.DB, ctx context.Context, q string) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	return plan
}

func FloatOrderingSameOrder(a, b []int64) bool {
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
