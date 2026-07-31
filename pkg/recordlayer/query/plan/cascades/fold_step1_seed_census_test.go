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
func positionalMergeRCOverLegs(legs ...*values.RecordType) *values.RecordConstructorValue {
	fields := make([]values.RecordConstructorField, len(legs))
	for i, rt := range legs {
		corr := values.NamedCorrelationIdentifier(strings.ToUpper(string(rune('A' + i))))
		fields[i] = values.RecordConstructorField{
			Name:  values.OrdinalFieldName(i),
			Value: values.NewQuantifiedObjectValueOfType(corr, rt),
		}
	}
	return values.NewRawRecordConstructorValue(fields...)
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

	legA := &values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0}}}
	legB := &values.RecordType{Fields: []values.Field{{Name: "QTY", Ordinal: 0}, {Name: "V", Ordinal: 1}}}
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, legA, false)

	for _, tc := range []struct {
		name string
		rv   values.Value
		want foldStep1LegShape
	}{
		{
			name: "positional merge",
			rv:   positionalMergeRCOverLegs(legA, legB),
			want: foldStep1LegShapePositionalMerge,
		},
		{
			name: "bare QOV identity pass-through",
			rv:   values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A")),
			want: foldStep1LegShapeBareQOV,
		},
		{
			name: "a NAMED 2-slot RC of bare typed leg QOVs is NOT a merge",
			rv: values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "A", Value: values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), legA)},
				values.RecordConstructorField{Name: "B", Value: values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), legB)},
			),
			want: foldStep1LegShapeRCNotMerge,
		},
		{
			name: "no result value at all",
			rv:   nil,
			want: foldStep1LegShapeOther,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fm := plans.NewRecordQueryFlatMapPlan(scan, scan,
				values.NamedCorrelationIdentifier("O"), values.NamedCorrelationIdentifier("I"), tc.rv, false)
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
