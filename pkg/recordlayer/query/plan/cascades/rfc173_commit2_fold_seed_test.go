package cascades

// RFC-173 S4 commit 2 (C) — the projected-EXISTS-over-join ordinalization
// helpers. These white-box pins guard the step-1 seed reconstruction and the
// leg-eligibility gate against a silent revert to the name model: the dualwindow
// differential is BLIND to such a revert (name-model rows are correct too, so
// both windows agree), so the ordinal path's activation must be pinned
// structurally, not only by row equality (the R1 lesson).

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func commit2RecType(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
	}
	return &values.RecordType{RecordName: name, Fields: fields}
}

// A gated projected-EXISTS fold over two SCAN legs reconstructs the FULL
// leg-concat ordinal seed the step-1 NLJ births from — a non-anchored RC of
// baked ofOrdinal references, byte-compatible with buildOrdinalJoinResultValue,
// which BOTH layout twins accept (full coverage). The projection is never a
// windows source; the seed is.
func TestRFC173Commit2_ReconstructFoldStep1Seed(t *testing.T) {
	t.Parallel()
	t1 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
	t2 := plans.NewRecordQueryScanPlan([]string{"T2"}, commit2RecType("T2", "ID", "T1_ID"), false)

	seed := reconstructFoldStep1Seed(t1, t2, "T1", "T2")
	if seed == nil {
		t.Fatal("two scan legs must reconstruct an ordinal step-1 seed")
	}
	rc, ok := seed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("seed must be a RecordConstructorValue, got %T", seed)
	}
	if rc.AnchoredJoin {
		t.Fatal("the reconstructed seed must be NON-anchored (the whole point: no name-model producer)")
	}
	if len(rc.Fields) != 4 {
		t.Fatalf("seed must concat all leg columns (T1.ID,T1.V,T2.ID,T2.T1_ID), got %d fields", len(rc.Fields))
	}
	// Every field is a FrontierPinned baked ordinal over its leg QOV — the shape
	// OrdinalSeedLegWindows / ordinalJoinSpansOf require.
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			t.Fatalf("field[%d] must be a FrontierPinned baked FieldValue, got %T", i, f.Value)
		}
	}

	// The layout twin ACCEPTS the reconstructed seed (both-accept), with the leg
	// windows at their declaration offsets — this is what makes the seed a valid
	// windows source while the folded projection is not.
	w, _ := ordinalSeedLegWindowsOf(seed)
	if w == nil {
		t.Fatal("OrdinalSeedLegWindows must accept the full-coverage reconstructed seed")
	}
	if w["T1"].Offset != 0 {
		t.Fatalf("T1 window offset = %d, want 0", w["T1"].Offset)
	}
	if w["T2"].Offset != 2 {
		t.Fatalf("T2 window offset = %d, want 2 (after T1's two columns)", w["T2"].Offset)
	}
}

