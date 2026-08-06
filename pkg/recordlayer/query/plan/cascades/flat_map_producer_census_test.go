package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The witness must report the declined leg's flowed type SPELLING, not just a
// boolean.
//
// This is the dimension that turned a booked conversion into a refutation. The
// residue was booked as "bare UNTYPED QuantifiedObjectValue result values, where
// Java types unconditionally" and the whole prescribed fix was to type them.
// Measured over the real-FDB corpus with this spelling in place, all 102
// declined legs carry a real RecordType of arity 1-3 — there is nothing to type,
// and the refusal is on SHAPE (values.IsPositionalMergeRC needs a
// *RecordConstructorValue, which no QOV is at any typing).
//
// A boolean could not have said that. `typed=true` is compatible with a
// zero-field record, an UnknownType degraded elsewhere, or a scalar; the arity
// is what makes "these are real rows the layout authority still cannot position"
// a measurement instead of a reading. So the spelling is asserted, in both the
// record and the non-record direction.
func TestFlatMapProducerCensus_WitnessSpellsTheFlowedType(t *testing.T) {
	t.Parallel()

	twoCol := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0}, {Name: "K", Ordinal: 1},
	}}
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, twoCol, false)
	corr := values.NamedCorrelationIdentifier("A")

	for _, tc := range []struct {
		name string
		rv   values.Value
		want string
	}{
		{
			// The measured shape of the residue: a real row type with a real arity.
			name: "record type reports its ARITY",
			rv:   values.NewQuantifiedObjectValueOfType(corr, twoCol),
			want: "rvtype=RecordType(2)",
		},
		{
			// Without this direction the arity branch could be the only branch and
			// an untyped value would print as RecordType(0) rather than as unknown.
			name: "untyped reports UNKNOWN, not an arity",
			rv:   values.NewQuantifiedObjectValue(corr),
			want: "rvtype=UNKNOWN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fm := plans.NewRecordQueryFlatMapPlan(scan, scan,
				values.NamedCorrelationIdentifier("O"), values.NamedCorrelationIdentifier("I"), tc.rv, false)
			shape, witness := classifyDeclinedLeg(fm)
			if shape != foldStep1LegShapeBareQOV {
				t.Fatalf("shape = %v, want bare-QOV — the type spelling is a cut WITHIN that "+
					"bucket, so a case that leaves it exercises nothing (witness %q)", shape, witness)
			}
			if !strings.Contains(witness, tc.want) {
				t.Fatalf("witness %q does not report %q.\n"+
					"This spelling is what refuted the typing conversion: a boolean reported "+
					"typed=true for a population and could not say the values were real rows "+
					"of arity 1-3, which is the fact that makes the residue a SHAPE problem "+
					"rather than a typing one.", witness, tc.want)
			}
		})
	}
}

// Producer attribution must come from the value's own identity, and must say so
// when it cannot tell.
//
// The alternative that was NOT taken is reading the per-site arity histogram and
// matching it against the declined legs. That closes only the arities appearing
// at exactly one site and leaves the rest to judgement — the class of argument
// this census family exists to replace. Two directions are pinned because the
// map has two ways to lie: it can forget a value (reporting a real producer as
// unknown) and it can pick one of two producers silently.
func TestFlatMapProducerCensus_OriginIsMeasuredNotInferred(t *testing.T) {
	t.Parallel()

	rowType := &values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0}}}
	corr := values.NamedCorrelationIdentifier("A")

	// The census keys a package-global map, so this test cannot run in parallel
	// with another that records into it. It records values it constructs itself
	// and asserts only about those, so a concurrent recorder adds entries without
	// changing any answer below.
	recorded := values.NewQuantifiedObjectValueOfType(corr, rowType)
	shared := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), rowType)
	absent := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("C"), rowType)

	recordFlatMapResultValue(flatMapSiteCorrelated, recorded)
	recordFlatMapResultValue(flatMapSiteYieldExistsFlatMap, shared)
	recordFlatMapResultValue(flatMapSiteExistentialSelect, shared)

	if got := describeFlatMapResultOrigin(recorded); got != "origin=buildCorrelatedFlatMapPlan" {
		t.Fatalf("origin of a value recorded at ONE site = %q, want "+
			"origin=buildCorrelatedFlatMapPlan.\n"+
			"This attribution is the deliverable: it is what says the reconstruct-nil "+
			"residue comes from buildCorrelatedFlatMapPlan — the site that emits ZERO "+
			"untyped values — rather than from the minting site the residue was "+
			"hypothesised to come from.", got)
	}
	if got := describeFlatMapResultOrigin(shared); got != "origin=AMBIGUOUS(>1 site)" {
		t.Fatalf("origin of a value recorded at TWO sites = %q, want origin=AMBIGUOUS(>1 site).\n"+
			"Last-writer-wins would name one of two producers with no indication that "+
			"the other exists, which is worse than an admitted unknown: the whole point "+
			"of measuring attribution is to stop reporting a guess as a fact.", got)
	}
	if got := describeFlatMapResultOrigin(absent); got != "origin=UNRECORDED" {
		t.Fatalf("origin of a NEVER-recorded value = %q, want origin=UNRECORDED.\n"+
			"An unrecorded value must not borrow a neighbour's attribution — the two "+
			"NestedLoopJoin-legged declines in the corpus are legitimately unrecorded "+
			"(their result value never passes a FlatMap constructor) and must read as "+
			"such rather than as some FlatMap site's output.", got)
	}
}

