package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// RewriteOuterJoinRule carries LEFT-OUTER semantics on a NullOnEmpty edge of an
// otherwise INNER Select. Only the correlated FlatMap lowering knows how to
// turn that edge into DefaultOnEmpty. If correlation is absent (or a buried
// rebase later declines), implementing this rewritten member as an ordinary
// materialized INNER NLJ drops unmatched rows. The original LEFT-OUTER member
// is the materialized fallback and must remain independently implementable.
func TestImplementNestedLoopJoin_NullOnEmptyRequiresFlatMap(t *testing.T) {
	t.Parallel()

	newScanRef := func(t *testing.T, name string) *expressions.Reference {
		t.Helper()
		rowType := values.NewRecordType(name, false, []values.Field{{
			Name: "ID", FieldType: values.NotNullLong, Ordinal: 0,
		}})
		logicalScan, err := expressions.NewFullUnorderedScanExpression([]string{name}, rowType)
		if err != nil {
			t.Fatalf("build logical %s scan: %v", name, err)
		}
		ref := expressions.InitialOf(logicalScan)
		physicalScan, err := plans.NewRecordQueryScanPlan([]string{name}, rowType, false)
		if err != nil {
			t.Fatalf("build physical %s scan: %v", name, err)
		}
		ref.InsertFinal(physicalScan)
		return ref
	}
	build := func(t *testing.T, nullSide string, joinType expressions.JoinType) *expressions.SelectExpression {
		t.Helper()
		leftAlias := values.NamedCorrelationIdentifier("L")
		rightAlias := values.NamedCorrelationIdentifier("R")
		leftRef, rightRef := newScanRef(t, "LEFT"), newScanRef(t, "RIGHT")
		leftQ := expressions.NamedForEachQuantifier(leftAlias, leftRef)
		rightQ := expressions.NamedForEachQuantifier(rightAlias, rightRef)
		if nullSide == "left" {
			leftQ = expressions.NamedForEachNullOnEmptyQuantifier(leftAlias, leftRef)
		}
		if nullSide == "right" {
			rightQ = expressions.NamedForEachNullOnEmptyQuantifier(rightAlias, rightRef)
		}
		leftResult, err := leftQ.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("derive left result value: %v", err)
		}
		sel, err := expressions.NewSelectExpressionWithJoinType(
			leftResult,
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"L", "R"},
			joinType,
		)
		if err != nil {
			t.Fatalf("build join type %v select: %v", joinType, err)
		}
		return sel
	}
	fire := func(t *testing.T, sel *expressions.SelectExpression) []expressions.RelationalExpression {
		t.Helper()
		results, err := FireExpressionRule(NewImplementNestedLoopJoinRule(), expressions.InitialOf(sel))
		if err != nil {
			t.Fatalf("fire ImplementNestedLoopJoinRule: %v", err)
		}
		return results
	}
	hasMaterializedNLJ := func(results []expressions.RelationalExpression) bool {
		for _, result := range results {
			if _, ok := result.(*plans.RecordQueryNestedLoopJoinPlan); ok {
				return true
			}
		}
		return false
	}

	for _, nullSide := range []string{"left", "right"} {
		t.Run("rewritten_inner_"+nullSide+"_null_on_empty", func(t *testing.T) {
			results := fire(t, build(t, nullSide, expressions.JoinInner))
			if hasMaterializedNLJ(results) {
				t.Fatalf("rewritten INNER with %s NullOnEmpty edge yielded a materialized NLJ", nullSide)
			}
			if len(results) != 0 {
				t.Fatalf("uncorrelated NullOnEmpty shape yielded %d non-FlatMap implementation(s)", len(results))
			}
		})
	}

	t.Run("ordinary_inner_control", func(t *testing.T) {
		results := fire(t, build(t, "", expressions.JoinInner))
		if !hasMaterializedNLJ(results) {
			t.Fatal("ordinary INNER select lost its materialized NLJ implementation")
		}
	})

	t.Run("original_left_outer_fallback_control", func(t *testing.T) {
		results := fire(t, build(t, "", expressions.JoinLeftOuter))
		foundLeftOuter := false
		for _, result := range results {
			if nlj, ok := result.(*plans.RecordQueryNestedLoopJoinPlan); ok && nlj.GetJoinType() == plans.JoinLeftOuter {
				foundLeftOuter = true
			}
		}
		if !foundLeftOuter {
			t.Fatal("original LEFT OUTER select lost its materialized fail-closed fallback")
		}
	})
}
