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

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func commit2RecType(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

func commit2Scan(
	t testing.TB,
	recordTypes []string,
	flowedType values.Type,
) *plans.RecordQueryScanPlan {
	t.Helper()
	plan, err := plans.NewRecordQueryScanPlan(recordTypes, flowedType, false)
	return mustConstruct(t, plan, err)
}

func commit2IndexScan(
	t testing.TB,
	indexName string,
	recordTypes []string,
	flowedType values.Type,
) *plans.RecordQueryIndexPlan {
	t.Helper()
	plan, err := plans.NewRecordQueryIndexPlan(indexName, nil, recordTypes, flowedType, false)
	return mustConstruct(t, plan, err)
}

func commit2Filter(
	t testing.TB,
	inner plans.RecordQueryPlan,
) *plans.RecordQueryPredicatesFilterPlan {
	t.Helper()
	plan, err := plans.NewRecordQueryPredicatesFilterPlan(inner, nil)
	return mustConstruct(t, plan, err)
}

func commit2QOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	flowedType values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, flowedType)
	return mustConstruct(t, qov, err)
}

func commit2NLJ(
	t testing.TB,
	outer, inner plans.RecordQueryPlan,
	outerAlias, innerAlias values.CorrelationIdentifier,
) *plans.RecordQueryNestedLoopJoinPlan {
	t.Helper()
	// A bare exact QOV deliberately states a name-model/opaque join result, not
	// an ordinal RecordConstructorValue. legOrdinalSafety must reject it.
	resultValue := commit2QOV(t, values.NamedCorrelationIdentifier("JOIN_RESULT"), outer.GetResultType())
	plan, err := plans.NewRecordQueryNestedLoopJoinPlan(
		outer, inner, nil, plans.JoinInner, outerAlias, innerAlias, resultValue)
	return mustConstruct(t, plan, err)
}

