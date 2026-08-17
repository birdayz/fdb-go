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

func provenanceMergeRowType() values.Type {
	return values.NewRecordType("ProjectionProvenanceMergeRow", false, []values.Field{
		{Name: "U.NAME", FieldType: values.NullableString, Ordinal: 0},
		{Name: "O.TOTAL", FieldType: values.NullableLong, Ordinal: 1},
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

// TestProjectionMergeRule_ComposesAliasProvenance pins the one site that
// COMPOSES two provenance vectors rather than copying one.
//
// Both halves of the composition are pinned, because the rule can be wrong in
// two independent directions:
//
//   - a slot the outer had NOT aliased has its effective name written into the
//     alias vector BY THIS RULE. That name is the Value's own Field, which over
//     a join is leg-qualified ("U.NAME"), so reporting it as a user alias would
//     make the merge alone leak the qualifier into `SELECT u.name`'s label.
//     Measured at 67 firings across the sqldriver suite.
//   - a slot the outer HAD aliased keeps the outer's own marker, so a machinery
//     key that reached the outer stays a machinery key. Measured at 6 firings.
func TestProjectionMergeRule_ComposesAliasProvenance(t *testing.T) {
	t.Parallel()
	scanExpr := mustProvenanceConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, provenanceMergeRowType()))
	scan := expressions.ForEachQuantifier(expressions.InitialOf(scanExpr))
	// The inner supplies two slots the outer reads by baked ordinal.
	inner := mustProvenanceConstruct(expressions.NewLogicalProjectionExpressionWithAliasProvenance(
		provenanceFields(scan, 0, 1),
		[]string{"U.NAME", "O.TOTAL"},
		[]bool{true, true},
		scan,
	))
	inner = mustProvenanceConstruct(inner.WithAliasSources([]values.ProjectionAliasSource{
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("U")),
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("O")),
	}))
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(inner))
	outerFields := provenanceFields(innerQ, 0, 1)

	// Outer slot 0: NO alias — the rule mints its effective name here, so the
	// merged slot is machinery-named regardless of the outer's marker.
	// Outer slot 1: a user alias, marker false — must stay a user alias.
	outer := mustProvenanceConstruct(expressions.NewLogicalProjectionExpressionWithAliasProvenance(
		outerFields,
		[]string{"", "MY.TOTAL"},
		[]bool{false, false},
		innerQ,
	))

	rule := NewProjectionMergeRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), outer)
	if len(bindings) == 0 {
		t.Fatal("rule should match a projection over a projection")
	}
	call := NewExpressionRuleCall(expressions.InitialOf(outer), bindings[0], EmptyPlanContext())
	rule.OnMatch(call)

	var flat *expressions.LogicalProjectionExpression
	for _, y := range call.Yielded() {
		if p, ok := y.(*expressions.LogicalProjectionExpression); ok {
			flat = p
		}
	}
	if flat == nil {
		t.Fatal("merge rule yielded no flattened projection")
	}
	got := flat.GetAliasMinted()
	if len(got) != 2 {
		t.Fatalf("merged provenance has %d slots, want 2 (got %v)", len(got), got)
	}
	if !got[0] {
		t.Error("an outer slot with NO alias is named by this rule, so the merged slot is machinery-named; " +
			"reporting it as a user alias leaks the leg qualifier into the label")
	}
	if got[1] {
		t.Error("an outer slot the USER aliased must keep its user provenance across the merge")
	}
	mergedSources := flat.GetAliasSources()
	if len(mergedSources) != 2 || !mergedSources[0].Present ||
		mergedSources[0].Source != values.NamedCorrelationIdentifier("U") || mergedSources[1].Present {
		t.Errorf("rule-minted outer slot did not inherit exact inner source / user slot gained one: %+v", mergedSources)
	}

	// The other direction of the carry: an outer slot that was ALREADY a
	// machinery key must stay one even though it carries an explicit alias.
	outerMinted := mustProvenanceConstruct(expressions.NewLogicalProjectionExpressionWithAliasProvenance(
		outerFields,
		[]string{"U.NAME", "O.TOTAL"},
		[]bool{true, true},
		innerQ,
	))
	outerMinted = mustProvenanceConstruct(outerMinted.WithAliasSources([]values.ProjectionAliasSource{
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("OUTER_U")),
		values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("OUTER_O")),
	}))
	bindings = rule.Matcher().BindMatches(matching.NewBindings(), outerMinted)
	if len(bindings) == 0 {
		t.Fatal("rule should match the minted-outer projection")
	}
	call = NewExpressionRuleCall(expressions.InitialOf(outerMinted), bindings[0], EmptyPlanContext())
	rule.OnMatch(call)
	flat = nil
	for _, y := range call.Yielded() {
		if p, ok := y.(*expressions.LogicalProjectionExpression); ok {
			flat = p
		}
	}
	if flat == nil {
		t.Fatal("merge rule yielded no flattened projection for the minted outer")
	}
	if got := flat.GetAliasMinted(); len(got) != 2 || !got[0] || !got[1] {
		t.Errorf("a machinery key must survive the merge: got %v, want [true true]", got)
	}
	gotSources := flat.GetAliasSources()
	if len(gotSources) != 2 || gotSources[0].Source != values.NamedCorrelationIdentifier("OUTER_U") ||
		gotSources[1].Source != values.NamedCorrelationIdentifier("OUTER_O") {
		t.Errorf("existing outer machinery sources did not survive merge: %+v", gotSources)
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
