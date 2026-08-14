package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func flatMapCensusRowType(columns ...string) *values.RecordType {
	fields := make([]values.Field, len(columns))
	for i, column := range columns {
		fields[i] = values.Field{Name: column, FieldType: values.NotNullLong, Ordinal: i}
	}
	return values.NewRecordType("FlatMapCensusRow", false, fields)
}

func flatMapCensusQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	flowedType values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, flowedType)
	return mustConstruct(t, qov, err)
}

func flatMapCensusScan(
	t testing.TB,
	flowedType values.Type,
) *plans.RecordQueryScanPlan {
	t.Helper()
	plan, err := plans.NewRecordQueryScanPlan([]string{"T"}, flowedType, false)
	return mustConstruct(t, plan, err)
}

func flatMapCensusPlan(
	t testing.TB,
	leg plans.RecordQueryPlan,
	resultValue values.Value,
) *plans.RecordQueryFlatMapPlan {
	t.Helper()
	plan, err := plans.NewRecordQueryFlatMapPlan(
		leg, leg,
		values.NamedCorrelationIdentifier("O"), values.NamedCorrelationIdentifier("I"),
		resultValue, false)
	return mustConstruct(t, plan, err)
}

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
// A boolean could not have said that. `typed=true` is compatible with both a
// record and a scalar; the arity is what makes "these are real rows the layout
// authority still cannot position" a measurement instead of a reading. Exact
// QOV admission has retired the old untyped construction, so the opposite arm
// is now an exact scalar and must spell its concrete code rather than an arity.
func TestFlatMapProducerCensus_WitnessSpellsTheFlowedType(t *testing.T) {
	t.Parallel()

	twoCol := flatMapCensusRowType("ID", "K")
	scan := flatMapCensusScan(t, twoCol)
	corr := values.NamedCorrelationIdentifier("A")

	for _, tc := range []struct {
		name string
		rv   values.Value
		want string
	}{
		{
			// The measured shape of the residue: a real row type with a real arity.
			name: "record type reports its ARITY",
			rv:   flatMapCensusQOV(t, corr, twoCol),
			want: "rvtype=RecordType(2)",
		},
		{
			// Without this direction the arity branch could be the only branch and
			// a scalar could be misleadingly printed as a zero-field record.
			name: "scalar reports LONG, not an arity",
			rv:   flatMapCensusQOV(t, corr, values.NotNullLong),
			want: "rvtype=LONG",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fm := flatMapCensusPlan(t, scan, tc.rv)
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

	rowType := flatMapCensusRowType("ID")
	corr := values.NamedCorrelationIdentifier("A")

	// The census keys a package-global map, and this test IS parallel-safe
	// anyway — the earlier note claiming it "cannot run in parallel" contradicted
	// the t.Parallel() above it and then argued the opposite in its own next
	// sentence. The reason it is safe: every value below is constructed here and
	// asserted about only here, so a concurrent recorder can add entries to the
	// map but cannot change the answer for a value it has never seen. What would
	// break that is an assertion about the map's SIZE or about a value this test
	// did not build; there is none.
	recorded := flatMapCensusQOV(t, corr, rowType)
	shared := flatMapCensusQOV(t, values.NamedCorrelationIdentifier("B"), rowType)
	absent := flatMapCensusQOV(t, values.NamedCorrelationIdentifier("C"), rowType)

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
		// One real-FDB corpus run, verbatim — including OtherRV, so the state
		// PARTITIONS. An earlier copy of these figures had calls that did not equal
		// the sum of the arms at any site, which made "the measured state passes"
		// an assertion about a state the planner has never been in.
		c.Calls = [flatMapProducerSiteCount]int{25406, 1754, 431, 449}
		c.TypedQOV = [flatMapProducerSiteCount]int{476, 0, 0, 130}
		c.UntypedQOV = [flatMapProducerSiteCount]int{0, 1609, 249, 269}
		c.MergeRC = [flatMapProducerSiteCount]int{6732, 0, 0, 0}
		c.OtherRV = [flatMapProducerSiteCount]int{18198, 145, 182, 50}
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
		// The untyped count is held ABOVE its floor (20) while the call count is
		// dropped BELOW its own (40), so the CALL floor is the only arm that can
		// fire. Zeroing both — which this subtest did on first writing — trips the
		// untyped floor first and passes without ever reaching the call floor: the
		// test would stay green with the dark-site arm deleted outright.
		c := base()
		c.Calls[flatMapSiteJoinWithExistential] = 30
		c.UntypedQOV[flatMapSiteJoinWithExistential] = 25
		var b strings.Builder
		if !assertFlatMapProducerCounters(&b, c, floors) {
			t.Fatal("a producer site making too few constructions must fail — otherwise every " +
				"zero recorded beside it reads as an absent SHAPE when it is an absent " +
				"population.")
		}
		if !strings.Contains(b.String(), "gone dark") {
			t.Fatalf("the CALL floor must be the arm that fired, not the untyped floor "+
				"standing in for it — that substitution is what made this subtest vacuous:\n%s",
				b.String())
		}
	})
}

