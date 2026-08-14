package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustRFC219Criterion2Construct[T any](value T, err error) T {
	if err != nil {
		panic("construct RFC-219 criterion-2 fixture: " + err.Error())
	}
	return value
}

func rfc219Criterion2RowType() *values.RecordType {
	return values.NewRecordType("RFC219Criterion2Row", false, []values.Field{{
		Name:      "V",
		FieldType: values.NullableLong,
	}})
}

// rfc219LogicalParent wraps a data-access plan in a logical filter with a
// constant-true predicate so findExpressionsByType takes the LOGICAL
// memo-descent walk (countLogicalPlanNode) rather than the concrete-plan walk.
// A bare physical plan is itself a physicalPlanExpression and would be routed
// straight to concretePlanCounts, which is the very path these probes are
// contrasting the logical walk against.
func rfc219LogicalParent(child expressions.RelationalExpression) expressions.RelationalExpression {
	return mustRFC219Criterion2Construct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		expressions.ForEachQuantifier(expressions.FinalOf(child)),
	))
}

// rfc219UniqueIndexCtx is the PlanContext both index probes run under: it
// declares IDX over record type T as a UNIQUE single-column index on V, and
// declares T's primary key as (V). One context object states everything either
// walk could need, so a difference in outcome is a difference in which walk
// consults it — not a difference in what was available.
func rfc219UniqueIndexCtx() *pkGateTestCtx {
	candidate := newKnownDistinctValueIndexCandidate(
		"IDX",
		[]string{"T"},
		[]string{"V"},
		[]values.CorrelationIdentifier{values.NamedCorrelationIdentifier("v")},
		rfc219Criterion2RowType(),
		true,
		[]string{"V"},
	)
	ctx := &pkGateTestCtx{pk: []string{"V"}}
	ctx.candidates = []MatchCandidate{candidate}
	return ctx
}

// rfc219UnstampedIndex builds an IDX probe fully equality-bound on its single
// key column, WITHOUT WithIndexMetadata — the shape a hand-built or
// pre-metadata-adapter plan carries: unique=false and nil column names.
func rfc219UnstampedIndex(t *testing.T) *plans.RecordQueryIndexPlan {
	t.Helper()
	return mustRFC219Criterion2Construct(plans.NewRecordQueryIndexPlan(
		"IDX",
		[]*predicates.ComparisonRange{pkGateEq(t, int64(7))},
		[]string{"T"},
		rfc219Criterion2RowType(),
		false,
	)).WithKeyComponentTypes([]values.Type{values.NullableLong})
}

// TestRFC219_LogicalWalkIndexArmIgnoresContext measures the asymmetry at the
// heart of RFC-219's criterion-2 walk: countLogicalPlanNode's INDEX arm calls
// the plan-local indexProvableMaxCard, which takes no PlanContext, so an
// unstamped index plan abstains — even though the very same PlanContext the
// walk is already holding carries the uniqueness and key-column metadata that
// makes the probe provably a single row. The concrete walk asks the context
// (indexMetadata + indexPlanProvableMaxCard) and proves the bound.
//
// This documents a LATENT asymmetry, not a reachable bug, and the distinction
// is load-bearing. Every production construction site stamps its index plan via
// stampIndexMetadata (aggregate_index_candidate.go:276, match_candidate_index.go:968,
// rule_implement_nested_loop_join.go:4373, windowed_index_match_candidate.go:337),
// so the unstamped shape built below is one production never constructs, and a
// reachability census over the SQL corpus reaches this arm zero times. Structurally
// an abstention would set unboundedDataAccess and erase maxDataAccessCardinality
// for the whole subtree — but no query gets there, so this test must not be read
// as evidence of a mis-ranked plan.
//
// It is kept because an unreachable arm is one refactor away from reachable: if a
// future construction path forgets the stamp, this test says exactly what breaks.
func TestRFC219_LogicalWalkIndexArmIgnoresContext(t *testing.T) {
	t.Parallel()

	ctx := rfc219UniqueIndexCtx()
	idx := rfc219UnstampedIndex(t)

	counts := findExpressionsByType(rfc219LogicalParent(idx), nil, ctx)
	if !counts.unboundedDataAccess || counts.maxDataAccessCardinality != -1 {
		t.Fatalf("logical walk index arm counts = {unbounded:%v max:%v}, want {true -1}: "+
			"this probe measures the arm DROPPING a bound, so a proven bound here means the "+
			"asymmetry is gone and the RFC-219 premise no longer holds",
			counts.unboundedDataAccess, counts.maxDataAccessCardinality)
	}

	// Same plan, same ctx, the ctx-aware pair the CONCRETE walk uses.
	cols, unique := indexMetadata(idx, ctx)
	if len(cols) != 1 || cols[0] != "V" || !unique {
		t.Fatalf("indexMetadata = (%v,%v), want ([V],true): the logical walk's index arm drops "+
			"a bound the concrete walk proves from the same ctx, and this fixture must show "+
			"that ctx really does carry the metadata",
			cols, unique)
	}
	card, known := indexPlanProvableMaxCard(idx, cols, unique)
	if !known || card != 1 {
		t.Fatalf("indexPlanProvableMaxCard = (%v,%v), want (1,true): the logical walk's index arm "+
			"drops a bound the concrete walk proves from the same ctx",
			card, known)
	}
}