// A gated projected-EXISTS fold over two SCAN legs reconstructs the FULL
// leg-concat ordinal seed the step-1 NLJ builds from — a non-anchored RC of
// baked ofOrdinal references, byte-compatible with buildOrdinalJoinResultValue,
// which BOTH layout twins accept (full coverage). The projection is never a
// windows source; the seed is.
func TestReconstructFoldStep1Seed(t *testing.T) {
	t.Parallel()
	t1 := commit2Scan(t, []string{"T1"}, commit2RecType("T1", "ID", "V"))
	t2 := commit2Scan(t, []string{"T2"}, commit2RecType("T2", "ID", "T1_ID"))

	seed, decline := reconstructFoldStep1Seed(t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"), plans.JoinInner)
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
		field, isField := values.AsFieldValue(f.Value)
		if !isField || field.Path() == nil || !field.Path().IsFrontierPinned() {
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

// TestReconstructFoldStep1SeedNullExtendsTheOuterJoinSide drives the
// null-supplying arm of the step-1 seed for EVERY join kind, because the corpus
// reaches only some of them and the arm that ships untested is the one whose
// first real firing gets read as a finding.
//
// The seed's leg QOV must be null-extended on exactly the side the join kind
// pads. RecordQueryNestedLoopJoinPlan derives its own null-supplying aliases
// from the SAME kind and then looks the source up IN THIS SEED, refusing to
// build when the record it finds is not nullable — so a disagreement here is
// not a cosmetic type difference, it is a query that cannot be planned at all.
//
// Both directions are asserted per kind: the padded side nullable AND the
// preserved side not. Asserting only the padded side passes against a seed that
// null-extends everything, which would silently make every preserved leg
// look absent to the layout's presence proof.
func TestReconstructFoldStep1SeedNullExtendsTheOuterJoinSide(t *testing.T) {
	t.Parallel()
	left := values.NamedCorrelationIdentifier("L")
	right := values.NamedCorrelationIdentifier("R")

	for _, testCase := range []struct {
		name          string
		joinType      plans.JoinType
		leftNullable  bool
		rightNullable bool
	}{
		{name: "inner pads neither side", joinType: plans.JoinInner},
		{name: "left outer pads the RIGHT side", joinType: plans.JoinLeftOuter, rightNullable: true},
		{name: "full outer pads BOTH sides", joinType: plans.JoinFullOuter, leftNullable: true, rightNullable: true},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			l := commit2Scan(t, []string{"L"}, commit2RecType("L", "ID", "V"))
			r := commit2Scan(t, []string{"R"}, commit2RecType("R", "ID", "L_ID"))
			seed, decline := reconstructFoldStep1Seed(l, r, left, right, testCase.joinType)
			if seed == nil {
				t.Fatalf("two scan legs must reconstruct a seed, declined %+v", decline)
			}
			want := map[values.CorrelationIdentifier]bool{
				left: testCase.leftNullable, right: testCase.rightNullable,
			}
			seen := map[values.CorrelationIdentifier]bool{}
			values.WalkValue(seed, func(node values.Value) bool {
				qov, isQOV := values.AsQuantifiedObjectValue(node)
				if !isQOV {
					return true
				}
				alias := qov.Correlation()
				wantNullable, tracked := want[alias]
				if !tracked {
					t.Errorf("seed carries an unexpected leg %s", alias.Name())
					return true
				}
				seen[alias] = true
				if got := qov.FlowedType().IsNullable(); got != wantNullable {
					t.Errorf("%s leg row nullable = %t, want %t under %v — the physical join "+
						"reads its null-supplying source out of THIS seed and refuses to build "+
						"when the record it finds is not nullable",
						alias.Name(), got, wantNullable, testCase.joinType)
				}
				return true
			})
			if len(seen) != 2 {
				t.Fatalf("walked %d leg QOVs, want both legs — an assertion over an empty "+
					"population proves nothing", len(seen))
			}
		})
	}
}

// legOrdinalSafety admits a single-source scan leg (its rows are one namespace,
// ordinal-positionable) and REJECTS a name-model merged-row leg (a join), which
// stays name-model — the executor twin of the translator gate's ordinalEligible
// (correct-or-conservative). This is the guard that keeps the step-1 seed from
// mis-positioning a leg whose rows are a dotted-keyed merged row.
func TestLegOrdinalSafety(t *testing.T) {
	t.Parallel()
	scan := commit2Scan(t, []string{"T"}, commit2RecType("T", "ID"))
	if safe, _ := legOrdinalSafety(scan); !safe {
		t.Fatal("a scan leg must be ordinal-safe")
	}
	// An INDEX / covering-index leg is also a single source — ordinal-safe. The
	// realistic shape: a covering index gets picked for a fold leg. If its flowed
	// type IS a record it ordinalizes consistently (seed typed by flowedType, the
	// NLJ builds from the same seed); if it is NOT a record the reconstruction
	// declines below and the leg stays name-model — never a silent-wrong path.
	idxRec := commit2IndexScan(t, "idx", []string{"T2"}, commit2RecType("T2", "ID"))
	if safe, _ := legOrdinalSafety(idxRec); !safe {
		t.Fatal("an index-scan leg is a single source — ordinal-safe")
	}
	if s, _ := reconstructFoldStep1Seed(scan, idxRec, values.NamedCorrelationIdentifier("T"), values.NamedCorrelationIdentifier("T2"), plans.JoinInner); s == nil {
		t.Fatal("two single-source legs (scan + record-typed index) must reconstruct a seed")
	}
	// A covering index whose flowed type is NOT a record: ordinal-safe by shape,
	// but the reconstruction DECLINES (leg type is not addressable positionally)
	// — the leg keeps the name model, correct-or-conservative.
	idxOpaque := commit2IndexScan(t, "idx", []string{"T2"}, values.NotNullLong)
	opaqueSeed, opaqueDecline := reconstructFoldStep1Seed(scan, idxOpaque, values.NamedCorrelationIdentifier("T"), values.NamedCorrelationIdentifier("T2"), plans.JoinInner)
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
	filtered := commit2Filter(t, scan)
	if safe, _ := legOrdinalSafety(filtered); !safe {
		t.Fatal("a filter over a scan unwraps to a single source — ordinal-safe")
	}
	// A join leg emits a name-model merged row — NOT ordinal-safe.
	nlj := commit2NLJ(t, scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"))
	if safe, _ := legOrdinalSafety(nlj); safe {
		t.Fatal("a name-model join leg must NOT be ordinal-safe (it stays name-model)")
	}
	// A reconstruction over a join leg must therefore DECLINE (nil), keeping the
	// name model for that shape.
	nljSeed, nljDecline := reconstructFoldStep1Seed(nlj, scan, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("T"), plans.JoinInner)
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
	if nljDecline.Shape != foldStep1LegShapeBareQOV {
		t.Fatalf("an exact name-model join leg classified as %v, want %v — its bare QOV "+
			"states one opaque row rather than an ordinal positional merge",
			nljDecline.Shape, foldStep1LegShapeBareQOV)
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
	t1 := commit2Scan(t, []string{"T1"}, commit2RecType("T1", "ID"))
	t2 := commit2Scan(t, []string{"T2"}, commit2RecType("T2", "ID"))
	// A projected fold RV references the existential quantifier (the ExistsValue).
	t1Root := commit2QOV(t, values.NamedCorrelationIdentifier("T1"), t1.GetResultType())
	t1ID, err := values.ResolveFieldOrdinals(t1Root, []int{0})
	t1ID = mustConstruct(t, t1ID, err)
	existsValue, err := values.NewExistsValue(existAlias, t2.GetResultType())
	existsValue = mustConstruct(t, existsValue, err)
	foldRV := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: t1ID},
		values.RecordConstructorField{Name: "F", Value: existsValue},
	)

	// GATED: independent scan legs + a projected fold → the reconstructed seed.
	rv, gated, class := foldStep1Seed(foldRV, existAlias, false, t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"), plans.JoinInner)
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
	if _, g, c := foldStep1Seed(foldRV, existAlias, true, t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"), plans.JoinInner); g || c.Step1 != foldStep1DeclineCorrelatedStep1 {
		t.Fatalf("a correlated step-1 must NOT ordinalize and must classify as the "+
			"correlatedStep1 wall, got gated=%t class=%v", g, c)
	}

	// DECLINE, not a projected fold (WHERE-EXISTS pass-through — RV is bare QOV,
	// no existential reference): stays name-model, RV unchanged.
	bareRV := commit2QOV(t, values.NamedCorrelationIdentifier("T1"), t1.GetResultType())
	if rv2, g, c := foldStep1Seed(bareRV, existAlias, false, t1, t2, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"), plans.JoinInner); g || rv2 != bareRV || c.Step1 != foldStep1DeclineNoExistRef {
		t.Fatalf("a non-fold RV must NOT ordinalize, must pass through unchanged, and must "+
			"classify as rv-no-exist-ref (a correct pass-through, not a residue), got "+
			"gated=%t class=%v", g, c)
	}

	// DECLINE, a non-scan (join) leg: stays name-model.
	njoin := commit2NLJ(t, t1, t2,
		values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"))
	if _, g, c := foldStep1Seed(foldRV, existAlias, false, njoin, t2, values.NamedCorrelationIdentifier("J"), values.NamedCorrelationIdentifier("T2"), plans.JoinInner); g || c.Step1 != foldStep1DeclineReconstructNil {
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
	t1 := commit2Scan(t, []string{"T1"}, commit2RecType("T1", "ID", "V"))
	t2 := commit2Scan(t, []string{"T2"}, commit2RecType("T2", "ID", "T1_ID"))

	seed, _ := reconstructFoldStep1Seed(t1, t2, minted, other, plans.JoinInner)
	if seed == nil {
		t.Fatal("two scan legs must reconstruct an ordinal step-1 seed")
	}
	rc, ok := seed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("seed must be a RecordConstructorValue, got %T", seed)
	}

	seen := map[values.CorrelationIdentifier]bool{}
	for i, f := range rc.Fields {
		field, isField := values.AsFieldValue(f.Value)
		if !isField {
			t.Fatalf("field[%d] is %T, want *values.FieldValue", i, f.Value)
		}
		qov, isQOV := values.AsQuantifiedObjectValue(field.ChildValue())
		if !isQOV {
			t.Fatalf("field[%d] child is %T, want a QuantifiedObjectValue", i, field.ChildValue())
		}
		seen[qov.Correlation()] = true
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