// legIsOrdinalSafe admits a single-source scan leg (its rows are one namespace,
// ordinal-positionable) and REJECTS a name-model merged-row leg (a join), which
// stays name-model — the executor twin of the translator gate's ordinalEligible
// (correct-or-conservative). This is the guard that keeps the step-1 seed from
// mis-positioning a leg whose rows are a dotted-keyed merged row.
func TestRFC173Commit2_LegIsOrdinalSafe(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, commit2RecType("T", "ID"), false)
	if !legIsOrdinalSafe(scan) {
		t.Fatal("a scan leg must be ordinal-safe")
	}
	// An INDEX / covering-index leg is also a single source — ordinal-safe. The
	// realistic shape: a covering index gets picked for a fold leg. If its flowed
	// type IS a record it ordinalizes consistently (seed typed by flowedType, the
	// NLJ births from the same seed); if it is NOT a record the reconstruction
	// declines below and the leg stays name-model — never a silent-wrong path.
	idxRec := plans.NewRecordQueryIndexPlan("idx", nil, []string{"T2"}, commit2RecType("T2", "ID"), false)
	if !legIsOrdinalSafe(idxRec) {
		t.Fatal("an index-scan leg is a single source — ordinal-safe")
	}
	if reconstructFoldStep1Seed(scan, idxRec, "T", "T2") == nil {
		t.Fatal("two single-source legs (scan + record-typed index) must reconstruct a seed")
	}
	// A covering index whose flowed type is NOT a record: ordinal-safe by shape,
	// but the reconstruction DECLINES (leg type is not addressable positionally)
	// — the leg keeps the name model, correct-or-conservative.
	idxOpaque := plans.NewRecordQueryIndexPlan("idx", nil, []string{"T2"}, values.UnknownType, false)
	if reconstructFoldStep1Seed(scan, idxOpaque, "T", "T2") != nil {
		t.Fatal("a non-record-typed index leg must decline the seed reconstruction (name model)")
	}
	// A filter over a scan (a leg with a pushed predicate) unwraps to the scan.
	filtered := plans.NewRecordQueryPredicatesFilterPlan(scan, nil)
	if !legIsOrdinalSafe(filtered) {
		t.Fatal("a filter over a scan unwraps to a single source — ordinal-safe")
	}
	// A join leg emits a name-model merged row — NOT ordinal-safe.
	nlj := plans.NewRecordQueryNestedLoopJoinPlan(scan, scan, nil, plans.JoinInner, "A", "B", nil)
	if legIsOrdinalSafe(nlj) {
		t.Fatal("a name-model join leg must NOT be ordinal-safe (it stays name-model)")
	}
	// A reconstruction over a join leg must therefore DECLINE (nil), keeping the
	// name model for that shape.
	if reconstructFoldStep1Seed(nlj, scan, "A", "T") != nil {
		t.Fatal("a join leg must decline the seed reconstruction")
	}
}

// foldStep1Seed is the exact gate the rule wires: it must ORDINALIZE
// (gated=true, step1RV=seed) a projected-EXISTS fold over independent scan legs,
// and DECLINE (gated=false, step1RV=the original RV) for each disqualifier — the
// wiring pin the dualwindow differential cannot provide (a name-model revert
// leaves both windows agreeing on correct rows).
func TestRFC173Commit2_FoldStep1SeedGate(t *testing.T) {
	t.Parallel()
	existAlias := values.NamedCorrelationIdentifier("Q_EXISTS")
	t1 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID"), false)
	t2 := plans.NewRecordQueryScanPlan([]string{"T2"}, commit2RecType("T2", "ID"), false)
	// A projected fold RV references the existential quantifier (the ExistsValue).
	foldRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: &values.FieldValue{Field: "T1.ID", Typ: values.NotNullLong}},
		values.RecordConstructorField{Name: "F", Value: values.NewExistsValue(existAlias)},
	)

	// GATED: independent scan legs + a projected fold → the reconstructed seed.
	rv, gated := foldStep1Seed(foldRV, existAlias, false, t1, t2, "T1", "T2")
	if !gated {
		t.Fatal("a projected-EXISTS fold over independent scan legs must ordinalize (gated=true)")
	}
	if _, isRC := rv.(*values.RecordConstructorValue); !isRC || rv == foldRV {
		t.Fatal("gated step1RV must be the reconstructed seed, not the projection")
	}
	if w, _ := ordinalSeedLegWindowsOf(rv); w == nil {
		t.Fatal("the gated step1RV must be a windows-yielding ordinal seed")
	}

	// DECLINE, correlated step 1 (the twice-reverted wall): stays name-model.
	if _, g := foldStep1Seed(foldRV, existAlias, true, t1, t2, "T1", "T2"); g {
		t.Fatal("a correlated step-1 must NOT ordinalize (the F2 revert wall)")
	}

	// DECLINE, not a projected fold (WHERE-EXISTS pass-through — RV is bare QOV,
	// no existential reference): stays name-model, RV unchanged.
	bareRV := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T1"))
	if rv2, g := foldStep1Seed(bareRV, existAlias, false, t1, t2, "T1", "T2"); g || rv2 != bareRV {
		t.Fatal("a non-fold RV must NOT ordinalize and must pass through unchanged")
	}

	// DECLINE, a non-scan (join) leg: stays name-model.
	njoin := plans.NewRecordQueryNestedLoopJoinPlan(t1, t2, []predicates.QueryPredicate(nil), plans.JoinInner, "T1", "T2", nil)
	if _, g := foldStep1Seed(foldRV, existAlias, false, njoin, t2, "J", "T2"); g {
		t.Fatal("a name-model join leg must NOT ordinalize")
	}
}

