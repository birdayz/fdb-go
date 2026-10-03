package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustProvenanceConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct projection-provenance fixture: " + err.Error())
	}
	return value
}

func provenanceRowType() values.Type {
	return values.NewRecordType("ProjectionProvenanceRow", false, []values.Field{
		{Name: "A.K", FieldType: values.NullableLong, Ordinal: 0},
	})
}

func provenanceFields(q expressions.Quantifier, ordinals ...int) []values.Value {
	root := mustProvenanceConstruct(q.RequireFlowedObjectValue())
	fields := make([]values.Value, len(ordinals))
	for i, ordinal := range ordinals {
		fields[i] = mustProvenanceConstruct(values.ResolveFieldOrdinals(
			root, []int{ordinal}))
	}
	return fields
}

// The per-slot alias provenance (who named an output column: the machinery's
// duplicate-disambiguation mint, or the user's `AS`) has to survive every
// rewrite between the translator and the ResultSet metadata site, because
// nothing downstream can re-derive it — a machinery key and a user alias are
// spelled alike, which is the whole reason it is carried.
//
// These tests drive one rewrite each, DIRECTLY. That is deliberate: an
// end-to-end query cannot isolate a single carry, because the marker is
// excluded from memo identity, so two lowerings of one projection that differ
// only in the marker intern as ONE member and whichever was memoized first
// supplies it. Measured: dropping the carry in ImplementProjectionFinalRule
// alone leaves every FDB label test green, because ImplementProjectionRule's
// equal member wins the group. End-to-end coverage therefore cannot see a
// single-site drop, and only a per-site test can.

// provenanceTestProjection builds a one-slot projection carrying the given
// provenance over a PHYSICAL inner: both lowering rules require the inner
// reference to already hold a physical plan member, so a logical scan yields
// nothing and the test would pass vacuously.
func provenanceTestProjection(aliasMinted []bool) *expressions.LogicalProjectionExpression {
	innerPlan := mustProvenanceConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, provenanceRowType(), false))
	inner := expressions.ForEachQuantifier(expressions.InitialOf(innerPlan))
	projection := mustProvenanceConstruct(expressions.NewLogicalProjectionExpressionWithAliasProvenance(
		provenanceFields(inner, 0),
		[]string{"A.K"},
		aliasMinted,
		inner,
	))
	if len(aliasMinted) > 0 && aliasMinted[0] {
		projection = mustProvenanceConstruct(projection.WithAliasSources(
			[]values.ProjectionAliasSource{
				values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
			}))
	}
	return projection
}

func assertCarriedMinted(t *testing.T, got []bool, where string) {
	t.Helper()
	if len(got) != 1 || !got[0] {
		t.Errorf("%s dropped the alias provenance: got %v, want [true]", where, got)
	}
}

func assertCarriedSource(t *testing.T, got []values.ProjectionAliasSource, where, want string) {
	t.Helper()
	if len(got) != 1 || !got[0].Present || got[0].Source != values.NamedCorrelationIdentifier(want) {
		t.Errorf("%s dropped the structured alias source: got %+v, want %s", where, got, want)
	}
}

// TestImplementProjectionFinalRule_CarriesAliasProvenance pins the PLANNING-phase
// lowering. Measured as the most-travelled of the two lowerings (1707 firings
// with a minted slot across the sqldriver suite, against 467 for the
// EXPLORE-phase rule), and the one an end-to-end test cannot pin on its own.
func TestImplementProjectionFinalRule_CarriesAliasProvenance(t *testing.T) {
	t.Parallel()
	proj := provenanceTestProjection([]bool{true})
	rule := NewImplementProjectionFinalRule()

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	if len(bindings) == 0 {
		t.Fatal("rule should match LogicalProjectionExpression")
	}
	call := &ImplementationRuleCall{Bindings: bindings[0], Context: EmptyPlanContext()}
	rule.OnMatch(call)

	found := false
	for _, y := range call.yielded {
		p, ok := y.(*plans.RecordQueryProjectionPlan)
		if !ok {
			continue
		}
		found = true
		assertCarriedMinted(t, p.GetAliasMinted(), "ImplementProjectionFinalRule")
		assertCarriedSource(t, p.GetAliasSources(), "ImplementProjectionFinalRule", "A")
	}
	if !found {
		t.Fatal("rule yielded no RecordQueryProjectionPlan")
	}
}