// The producer census's assertion arms, driven from counter states.
//
// Each arm is a claim about which states FAIL. Driving the planner into those
// states is not possible for most of them — the whole finding is that the corpus
// does not produce them — so the gate is exercised directly, exactly as the
// sibling censuses' arm tests do.
func TestFlatMapProducerCensus_AssertionArmsGoRed(t *testing.T) {
	t.Parallel()

	base := func() flatMapProducerCounters {
		var c flatMapProducerCounters
		c.Calls = [flatMapProducerSiteCount]int{25644, 1830, 431, 449}
		c.TypedQOV = [flatMapProducerSiteCount]int{476, 0, 0, 130}
		c.UntypedQOV = [flatMapProducerSiteCount]int{0, 1685, 249, 269}
		c.MergeRC = [flatMapProducerSiteCount]int{6732, 0, 0, 0}
		return c
	}
	floors := &FlatMapProducerFloors{
		Calls:           [flatMapProducerSiteCount]int{2000, 150, 40, 40},
		UntypedQOVFloor: [flatMapProducerSiteCount]int{0, 150, 20, 20},
	}

	t.Run("the measured state PASSES", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		if assertFlatMapProducerCounters(&b, base(), floors) {
			t.Fatalf("the state measured over the real-FDB corpus must pass its own gate, "+
				"or every red below is measuring the gate rather than the population:\n%s", b.String())
		}
	})

	t.Run("an UNTYPED value at the residue's producer is RED", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.UntypedQOV[flatMapSiteCorrelated] = 1
		var b strings.Builder
		if !assertFlatMapProducerCounters(&b, c, floors) {
			t.Fatal("a single untyped QOV at buildCorrelatedFlatMapPlan must fail. That zero " +
				"is what the refutation of the typing conversion rests on — the residue's " +
				"own producer never emits an untyped value — and a zero nothing checks is " +
				"the exact failure this census family exists to prevent.")
		}
		if !strings.Contains(b.String(), "want 0") {
			t.Fatalf("failure message does not state the expectation: %s", b.String())
		}
	})

	t.Run("the untyped population DISAPPEARING is RED", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.UntypedQOV[flatMapSiteExistentialSelect] = 0
		var b strings.Builder
		if !assertFlatMapProducerCounters(&b, c, floors) {
			t.Fatal("the untyped population at implementExistentialSelect going to zero must " +
				"fail. It is a floor on a DIVERGENCE: Java cannot build an untyped QOV at " +
				"all, so this population is a real gap, and a silent drop is either the gap " +
				"closing or the site going dark — indistinguishable from a smaller number.")
		}
	})

	t.Run("a site going DARK is RED", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.Calls[flatMapSiteJoinWithExistential] = 0
		c.UntypedQOV[flatMapSiteJoinWithExistential] = 0
		var b strings.Builder
		if !assertFlatMapProducerCounters(&b, c, floors) {
			t.Fatal("a producer site making zero constructions must fail — otherwise every " +
				"zero recorded beside it reads as an absent SHAPE when it is an absent " +
				"population.")
		}
	})
}
