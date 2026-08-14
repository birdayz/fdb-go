package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustImplementUniqueConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct implement-unique fixture: " + err.Error())
	}
	return value
}

func implementUniqueRowType() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{{
		Name: "ID", FieldType: values.NotNullLong,
	}})
}

func implementUniquePrimaryKey() []values.Value {
	root := mustImplementUniqueConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("implement_unique_primary_key"),
		implementUniqueRowType()))
	return []values.Value{
		mustImplementUniqueConstruct(values.ResolveFieldOrdinals(root, []int{0})),
	}
}

func implementUniqueScan(recordType string, withPrimaryKey bool) *plans.RecordQueryScanPlan {
	scan := mustImplementUniqueConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, implementUniqueRowType(), false))
	if !withPrimaryKey {
		return scan
	}
	return scan.WithPrimaryKey(implementUniquePrimaryKey()).
		WithKeyComponentTypes([]values.Type{values.NotNullLong})
}

// ---------------------------------------------------------------------------
// ImplementUniqueRule
// ---------------------------------------------------------------------------

func TestImplementUniqueRule_MatchesLogicalUniqueExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementUniqueRule()
	scanRef := expressions.InitialOf(mustImplementUniqueConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, implementUniqueRowType())))
	unique := mustImplementUniqueConstruct(expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(scanRef)))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	if len(bindings) == 0 {
		t.Fatal("ImplementUniqueRule should match LogicalUniqueExpression")
	}
}

func TestImplementUniqueRule_SkipsNonMatching(t *testing.T) {
	t.Parallel()
	rule := NewImplementUniqueRule()
	// A filter expression should not match.
	scanRef := expressions.InitialOf(mustImplementUniqueConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, implementUniqueRowType())))
	filter := mustImplementUniqueConstruct(expressions.NewLogicalFilterExpression(nil, expressions.ForEachQuantifier(scanRef)))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("ImplementUniqueRule should NOT match LogicalFilterExpression")
	}
}

func TestImplementUniqueRule_AbsorbsWhenInnerIsDistinct(t *testing.T) {
	t.Parallel()
	// Build: Unique(innerRef) where innerRef holds a bare scan plan
	// with distinct=true and a proven primary key.
	scan := implementUniqueScan("T", true)
	scanWrapper := scan

	// Create inner reference with physical wrapper as final member.
	innerRef := expressions.InitialOf(scanWrapper)

	// Compute plan properties on the inner reference.
	pm := NewPlanPropertiesMap()
	pm.Add(scanWrapper)
	innerRef.SetPlanProperties(pm)

	// Build the LogicalUniqueExpression.
	unique := mustImplementUniqueConstruct(expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(innerRef)))
	outerRef := expressions.InitialOf(unique)

	// Fire the rule.
	results := mustFireImplementationRule(t, NewImplementUniqueRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("ImplementUniqueRule should yield expressions when inner is distinct")
	}

	// The yielded expression should be the inner scan wrapper itself
	// (Unique is absorbed, inner plans are promoted).
	found := false
	for _, r := range results {
		if r == scanWrapper {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("yielded expressions should include the inner scan wrapper (Unique absorbed)")
	}
}

func TestImplementUniqueRule_OrdinaryRequiresDistinctAndPrimaryKeyOnSameMember(
	t *testing.T,
) {
	t.Parallel()

	distinctWithPK := implementUniqueScan("DISTINCT_WITH_PK", true)
	distinctWithoutPK := implementUniqueScan("DISTINCT_WITHOUT_PK", false)
	notDistinctWithPK := mustImplementUniqueConstruct(plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
		[]string{"ID"}, distinctWithPK))

	innerRef := expressions.InitialOf(distinctWithPK)
	innerRef.Insert(distinctWithoutPK)
	innerRef.Insert(notDistinctWithPK)
	pm := NewPlanPropertiesMap()
	pm.Add(distinctWithPK)
	pm.Add(distinctWithoutPK)
	pm.Add(notDistinctWithPK)
	innerRef.SetPlanProperties(pm)

	unique := mustImplementUniqueConstruct(expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(innerRef),
	))
	results := mustFireImplementationRule(t,
		NewImplementUniqueRule(),
		expressions.InitialOf(unique),
	)
	if len(results) != 1 || results[0] != distinctWithPK {
		t.Fatalf(
			"ordinary Unique yielded %v, want only exact distinct+PK member",
			results,
		)
	}
}