// TestImplementProjectionRule_CarriesAliasProvenance pins the EXPLORE-phase
// lowering's normal (winner-wrapping) arm.
func TestImplementProjectionRule_CarriesAliasProvenance(t *testing.T) {
	t.Parallel()
	proj := provenanceTestProjection([]bool{true})
	rule := NewImplementProjectionRule()

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	if len(bindings) == 0 {
		t.Fatal("rule should match LogicalProjectionExpression")
	}
	ref := expressions.InitialOf(proj)
	call := NewExpressionRuleCall(ref, bindings[0], EmptyPlanContext())
	rule.OnMatch(call)

	found := false
	for _, y := range call.Yielded() {
		p, ok := y.(*plans.RecordQueryProjectionPlan)
		if !ok {
			continue
		}
		found = true
		assertCarriedMinted(t, p.GetAliasMinted(), "ImplementProjectionRule")
		assertCarriedSource(t, p.GetAliasSources(), "ImplementProjectionRule", "A")
	}
	if !found {
		t.Fatal("rule yielded no RecordQueryProjectionPlan")
	}
}

func TestSameProjectionMetadataComparesStructuredAliasSource(t *testing.T) {
	t.Parallel()
	logical := provenanceTestProjection([]bool{true})
	inner := logical.GetInner()
	physical := mustProvenanceConstruct(plans.NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
		logical.GetProjectedValues(), logical.GetAliases(), logical.GetAliasMinted(), logical.GetOutputNames(), inner))
	physical = mustProvenanceConstruct(physical.WithAliasSources([]values.ProjectionAliasSource{
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
	}))
	if !sameProjectionMetadata(logical, physical, 1) {
		t.Fatal("equal frozen structured alias sources did not license projection reuse")
	}

	foreign := mustProvenanceConstruct(physical.WithAliasSources([]values.ProjectionAliasSource{
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("Z")),
	}))
	if sameProjectionMetadata(logical, foreign, 1) {
		t.Fatal("projection reuse discarded a different frozen structured alias source")
	}
}

// TestPlanExtraction_CarriesAliasProvenance pins the extraction rebuild. Measured
// at ZERO firings with a minted slot across the sqldriver suite today, which is
// exactly why it needs a unit pin rather than reliance on end-to-end coverage:
// the site is live (every rebuilt projection goes through it) and only the
// current query mix keeps a minted vector away from it. The same rebuild already
// shipped one defect by dropping the alias vector itself.
func TestPlanExtraction_CarriesAliasProvenance(t *testing.T) {
	t.Parallel()
	proj := provenanceTestProjection([]bool{true})
	freshScan := mustProvenanceConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"U"}, provenanceRowType()))
	fresh := expressions.ForEachQuantifier(expressions.InitialOf(freshScan))

	rebuilt, err := rebuildWithFreshChildren(proj, []expressions.Quantifier{fresh})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rp, ok := rebuilt.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("rebuild returned %T, want *LogicalProjectionExpression", rebuilt)
	}
	assertCarriedMinted(t, rp.GetAliasMinted(), "plan extraction rebuild")
	assertCarriedSource(t, rp.GetAliasSources(), "plan extraction rebuild", "A")
	if got := rp.GetAliases(); len(got) != 1 || got[0] != "A.K" {
		t.Errorf("plan extraction rebuild dropped the aliases: got %v, want [A.K]", got)
	}
}