// TestExistInnerIsScanSafe pins the N-way EXISTS existential-inner safety gate
// (implementNWayJoinWithExistential, RFC-173 :2908/:3033), in particular the
// FirstOrDefault/DefaultOnEmpty DECLINE — a discovered silent-wrong: the
// existential semi-join reads its inner's EMPTINESS as the no-match signal
// (FirstOrDefault(inner,NULL) IS NOT NULL), but a default-on-empty wrapper EMITS
// a row over an empty scan, so a FOD/DefaultOnEmpty-topped inner reports
// "non-empty" even with no match → EXISTS flips toward always-true. The gate must
// DECLINE those (fail-closed), while still admitting the emptiness-preserving
// scan/index through Filter/Fetch. Re-adding the FOD/DefaultOnEmpty peel arms
// reddens the two `false` rows (red-first). Defensive: the common existential
// inner is a scan/filter before the fold's own FOD wrap; a FOD-topped inner is
// not known SQL-reachable today, so this is the structural sentinel.
func TestExistInnerIsScanSafe(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"E"}, commit2RecType("E", "EID"), false)
	null := values.NewNullValue(values.UnknownType)
	cases := []struct {
		name string
		plan plans.RecordQueryPlan
		want bool
	}{
		{"scan", scan, true},
		{"index", plans.NewRecordQueryIndexPlan("idx", nil, []string{"E"}, commit2RecType("E", "EID"), false), true},
		{"predicatesFilter(scan)", plans.NewRecordQueryPredicatesFilterPlan(scan, nil), true},
		{"filter(scan)", plans.NewRecordQueryFilterPlan(nil, scan), true},
		// The P2 peel widening: an UNCORRELATED `EXISTS (SELECT 1 FROM t)` plans
		// as Projection(1, TypeFilter(Scan)) — both row-count-preserving, outside
		// the guard's hazard set. Deleting either peel arm reddens these (RED) and
		// regresses the shape to a 0AF00 decline (the caught P4b regression).
		{"projection(scan)", plans.NewRecordQueryProjectionPlan([]values.Value{values.LiteralValue(int64(1))}, scan), true},
		{"typeFilter(scan)", plans.NewRecordQueryTypeFilterPlan([]string{"E"}, scan), true},
		// The FIX — emit-on-empty wrappers destroy the emptiness signal → decline.
		// Re-adding either peel arm to existInnerIsScanSafe flips these to true (RED).
		{"firstOrDefault(scan)", plans.NewRecordQueryFirstOrDefaultPlan(scan, null), false},
		{"defaultOnEmpty(scan)", plans.NewRecordQueryDefaultOnEmptyPlan(scan, null), false},
		// A join inner (its own ON is not re-enforced by the merged-row fold) → decline.
		{"nlj(scan,scan)", plans.NewRecordQueryNestedLoopJoinPlan(scan, scan, nil, plans.JoinInner, "E", "E2", nil), false},
		// COMPOSITIONAL safety pole: an emit-on-empty / join inner still declines
		// even BEHIND a peeled Projection/TypeFilter — the loop peels the outer
		// row-preserving wrapper, then hits the hazard node and defaults false. The
		// peel widening must not open a back door for existence manufacture.
		{"projection(firstOrDefault(scan))", plans.NewRecordQueryProjectionPlan([]values.Value{values.LiteralValue(int64(1))}, plans.NewRecordQueryFirstOrDefaultPlan(scan, null)), false},
		{"typeFilter(nlj(scan,scan))", plans.NewRecordQueryTypeFilterPlan([]string{"E"}, plans.NewRecordQueryNestedLoopJoinPlan(scan, scan, nil, plans.JoinInner, "E", "E2", nil)), false},
	}
	for _, c := range cases {
		if got := existInnerIsScanSafe(c.plan); got != c.want {
			t.Errorf("existInnerIsScanSafe(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}