// The MINT must win over the courier in a producer attribution.
//
// The FlatMap producer census names the construction that handed a value to a
// plan. Three of its four sites BUILD nothing: implementExistentialSelect,
// yieldExistsFlatMap and buildCorrelatedFlatMapPlan flow sel.GetResultValue()
// verbatim, exactly as Java's three RecordQueryFlatMapPlan constructions flow
// selectExpression.getResultValue() (ImplementNestedLoopJoinRule.java:187, 201,
// 214). So their counts are a count of TRAFFIC through a courier, and reading
// them as production is what booked the untyped-QOV divergence against sites
// that only carried it.
//
// The author is whatever fills sel.GetResultValue(), which on the EXISTS path is
// the SQL translator's mint. This asserts the witness says so — and that it
// still names the courier too, because the courier is what put the value in a
// plan and the two are different facts.
func TestFlatMapProducerCensus_MintOutranksTheCourier(t *testing.T) {
	// NOT parallel: it drives the process-global census flag.
	restore := values.LegIdentityCensusEnabled()
	values.SetLegIdentityCensusEnabled(true)
	defer values.SetLegIdentityCensusEnabled(restore)

	rowType := flatMapCensusRowType("ID")
	minted := flatMapCensusQOV(t, values.NamedCorrelationIdentifier("MINTED"), rowType)
	values.RecordSelectResultMint(values.SelectResultMintExistsSelect, minted)
	recordFlatMapResultValue(flatMapSiteExistentialSelect, minted)

	got := describeFlatMapResultOrigin(minted)
	if !strings.Contains(got, "mint=") {
		t.Fatalf("origin of a MINTED value = %q, want it to name the mint.\n"+
			"A site that flows sel.GetResultValue() verbatim did not build the value it "+
			"passed on. Crediting it is the inference-instead-of-measurement this census "+
			"family exists to kill: it names a courier as the author, and the divergence "+
			"then gets booked against a site that cannot fix it.", got)
	}
	if !strings.Contains(got, "via=implementExistentialSelect") {
		t.Fatalf("origin of a MINTED value = %q, want the courier named too.\n"+
			"The mint says who built it and the courier says what put it in a plan. "+
			"Dropping the second would trade one incomplete attribution for another.", got)
	}

	// A value with NO mint still reports its courier — the population that reaches
	// the decline classifier is exactly this kind, and it must not go dark when
	// the mint lookup is added ahead of it.
	unminted := flatMapCensusQOV(t, values.NamedCorrelationIdentifier("UNMINTED"), rowType)
	recordFlatMapResultValue(flatMapSiteCorrelated, unminted)
	if got := describeFlatMapResultOrigin(unminted); got != "origin=buildCorrelatedFlatMapPlan" {
		t.Fatalf("origin of an UNMINTED value = %q, want origin=buildCorrelatedFlatMapPlan. "+
			"Every FlatMap-legged decline in the corpus is this shape; if the mint lookup "+
			"shadowed it, the one attribution the residue rests on would silently vanish", got)
	}
}

// The producer counters must be a SNAPSHOT, maps included.
//
// assertFlatMapProducerCounters takes an explicit state so a failure message
// quotes the state that FAILED rather than re-reading globals a concurrent run
// has moved. Returning the struct by value copies the arrays and SHARES the maps
// inside them, which defeats that silently — and iterating a shared map while a
// planner writes it is not a stale number, it is a fatal concurrent map
// iteration and map write.
func TestFlatMapProducerCensus_SnapshotDeepCopiesShapes(t *testing.T) {
	// NOT parallel: it records into the process-global census.
	restore := values.LegIdentityCensusEnabled()
	values.SetLegIdentityCensusEnabled(true)
	defer values.SetLegIdentityCensusEnabled(restore)

	rowType := flatMapCensusRowType("ID")
	recordFlatMapResultValue(flatMapSiteYieldExistsFlatMap,
		flatMapCensusQOV(t, values.NamedCorrelationIdentifier("SNAPA"), rowType))

	snap := FlatMapProducerCensus()
	shapes := snap.Shapes[flatMapSiteYieldExistsFlatMap]
	if len(shapes) == 0 {
		t.Fatal("no shape recorded, so this test is asserting about an empty map")
	}
	var key string
	for k := range shapes {
		key = k
		break
	}
	was := shapes[key]

	recordFlatMapResultValue(flatMapSiteYieldExistsFlatMap,
		flatMapCensusQOV(t, values.NamedCorrelationIdentifier("SNAPB"), rowType))

	if got := snap.Shapes[flatMapSiteYieldExistsFlatMap][key]; got != was {
		t.Fatalf("shape %q in a SNAPSHOT moved %d -> %d when the census was written again.\n"+
			"The snapshot shares the live map, so the 'renders an EXPLICIT counter state' "+
			"split buys nothing for the shapes and a concurrent planner turns the same "+
			"sharing into a fatal concurrent map iteration and map write", key, was, got)
	}
}