// TestRFC219_LogicalWalkScanArmHonoursContext is the POSITIVE CONTROL that
// makes the probe above interpretable. It is the same walk, the same context
// object, and the same question — "is this data access a point probe?" — asked
// of a primary SCAN instead of an index. The scan arm calls the ctx-enabled
// scanPlanProvableMaxCard, resolves the primary-key arity from the context that
// the plan itself does not carry, and PROVES the bound.
//
// So the abstention in the index case is not "the logical walk cannot prove
// bounds", nor "the context lacks the facts", nor "an unstamped plan is
// inherently unprovable": all three are ruled out here. The only remaining
// difference is which of the two arms was given the context.
func TestRFC219_LogicalWalkScanArmHonoursContext(t *testing.T) {
	t.Parallel()

	ctx := rfc219UniqueIndexCtx()
	scan := mustRFC219Criterion2Construct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, rfc219Criterion2RowType(), false)).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))}).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	if got := len(scan.GetPrimaryKeyValues()); got != 0 {
		t.Fatalf("control scan stamped %d primary-key values, want 0 (it must be unstamped, "+
			"exactly like the index probe, or the comparison proves nothing)", got)
	}

	counts := findExpressionsByType(rfc219LogicalParent(scan), nil, ctx)
	if counts.unboundedDataAccess || counts.maxDataAccessCardinality != 1 {
		t.Fatalf("logical walk scan arm counts = {unbounded:%v max:%v}, want {false 1}: the scan "+
			"arm is the control that makes the index abstention interpretable, so if it stops "+
			"proving, the two arms are no longer distinguishable and the measurement is void",
			counts.unboundedDataAccess, counts.maxDataAccessCardinality)
	}
}

// TestRFC219_StampedIndexProvesInBothWalks is the SECOND control: it rules out
// a broken fixture. The index plan here is identical to the one that abstains,
// except that it carries WithIndexMetadata. The logical walk now proves the
// bound, which establishes that the comparison, the key-component types, the
// filter wrapper and the walk itself are all sound — the abstention above is
// caused solely by the MISSING STAMP on a path that had a context able to
// supply it.
func TestRFC219_StampedIndexProvesInBothWalks(t *testing.T) {
	t.Parallel()

	ctx := rfc219UniqueIndexCtx()
	stamped := rfc219UnstampedIndex(t).
		WithIndexMetadata([]string{"V"}, []string{"V"}, true)

	counts := findExpressionsByType(rfc219LogicalParent(stamped), nil, ctx)
	if counts.unboundedDataAccess || counts.maxDataAccessCardinality != 1 {
		t.Fatalf("stamped index logical counts = {unbounded:%v max:%v}, want {false 1}: this "+
			"control proves the fixture is capable of a bound at all, so a failure here means "+
			"the abstention measured elsewhere cannot be attributed to the missing stamp",
			counts.unboundedDataAccess, counts.maxDataAccessCardinality)
	}
}
