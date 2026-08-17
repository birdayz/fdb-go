package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// positionalMergeRCOverLegs builds the exact shape Java's PartitionSelectRule
// emits and Go's positionalMergeCase builds 1:1: one UNNAMED column per
// collapsed lower quantifier, each holding that quantifier's WHOLE row.
//
// It is a helper rather than an inline literal because three tests in this file
// and the census's own classification all have to agree on what "a positional
// merge" is, and the one place that decides is values.IsPositionalMergeRC. A
// fixture that drifted from it would make every assertion below vacuous.
func positionalMergeRCOverLegs(
	t testing.TB,
	legs ...*values.RecordType,
) *values.RecordConstructorValue {
	t.Helper()
	fields := make([]values.RecordConstructorField, len(legs))
	for i, rt := range legs {
		corr := values.NamedCorrelationIdentifier(strings.ToUpper(string(rune('A' + i))))
		qov, err := values.NewQuantifiedObjectValue(corr, rt)
		fields[i] = values.RecordConstructorField{
			Name:  values.OrdinalFieldName(i),
			Value: mustConstruct(t, qov, err),
		}
	}
	return values.NewRawRecordConstructorValue(fields...)
}

func foldStep1CensusQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	flowedType values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, flowedType)
	return mustConstruct(t, qov, err)
}

func foldStep1CensusScan(
	t testing.TB,
	flowedType values.Type,
) *plans.RecordQueryScanPlan {
	t.Helper()
	plan, err := plans.NewRecordQueryScanPlan([]string{"T"}, flowedType, false)
	return mustConstruct(t, plan, err)
}

