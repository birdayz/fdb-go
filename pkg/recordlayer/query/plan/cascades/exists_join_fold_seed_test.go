package cascades

// White-box pins for the projected-EXISTS-over-join ordinalization helpers:
// the step-1 seed reconstruction and the leg-eligibility gate. A functional
// (row-equality) test is BLIND to a silent revert to the name model — the
// name-model rows are correct too, so a functional diff would agree either
// way — so the ordinal path's activation must be pinned structurally here,
// not only by row equality.

import (
	"sort"
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
// leg-concat ordinal seed the step-1 NLJ builds from — a non-anchored RC of
// baked ofOrdinal references, byte-compatible with buildOrdinalJoinResultValue,
// which BOTH layout twins accept (full coverage). The projection is never a
// windows source; the seed is.
func TestReconstructFoldStep1Seed(t *testing.T) {
	t.Parallel()
	t1 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
	t2 := plans.NewRecordQueryScanPlan([]string{"T2"}, commit2RecType("T2", "ID", "T1_ID"), false)

	seed, decline := reconstructFoldStep1Seed(t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"))
	if seed == nil {
		t.Fatal("two scan legs must reconstruct an ordinal step-1 seed")
	}
	if decline != (foldStep1LegDecline{}) {
		t.Fatalf("an ACCEPTED reconstruction must state no decline, got %+v — the census "+
			"sub-partitions reconstruct-nil firings by this value, so a decline stated "+
			"alongside a seed files a firing that did not happen", decline)
	}
	rc, ok := seed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("seed must be a RecordConstructorValue, got %T", seed)
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
	if w[values.NamedCorrelationIdentifier("T1")].Offset != 0 {
		t.Fatalf("T1 window offset = %d, want 0", w[values.NamedCorrelationIdentifier("T1")].Offset)
	}
	if w[values.NamedCorrelationIdentifier("T2")].Offset != 2 {
		t.Fatalf("T2 window offset = %d, want 2 (after T1's two columns)", w[values.NamedCorrelationIdentifier("T2")].Offset)
	}
}

// legOrdinalSafety admits a single-source scan leg (its rows are one namespace,
// ordinal-positionable) and REJECTS a name-model merged-row leg (a join), which
// stays name-model — the executor twin of the translator gate's ordinalEligible
// (correct-or-conservative). This is the guard that keeps the step-1 seed from
// mis-positioning a leg whose rows are a dotted-keyed merged row.
func TestLegOrdinalSafety(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, commit2RecType("T", "ID"), false)
	if safe, _ := legOrdinalSafety(scan); !safe {
		t.Fatal("a scan leg must be ordinal-safe")
	}
	// An INDEX / covering-index leg is also a single source — ordinal-safe. The
	// realistic shape: a covering index gets picked for a fold leg. If its flowed
	// type IS a record it ordinalizes consistently (seed typed by flowedType, the
	// NLJ builds from the same seed); if it is NOT a record the reconstruction
	// declines below and the leg stays name-model — never a silent-wrong path.
	idxRec := plans.NewRecordQueryIndexPlan("idx", nil, []string{"T2"}, commit2RecType("T2", "ID"), false)
	if safe, _ := legOrdinalSafety(idxRec); !safe {
		t.Fatal("an index-scan leg is a single source — ordinal-safe")
	}
	if s, _ := reconstructFoldStep1Seed(scan, idxRec, values.NamedCorrelationIdentifier("T"), values.NamedCorrelationIdentifier("T2")); s == nil {
		t.Fatal("two single-source legs (scan + record-typed index) must reconstruct a seed")
	}
	// A covering index whose flowed type is NOT a record: ordinal-safe by shape,
	// but the reconstruction DECLINES (leg type is not addressable positionally)
	// — the leg keeps the name model, correct-or-conservative.
	idxOpaque := plans.NewRecordQueryIndexPlan("idx", nil, []string{"T2"}, values.UnknownType, false)
	opaqueSeed, opaqueDecline := reconstructFoldStep1Seed(scan, idxOpaque, values.NamedCorrelationIdentifier("T"), values.NamedCorrelationIdentifier("T2"))
	if opaqueSeed != nil {
		t.Fatal("a non-record-typed index leg must decline the seed reconstruction (name model)")
	}
	// Both legs were ordinal-SAFE by shape and the concat still failed, so this
	// decline belongs in the census's no-unsafe-leg bucket and NOT in a
	// refused-leg-shape bucket. The distinction is what keeps RFC-200's exact
	// equality on positional-merge legs from absorbing a residue with a different
	// fix.
	if opaqueDecline.Shape != foldStep1LegShapeNone {
		t.Fatalf("an opaque index leg declined with shape %v, want %v — this nil comes from "+
			"BELOW legOrdinalSafety (planBuriedLegConcat could not state the leg's row), so "+
			"filing it under a refused-leg shape would attribute it to a population it is "+
			"not part of", opaqueDecline.Shape, foldStep1LegShapeNone)
	}
	// A filter over a scan (a leg with a pushed predicate) unwraps to the scan.
	filtered := plans.NewRecordQueryPredicatesFilterPlan(scan, nil)
	if safe, _ := legOrdinalSafety(filtered); !safe {
		t.Fatal("a filter over a scan unwraps to a single source — ordinal-safe")
	}
	// A join leg emits a name-model merged row — NOT ordinal-safe.
	nlj := plans.NewRecordQueryNestedLoopJoinPlan(scan, scan, nil, plans.JoinInner, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), nil)
	if safe, _ := legOrdinalSafety(nlj); safe {
		t.Fatal("a name-model join leg must NOT be ordinal-safe (it stays name-model)")
	}
	// A reconstruction over a join leg must therefore DECLINE (nil), keeping the
	// name model for that shape.
	nljSeed, nljDecline := reconstructFoldStep1Seed(nlj, scan, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("T"))
	if nljSeed != nil {
		t.Fatal("a join leg must decline the seed reconstruction")
	}
	// Exactly ONE leg was refused, and the census's per-firing sub-partition
	// records the first refused leg on the premise that this is always so. The
	// premise came from a removed probe's own breakdown and nothing was checking
	// it; this is the unit half of that check, the corpus half being the census's
	// both-legs-unsafe zero.
	if nljDecline.BothLegsUnsafe {
		t.Fatal("only the join leg is ordinal-unsafe here, but the decline reports BOTH legs unsafe")
	}
	if nljDecline.Shape != foldStep1LegShapeOther {
		t.Fatalf("a nil-result-value join leg classified as %v, want %v — this fixture's NLJ "+
			"carries no result value at all, which is neither the bare-QOV residue nor the "+
			"positional merge", nljDecline.Shape, foldStep1LegShapeOther)
	}
}