func TestImplementUniqueRule_RequiredWrapsEveryPKMemberAndFreezesExactInput(
	t *testing.T,
) {
	t.Parallel()

	distinctWithPK := implementUniqueScan("DISTINCT_WITH_PK", true)
	distinctWithoutPK := implementUniqueScan("DISTINCT_WITHOUT_PK", false)
	notDistinctWithPK := mustImplementUniqueConstruct(plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
		[]string{"ID"}, distinctWithPK))

	innerRef := expressions.InitialOf(distinctWithPK)
	innerRef.Insert(distinctWithoutPK)
	innerRef.Insert(notDistinctWithPK)
	pm := NewPlanPropertiesMap()
	pm.Add(distinctWithPK)
	pm.Add(distinctWithoutPK)
	pm.Add(notDistinctWithPK)
	innerRef.SetPlanProperties(pm)

	unique := mustImplementUniqueConstruct(expressions.NewRequiredLogicalUniqueExpression(
		expressions.ForEachQuantifier(innerRef),
	))
	results := mustFireImplementationRule(t,
		NewImplementUniqueRule(),
		expressions.InitialOf(unique),
	)
	if len(results) != 2 {
		t.Fatalf("required Unique yielded %d plans, want 2", len(results))
	}

	seen := map[plans.RecordQueryPlan]bool{}
	for _, result := range results {
		distinct, ok := result.(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan)
		if !ok {
			t.Fatalf(
				"required Unique yielded absorbed/raw %T, want PK-distinct wrapper",
				result,
			)
		}
		innerQ := distinct.GetInnerQuantifier()
		pinnedRef := innerQ.GetRangesOver()
		if pinnedRef == nil || pinnedRef == innerRef {
			t.Fatal("required PK-distinct did not detach and freeze its exact input")
		}
		pinnedMembers := pinnedRef.FinalMembers()
		if len(pinnedMembers) != 1 {
			t.Fatalf(
				"frozen input has %d final members, want exactly 1",
				len(pinnedMembers),
			)
		}
		if pinnedMembers[0] != distinct.GetInner() {
			t.Fatal("PK-distinct resolves a different plan than its frozen member")
		}
		seen[distinct.GetInner()] = true
	}

	if !seen[distinctWithPK] || !seen[notDistinctWithPK] {
		t.Fatalf(
			"required Unique wrapped inputs %v, want exact two PK-proven members",
			seen,
		)
	}
	if seen[distinctWithoutPK] {
		t.Fatal("required Unique wrapped a member without a primary-key proof")
	}
}

func TestImplementUniqueRule_NoYieldWhenInnerNotDistinct(t *testing.T) {
	t.Parallel()
	// Streaming agg has distinct=false. Since RFC-184 W2 the memo holds the bare
	// *plans.RecordQueryStreamingAggregationPlan (no physicalStreamingAggWrapper).
	scan := implementUniqueScan("T", false)
	aggPlan := mustImplementUniqueConstruct(plans.NewRecordQueryStreamingAggregationPlan(scan, nil, nil))
	aggWrapper := aggPlan

	innerRef := expressions.InitialOf(aggWrapper)
	pm := NewPlanPropertiesMap()
	pm.Add(aggWrapper)
	innerRef.SetPlanProperties(pm)

	unique := mustImplementUniqueConstruct(expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(innerRef)))
	outerRef := expressions.InitialOf(unique)

	results := mustFireImplementationRule(t, NewImplementUniqueRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("ImplementUniqueRule should NOT yield when inner is not distinct, got %d results", len(results))
	}
}

func TestImplementUniqueRule_RejectsNilInnerRef(t *testing.T) {
	t.Parallel()
	// RFC-232 moves this malformed boundary ahead of rule matching: an exact
	// Unique cannot be published without a ranged-over result type.
	unique, err := expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(nil))
	if err == nil || unique != nil {
		t.Fatalf("nil-inner Unique = (%#v, %v), want constructor rejection", unique, err)
	}
}