func foldStep1CensusFlatMap(
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

// The refused-leg classifier separates the two shapes RFC-200's exact
// equalities are stated against, and separates BOTH from an RC that merely looks
// like a merge.
//
// The third case is the one worth having: a NAMED 2-slot RC of bare typed leg
// QOVs is shape-adjacent to a positional merge and is NOT one
// (values.IsPositionalMergeRC requires the auto-generated `_i` names in position
// order). If it were bucketed with the merge, gate (c)'s "reconstruct-nil /
// positional-merge == 60" could be satisfied by a population that is not the
// one the nested window admits, and the equality would be measuring the wrong
// thing while reading green.
func TestFoldStep1Census_ClassifiesTheRefusedLegByItsResultValue(t *testing.T) {
	t.Parallel()

	legA := values.NewRecordType("LegA", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	legB := values.NewRecordType("LegB", false, []values.Field{
		{Name: "QTY", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
	scan := foldStep1CensusScan(t, legA)

	for _, tc := range []struct {
		name string
		rv   values.Value
		want foldStep1LegShape
	}{
		{
			name: "positional merge",
			rv:   positionalMergeRCOverLegs(t, legA, legB),
			want: foldStep1LegShapePositionalMerge,
		},
		{
			name: "bare QOV identity pass-through",
			rv: foldStep1CensusQOV(
				t, values.NamedCorrelationIdentifier("A"), legA),
			want: foldStep1LegShapeBareQOV,
		},
		{
			name: "a NAMED 2-slot RC of bare typed leg QOVs is NOT a merge",
			rv: values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "A", Value: foldStep1CensusQOV(t, values.NamedCorrelationIdentifier("A"), legA)},
				values.RecordConstructorField{Name: "B", Value: foldStep1CensusQOV(t, values.NamedCorrelationIdentifier("B"), legB)},
			),
			want: foldStep1LegShapeRCNotMerge,
		},
		{
			name: "exact scalar result value",
			rv:   &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
			want: foldStep1LegShapeOther,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fm := foldStep1CensusFlatMap(t, scan, tc.rv)
			got, witness := classifyDeclinedLeg(fm)
			if got != tc.want {
				t.Fatalf("classifyDeclinedLeg = %v, want %v (witness %q) — RFC-200's gate (c) is "+
					"an EXACT equality on the positional-merge bucket, so a shape landing in "+
					"the wrong bucket makes the equality measure a different population than "+
					"the one the nested window admits", got, tc.want, witness)
			}
			if witness == "" {
				t.Fatal("every classified decline must carry a witness naming the plan type — a " +
					"bucket that moves with no witness is a number with nothing to attribute it to")
			}
		})
	}
}

// Exact QOV admission has closed the historical typed/untyped split: unresolved
// roots are rejected at construction, and every admitted QOV reports
// `typed=true`. The witness must still spell WHICH exact type it carries,
// because a record row and a scalar both satisfy that boolean but only the row
// has an arity the ordinal layout could position.
func TestFoldStep1Census_BareQOVWitnessReportsAdmittedExactTypes(t *testing.T) {
	t.Parallel()

	legA := values.NewRecordType("LegA", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	scan := foldStep1CensusScan(t, legA)
	corr := values.NamedCorrelationIdentifier("A")

	if qov, err := values.NewQuantifiedObjectValue(corr, values.UnknownType); err == nil || qov != nil {
		t.Fatalf("unresolved QOV admission = (%v, %v), want (nil, error)", qov, err)
	}

	for _, tc := range []struct {
		name     string
		rv       values.Value
		wantType string
	}{
		{
			name:     "QOV carrying a real row type",
			rv:       foldStep1CensusQOV(t, corr, legA),
			wantType: "rvtype=RecordType(1)",
		},
		{
			name:     "QOV carrying an exact scalar type",
			rv:       foldStep1CensusQOV(t, corr, values.NotNullLong),
			wantType: "rvtype=LONG",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fm := foldStep1CensusFlatMap(t, scan, tc.rv)

			shape, witness := classifyDeclinedLeg(fm)
			if shape != foldStep1LegShapeBareQOV {
				t.Fatalf("shape = %v, want foldStep1LegShapeBareQOV (witness %q)", shape, witness)
			}
			if !strings.Contains(witness, "typed=true") || !strings.Contains(witness, tc.wantType) {
				t.Fatalf("witness %q must report typed=true and %q", witness, tc.wantType)
			}
		})
	}
}

// The census's own assertion arms, driven from counter states rather than from
// the planner.
//
// A gate is a claim about which counter states FAIL, and a claim reachable only
// by driving the whole planner into a defective state is a claim nothing pins —
// the same split every census on this path makes.
func TestFoldStep1Census_AssertionArmsGoRed(t *testing.T) {
	t.Parallel()

	base := func() foldStep1SeedCounters {
		var c foldStep1SeedCounters
		c.Denominator = 10
		c.Class[foldStep1Accept] = 4
		c.Class[foldStep1DeclineCorrelatedStep1] = 2
		c.Class[foldStep1DeclineNoExistRef] = 1
		c.Class[foldStep1DeclineReconstructNil] = 3
		c.ReconstructNilLegShape[foldStep1LegShapeBareQOV] = 2
		c.ReconstructNilLegShape[foldStep1LegShapePositionalMerge] = 1
		return c
	}

	var sb strings.Builder
	if assertFoldStep1SeedCounters(&sb, base(), nil, nil) {
		t.Fatalf("a consistent census must pass its structural checks: %s", sb.String())
	}

	// THE INDEPENDENT DENOMINATOR. This is the check the whole design rests on:
	// summing the classes would make the partition true by construction, so an
	// arm added without a counter would be invisible. Simulate exactly that — one
	// firing counted at the call site, classified nowhere.
	c := base()
	c.Denominator = 11
	sb.Reset()
	if !assertFoldStep1SeedCounters(&sb, c, nil, nil) {
		t.Fatal("a class arm with no counter left the partition GREEN — the independent " +
			"denominator is the census's one structural defence and it is not firing")
	}
	if !strings.Contains(sb.String(), "INDEPENDENT") {
		t.Fatalf("the denominator failure must name what it detects, got %q", sb.String())
	}

	// The refused-leg sub-partition.
	c = base()
	c.ReconstructNilLegShape[foldStep1LegShapeBareQOV] = 1
	sb.Reset()
	if !assertFoldStep1SeedCounters(&sb, c, nil, nil) {
		t.Fatal("a refused-leg shape that returns before recording left the sub-partition GREEN")
	}

	// The both-legs-unsafe premise. The per-firing sub-partition records the
	// FIRST refused leg, which is an honest summary only while at most one leg
	// per firing is refused — a premise that came from a removed probe and that
	// nothing was checking.
	c = base()
	c.ReconstructNilBothLegsUnsafe = 1
	sb.Reset()
	if !assertFoldStep1SeedCounters(&sb, c, nil, nil) {
		t.Fatal("a firing with BOTH legs refused left the sub-partition's premise unchecked")
	}

	// An EXACT equality is exact. A gate stated at 60 and measured at 59 must go
	// red, not be absorbed.
	sixty, fiftyNine := 60, 59
	c = base()
	c.ReconstructNilLegShape[foldStep1LegShapePositionalMerge] = fiftyNine
	c.ReconstructNilLegShape[foldStep1LegShapeBareQOV] = 0
	c.Class[foldStep1DeclineReconstructNil] = fiftyNine
	c.Denominator = 4 + 2 + 1 + fiftyNine
	sb.Reset()
	if !assertFoldStep1SeedCounters(&sb, c, nil, &FoldStep1SeedGates{ReconstructNilMerge: &sixty}) {
		t.Fatal("a stated equality of 60 accepted a measured 59 — these are PREDICTIONS and a " +
			"deviation is itself the finding, so an off-by-one must be loud")
	}
	if !strings.Contains(sb.String(), "want EXACTLY 60") {
		t.Fatalf("the equality failure must quote both numbers, got %q", sb.String())
	}
}

// The ORIENTATION GATE census's assertion arms, driven from counter states.
//
// Its sibling assertFoldStep1SeedCounters has had this treatment since it was
// written; this census — the newest on the path, and the one carrying RFC-200
// step 3d”s live/latent discriminator — had NONE. That asymmetry is the whole
// reason this exists: a gate is a claim about which counter states FAIL, and a
// claim reachable only by driving the planner into a defective state is a claim
// nothing pins. Every arm below can now be shown red on the state that violates
// it.
func TestOrientationGateCensus_AssertionArmsGoRed(t *testing.T) {
	t.Parallel()

	// The corpus's MEASURED state: calls 438 (not-a-seed 96, tiled-by-2 342,
	// tiled-by-other 0); of the tiled-by-2, unverifiable 84, matched 197,
	// declined 61; 72 firings where the MAP count differs from the TILE count, of
	// which 0 declined.
	base := func() orientationGateCounters {
		return orientationGateCounters{
			Calls: 438, NotASeed: 96, TiledByTwo: 342, TiledByOther: 0,
			Unverifiable: 84, Matched: 197, Declined: 61,
			MapCountDiffers: 72, DeclinedNewlyChecked: 0,
		}
	}
	floors := &OrientationGateFloors{
		Calls: 40, MapCountDiffers: 7, UnverifiableCeiling: 200,
		MatchedFloor: 40, DeclinedCeiling: 200,
	}

	var clean strings.Builder
	if assertOrientationGateCounters(&clean, base(), floors) {
		t.Fatalf("the gate FAILS the corpus's own measured state:\n%s\n"+
			"  Every case below expects a red, so a baseline that is already red proves "+
			"nothing about any of them.", clean.String())
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*orientationGateCounters)
		wantMsg string
	}{
		{
			// A firing counted into Calls and into no shape bucket — a shape arm
			// that returns before recording.
			name:    "a firing counted into no tile-count bucket",
			mutate:  func(c *orientationGateCounters) { c.NotASeed-- },
			wantMsg: "tiledByOther(0) = 437, but calls = 438",
		},
		{
			// A two-tile firing that reached the comparison and took none of the
			// three dispositions.
			name:    "a checkable firing with no disposition",
			mutate:  func(c *orientationGateCounters) { c.Matched-- },
			wantMsg: "declined(61) = 341, but tiledByTwo = 342",
		},
		{
			// The CROSS cannot exceed either of the two cuts it crosses.
			name:    "declinedNewlyChecked exceeds declined",
			mutate:  func(c *orientationGateCounters) { c.DeclinedNewlyChecked = 62 },
			wantMsg: "exceeds declined(61)",
		},
		{
			name:    "declinedNewlyChecked exceeds mapCountDiffers",
			mutate:  func(c *orientationGateCounters) { c.Declined = 80; c.Matched = 178; c.DeclinedNewlyChecked = 73 },
			wantMsg: "mapCountDiffers(72)",
		},
		{
			// The gate going dark entirely.
			name:    "the gate is not reached at all",
			mutate:  func(c *orientationGateCounters) { *c = orientationGateCounters{} },
			wantMsg: "0 calls, want >= 40",
		},
		{
			// THE LIVE/LATENT DISCRIMINATOR. This is the one number that separates
			// "3d' is live and every newly-checked firing agrees" from "3d' is
			// latent"; a zero here makes the fail-open's closure an untested claim
			// that prints exactly the same clean result as today.
			name: "the population 3d' moves reaches zero",
			mutate: func(c *orientationGateCounters) {
				c.MapCountDiffers = 0
				c.DeclinedNewlyChecked = 0
			},
			wantMsg: "only 0 firing(s) have a MAP count",
		},
		{
			// THE CEILING, whose dangerous direction is GROWTH. 84 measured, 200
			// allowed; 250 is the second fail-open absorbing the checkable
			// population.
			name: "the unverifiable fail-open grows past its ceiling",
			mutate: func(c *orientationGateCounters) {
				c.Unverifiable = 250
				c.Matched = 31 // keep the disposition partition exact
			},
			wantMsg: "250 UNVERIFIABLE firing(s), want <= 200",
		},
		{
			// THE MATCHED FLOOR, whose dangerous direction is COLLAPSE. Added
			// with RFC-226 because until then the two DECIDING arms had no bound
			// at all: a gate that proved nothing satisfied every other check
			// here, since the partition still adds up and a ceiling cannot
			// notice a drop.
			name: "the gate stops proving anything",
			mutate: func(c *orientationGateCounters) {
				c.Matched = 5
				c.Unverifiable = 276 // keep the disposition partition exact
			},
			wantMsg: "only 5 MATCHED firing(s), want >= 40",
		},
		{
			// THE DECLINED CEILING, whose dangerous direction is GROWTH — and
			// growth here is not "a slower plan", it is NO plan: this gate admits
			// the materialized NLJ, so refusing both orientations loses the query.
			name: "refusals grow into plan loss",
			mutate: func(c *orientationGateCounters) {
				c.Declined = 250
				c.Matched = 8 // keep the disposition partition exact
			},
			wantMsg: "250 DECLINED firing(s), want <= 200",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := base()
			tc.mutate(&c)
			var out strings.Builder
			if !assertOrientationGateCounters(&out, c, floors) {
				t.Fatalf("the gate stayed GREEN on %q — this arm asserts nothing", tc.name)
			}
			if !strings.Contains(out.String(), tc.wantMsg) {
				t.Errorf("the failure for %q does not name what it detected (want %q):\n%s",
					tc.name, tc.wantMsg, out.String())
			}
		})
	}

	// THE CEILING'S BOUNDARY IS <=, not <. Exactly at the ceiling passes; one
	// over fails. An off-by-one here either reds the measured corpus the day it
	// reaches the bound or lets the fail-open through by one, and neither is
	// visible without pinning both sides of the edge.
	at := base()
	at.Unverifiable = 200
	at.Matched = 81 // keep tiledByTwo exact: 200 + 81 + 61 = 342
	var atOut strings.Builder
	if assertOrientationGateCounters(&atOut, at, floors) {
		t.Fatalf("unverifiable EXACTLY at the ceiling (200) reds:\n%s", atOut.String())
	}
	over := at
	over.Unverifiable = 201
	over.Matched = 80
	var overOut strings.Builder
	if !assertOrientationGateCounters(&overOut, over, floors) {
		t.Fatal("unverifiable ONE OVER the ceiling (201) stays green — the boundary is " +
			"< where it must be <=, so the ceiling admits one more than it states")
	}

	// And with no floors (a NARROWED run) the partitions still hold, because they
	// are true over any population — the same split every census on this path
	// makes.
	var narrowed strings.Builder
	if assertOrientationGateCounters(&narrowed, base(), nil) {
		t.Fatalf("the measured state reds with floors dropped:\n%s", narrowed.String())
	}
	broken := base()
	broken.NotASeed--
	var brokenNarrow strings.Builder
	if !assertOrientationGateCounters(&brokenNarrow, broken, nil) {
		t.Fatal("a broken partition passed with floors dropped — the partitions must " +
			"hold over ANY population, including a -test.run-narrowed one")
	}
}