// foldStep1Seed is the exact gate the rule wires: it must ORDINALIZE
// (gated=true, step1RV=seed) a projected-EXISTS fold over independent scan legs,
// and DECLINE (gated=false, step1RV=the original RV) for each disqualifier — a
// wiring pin a plain row-equality test cannot provide (a name-model revert
// still leaves the rows correct).
func TestFoldStep1SeedGate(t *testing.T) {
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
	rv, gated, class := foldStep1Seed(foldRV, existAlias, false, t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"))
	if !gated {
		t.Fatal("a projected-EXISTS fold over independent scan legs must ordinalize (gated=true)")
	}
	if class.Step1 != foldStep1Accept {
		t.Fatalf("a gated fold classified as %v, want ACCEPT — the class is what the census "+
			"partitions on AND what is threaded to the leg rebase sites, so a wrong class "+
			"is a wrong number in both instruments", class)
	}
	if _, isRC := rv.(*values.RecordConstructorValue); !isRC || rv == foldRV {
		t.Fatal("gated step1RV must be the reconstructed seed, not the projection")
	}
	if w, _ := ordinalSeedLegWindowsOf(rv); w == nil {
		t.Fatal("the gated step1RV must be a windows-yielding ordinal seed")
	}

	// DECLINE, correlated step 1: stays name-model. A correlated FlatMap binds
	// legs by NAME, so a baked seed would hit a loud BakedNameContextError.
	if _, g, c := foldStep1Seed(foldRV, existAlias, true, t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2")); g || c.Step1 != foldStep1DeclineCorrelatedStep1 {
		t.Fatalf("a correlated step-1 must NOT ordinalize and must classify as the "+
			"correlatedStep1 wall, got gated=%t class=%v", g, c)
	}

	// DECLINE, not a projected fold (WHERE-EXISTS pass-through — RV is bare QOV,
	// no existential reference): stays name-model, RV unchanged.
	bareRV := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T1"))
	if rv2, g, c := foldStep1Seed(bareRV, existAlias, false, t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2")); g || rv2 != bareRV || c.Step1 != foldStep1DeclineNoExistRef {
		t.Fatalf("a non-fold RV must NOT ordinalize, must pass through unchanged, and must "+
			"classify as rv-no-exist-ref (a correct pass-through, not a residue), got "+
			"gated=%t class=%v", g, c)
	}

	// DECLINE, a non-scan (join) leg: stays name-model.
	njoin := plans.NewRecordQueryNestedLoopJoinPlan(t1, t2, []predicates.QueryPredicate(nil), plans.JoinInner, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"), nil)
	if _, g, c := foldStep1Seed(foldRV, existAlias, false, njoin, t2, values.NamedCorrelationIdentifier("J"), values.NamedCorrelationIdentifier("T2")); g || c.Step1 != foldStep1DeclineReconstructNil {
		t.Fatalf("a name-model join leg must NOT ordinalize and must classify as "+
			"reconstruct-nil, got gated=%t class=%v", g, c)
	}
}

// The seed's QOV correlation must be the leg identifier the caller threaded,
// VERBATIM. The reconstruction used to mint it as
// NamedCorrelationIdentifier(ToUpper(alias)) from a plan-level alias string, and
// that upper fold was a forgery generator rather than a normalization: the
// machine namespace is LOWERCASE (UniqueCorrelationIdentifier mints q$N), so a
// minted leg came out of here spelled Q$N.
//
// The consequence was an internally inconsistent plan, not merely an ugly name.
// The seed's QOVs would say Q$N while the join plan's own leg identity said q$N,
// and the runtime leg binder compares the two through values.SameLeg, which is
// EXACT — so the leg would fail to bind and a correlated read over it would fall
// through to a whole-row positional read or decline. This is measurable: over the
// FDB corpus the source-alias slice and the quantifier identifier disagree on 12
// of 79040 planner firings, and in those the alias is a minted q$N (witnesses
// "q$N vs E"), which is exactly the population the fold used to mangle.
//
// A minted, lowercase alias is therefore the shape this must be tested with. An
// already-upper alias makes the fold a no-op and the test vacuous.
func TestReconstructFoldStep1Seed_CarriesTheThreadedIdentityVerbatim(t *testing.T) {
	t.Parallel()

	minted := values.UniqueCorrelationIdentifier() // lowercase q$N, by construction
	other := values.NamedCorrelationIdentifier("T2")
	t1 := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
	t2 := plans.NewRecordQueryScanPlan([]string{"T2"}, commit2RecType("T2", "ID", "T1_ID"), false)

	seed, _ := reconstructFoldStep1Seed(t1, t2, minted, other)
	if seed == nil {
		t.Fatal("two scan legs must reconstruct an ordinal step-1 seed")
	}
	rc, ok := seed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("seed must be a RecordConstructorValue, got %T", seed)
	}

	seen := map[values.CorrelationIdentifier]bool{}
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*values.FieldValue)
		if !isFV {
			t.Fatalf("field[%d] is %T, want *values.FieldValue", i, f.Value)
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			t.Fatalf("field[%d] child is %T, want a QuantifiedObjectValue", i, fv.Child)
		}
		seen[qov.Correlation] = true
	}
	for _, want := range []values.CorrelationIdentifier{minted, other} {
		if !seen[want] {
			t.Errorf("no seed field reads leg %q; the seed's correlations are %v.\n"+
				"The reconstruction must carry the threaded identifier VERBATIM. Folding it "+
				"(ToUpper) re-spells a minted q$N as Q$N, and the join plan's own leg "+
				"identity still says q$N — values.SameLeg is exact, so the leg then fails "+
				"to bind at runtime and a correlated read over it silently reads the wrong "+
				"row or declines.", want.Name(), keysOfCorrSet(seen))
		}
	}
	// And the leg BOUNDARIES the walk stamps must carry the same identity the QOVs
	// do. An identity that matched the QOV but not the leg window would bind
	// nothing just as surely.
	fields, legs, ok := planBuriedLegConcat(t1, minted, 0)
	if !ok || len(fields) == 0 {
		t.Fatal("a scan leg must yield a concat window")
	}
	if len(legs) != 1 || legs[0].Alias != minted {
		t.Errorf("leg boundary Alias = %v, want the threaded %q — the seed's QOV and the "+
			"leg window must name the SAME leg or nothing binds",
			legs, minted.Name())
	}
}

// keysOfCorrSet renders a correlation set for a failure message.
func keysOfCorrSet(m map[values.CorrelationIdentifier]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.Name())
	}
	sort.Strings(out)
	return out
}
