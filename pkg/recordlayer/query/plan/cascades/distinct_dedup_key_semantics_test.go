package cascades

// The dedup-key semantics of the two distinct nodes, pinned at the rule level.
//
// Go has two dedup nodes where Java has two differently-shaped ones, and the
// names do not line up:
//
//	Java LogicalDistinctExpression   = PRIMARY-KEY dedup, materialized when the
//	                                   input is not already distinct
//	Java LogicalUniqueExpression     = PRIMARY-KEY dedup, absorb-only
//	Go   LogicalDistinctExpression   = FULL-ROW dedup (carries SELECT DISTINCT,
//	                                   which Java's Cascades has no node for)
//	Go   LogicalUniqueExpression     = PRIMARY-KEY dedup (required mode
//	                                   materializes, ordinary mode absorbs)
//
// So a port that carried Java's node NAME across carried the wrong MEANING. The
// tests here assert the meaning, because a shape assertion cannot: the correct
// and the incorrect plan differ only in which key a dedup node uses, and both
// spellings satisfy "a distinct node is present".

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// firePredicateUnionRuleOnSingleOR runs the OR-to-union rewrite over the
// simplest shape that triggers it — SELECT WHERE (A OR B) — and returns what it
// yielded.
func firePredicateUnionRuleOnSingleOR(t testing.TB) []expressions.RelationalExpression {
	t.Helper()
	orPred := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriFalse),
	)
	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{orPred})
	return mustFirePredicateUnionRule(t, NewPredicateToLogicalUnionRule(), ref)
}

// TestPushDistinctThroughFetch_DeclinesRowDistinct is the soundness pin.
//
// Pushing a dedup below a fetch is sound only for a key the PARTIAL row already
// carries and that identifies the record — the primary key. A full-ROW dedup
// below the fetch dedups partial rows, and two partial rows for the SAME record
// differ whenever they come from different covering indexes, so the record
// survives twice and the fetch materializes it twice.
//
// Java's PushDistinctThroughFetchRule matches
// unorderedPrimaryKeyDistinctPlan(fetchFromPartialRecordPlan(anyPlan())) — the
// primary-key node only. This test pins that Go declines the other one.
func TestPushDistinctThroughFetch_DeclinesRowDistinct(t *testing.T) {
	t.Parallel()

	indexPlan := pushFetchIndex("idx_a")
	indexRef := expressions.InitialOf(indexPlan)
	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	rowDistinct := mustPushFetchConstruct(plans.NewRecordQueryDistinctPlan(indexPlan))
	distinctQ := expressions.ForEachQuantifier(fetchRef)
	distinctWrapper := mustWithQuantifiers(t, rowDistinct, []expressions.Quantifier{distinctQ})

	yielded := mustFireImplementationRule(t, NewPushDistinctThroughFetchRule(),
		expressions.InitialOf(distinctWrapper))

	if len(yielded) != 0 {
		t.Fatalf("a full-ROW distinct was pushed below a fetch (%d plan(s) yielded), which is unsound: "+
			"below the fetch the rows are PARTIAL records, and two partial rows for the same record "+
			"differ whenever they come from different covering indexes, so the dedup collapses nothing "+
			"and the record is returned once per index that produced it. Java's rule matches "+
			"RecordQueryUnorderedPrimaryKeyDistinctPlan only.", len(yielded))
	}
}

// TestPushDistinctThroughFetch_FiresOnPrimaryKeyDistinct is the other half: the
// push must still happen for the node it IS sound for, otherwise "declines the
// row distinct" would be satisfied by a rule that declines everything.
func TestPushDistinctThroughFetch_FiresOnPrimaryKeyDistinct(t *testing.T) {
	t.Parallel()

	indexPlan := pushFetchIndex("idx_a")
	indexRef := expressions.InitialOf(indexPlan)
	translateFn := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
		return v, true
	}
	fetchPlan := pushFetchFetch(indexPlan, translateFn)
	fetchQ := expressions.ForEachQuantifier(indexRef)
	fetchWrapper := mustWithQuantifiers(t, fetchPlan, []expressions.Quantifier{fetchQ})
	fetchRef := expressions.InitialOf(fetchWrapper)

	pkDistinct := mustPushFetchConstruct(
		plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(
			expressions.ForEachQuantifier(fetchRef)))

	yielded := mustFireImplementationRule(t, NewPushDistinctThroughFetchRule(),
		expressions.InitialOf(pkDistinct))

	if len(yielded) != 1 {
		t.Fatalf("expected the primary-key distinct to be pushed below the fetch, got %d plan(s)", len(yielded))
	}
	if !IsPhysicalFetchFromPartialRecord(yielded[0]) {
		t.Fatalf("expected Fetch(PrimaryKeyDistinct(inner)) on top, got %T", yielded[0])
	}
	inner := yielded[0].(interface {
		GetChildren() []plans.RecordQueryPlan
	}).GetChildren()
	if len(inner) != 1 {
		t.Fatalf("pushed fetch has %d children, want 1", len(inner))
	}
	if _, ok := inner[0].(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan); !ok {
		t.Fatalf("the node pushed below the fetch is %T, want *plans.RecordQueryUnorderedPrimaryKeyDistinctPlan",
			inner[0])
	}
}

// TestPredicateToLogicalUnionRule_CrossLegDedupIsPrimaryKey pins the meaning of
// the cross-leg dedup the OR-to-union rewrite installs.
//
// The legs are separate access paths over the same table, so a record
// satisfying two DNF terms is produced by two legs. Those legs may be covering
// scans of DIFFERENT indexes, whose rows differ for the same record — so the
// dedup that collapses them has to key on the primary key. Java spells this
// LogicalDistinctExpression and implements it as
// RecordQueryUnorderedPrimaryKeyDistinctPlan; in Go the primary-key node is
// LogicalUniqueExpression.
func TestPredicateToLogicalUnionRule_CrossLegDedupIsPrimaryKey(t *testing.T) {
	t.Parallel()

	yielded := firePredicateUnionRuleOnSingleOR(t)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	unique, ok := yielded[0].(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf("cross-leg dedup is %T, want *expressions.LogicalUniqueExpression (a PRIMARY-KEY dedup).\n"+
			"A *LogicalDistinctExpression here is a FULL-ROW dedup: it cannot collapse the two rows a "+
			"single record produces when the union's legs are covering scans of different indexes, so "+
			"the record is returned once per matching leg.", yielded[0])
	}
	if !unique.IsRequired() {
		t.Fatalf("the cross-leg dedup is an ABSORBABLE Unique. ImplementUniqueRule only absorbs — it " +
			"yields no plan for an input that is not already distinct — and a union of legs never is, " +
			"so the access path would silently fail to be implemented rather than dedup.")
	}
}