// The correlatedStep1-with-windows reachability counter.
//
// It answers the one conjunction on the EXISTS arm that RFC-200 could not
// establish by reading, and that any conversion of the reconstruct-nil residue
// contacts: `:4124`'s mint and its FlatMap construction run on BOTH the
// correlated and the materialized arm, so a positionable leg result value
// produces a baked ordinal where a name-keyed row context raises
// values.BakedNameContextError.
//
// MEASURED: 108 of 108 over the whole real-FDB corpus, on three consecutive
// runs. The conjunction is UNIVERSAL on the correlated arm — every firing
// arrives at the layout read with a merged layout already derived — so the
// conversion meets the wall on day one, on 100% of that population.
//
// The NUMERATOR is deliberately not asserted at a value even so. At 100% the
// only movement available is a DROP, and a drop is a FINDING to read (the corpus
// moved, or the layout stopped being derived on that arm — two different causes
// needing to be told apart) rather than a regression to block. What IS asserted
// is that the pair stays a coherent ratio and that the denominator cannot go
// dark silently, because a zero denominator makes the numerator measure an
// absence of TRAFFIC while reading as an absence of the SHAPE.
func TestFoldStep1Census_CorrelatedStep1WindowsReachability(t *testing.T) {
	t.Parallel()

	t.Run("the numerator exceeding the denominator is RED", func(t *testing.T) {
		t.Parallel()
		var c foldStep1SeedCounters
		c.CorrelatedStep1Firings = 3
		c.CorrelatedStep1WithWindows = 4
		var b strings.Builder
		if !assertFoldStep1SeedCounters(&b, c, nil, nil) {
			t.Fatal("a with-windows count larger than the firing count must fail. They are " +
				"recorded by ONE call at ONE site, so a gap means the two stopped being the " +
				"numerator and denominator of one ratio and the printed fraction is fiction.")
		}
	})

	t.Run("the MEASURED shape PASSES", func(t *testing.T) {
		t.Parallel()
		// The real-FDB corpus, verbatim: `correlatedStep1 firings WITH a merged
		// layout 108 of 108`. This fixture read 108/0 on first writing — a value
		// drafted from the expectation that the correlated wall leaves nothing
		// positioned, never run. A fixture named "the measured shape" holding a
		// number nobody measured is worse than no fixture: it makes the census's
		// own prediction look like its result.
		var c foldStep1SeedCounters
		c.CorrelatedStep1Firings = 108
		c.CorrelatedStep1WithWindows = 108
		var b strings.Builder
		if assertFoldStep1SeedCounters(&b, c, nil, nil) {
			t.Fatalf("the measured shape (108 of 108 — the conjunction is UNIVERSAL on the "+
				"correlated arm) must pass, or the red above is measuring the gate rather "+
				"than the population:\n%s", b.String())
		}
	})

	t.Run("the DENOMINATOR going dark is RED", func(t *testing.T) {
		t.Parallel()
		n := func(v int) *int { return &v }
		var c foldStep1SeedCounters
		var b strings.Builder
		if !assertFoldStep1SeedCounters(&b, c, nil, &FoldStep1SeedGates{CorrelatedStep1FiringsFloor: n(50)}) {
			t.Fatal("zero correlatedStep1 firings reaching the layout read must fail. With the " +
				"denominator at zero the reachability measurement says nothing, and a " +
				"conversion of the residue would be planned against a number that only " +
				"looks like evidence.")
		}
		if !strings.Contains(b.String(), "absence of TRAFFIC") {
			t.Fatalf("the failure message must say what the zero actually means: %s", b.String())
		}
	})

	t.Run("the recorder counts both halves", func(t *testing.T) {
		// NOT parallel: it writes the process-global census.
		ResetFoldStep1SeedCensus()
		defer ResetFoldStep1SeedCensus()
		recordCorrelatedStep1Windows(false)
		recordCorrelatedStep1Windows(true)
		recordCorrelatedStep1Windows(false)
		c, _ := FoldStep1SeedCensus()
		if c.CorrelatedStep1Firings != 3 || c.CorrelatedStep1WithWindows != 1 {
			t.Fatalf("firings=%d withWindows=%d, want 3 and 1. The denominator must count "+
				"EVERY correlated firing at the layout read and the numerator only those the "+
				"layout answered; collapsing either makes the ratio unreadable",
				c.CorrelatedStep1Firings, c.CorrelatedStep1WithWindows)
		}
		if !strings.Contains(FormatFoldStep1SeedCensus(), "1 of 3") {
			t.Fatalf("the census must PRINT the ratio, not just hold it — a counter no "+
				"harness renders is a measurement nobody reads:\n%s", FormatFoldStep1SeedCensus())
		}
	})
}
