package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzFixpointApply pins three properties of the rule-engine driver:
//
//  1. Termination: FixpointApply on any random combination of seed
//     rules + random expression tree completes within the iter cap.
//     If converged is false, that's a sign of an infinite loop.
//  2. Idempotence at convergence: re-running FixpointApply on a
//     converged Reference yields zero new members.
//  3. Initial member preservation: the first member of the Reference
//     (the input expression) is always preserved — rules add, never
//     remove.
//
// The fuzzer constructs small expression trees from a byte stream
// and a subset of the default rules selected by another byte mask.
func FuzzFixpointApply(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		initialMember := ref.Get()
		rules := selectRules(b)

		_, converged := FixpointApply(rules, ref, 50)
		if !converged {
			t.Fatalf("FixpointApply didn't converge in 50 iters with %d rules — possible infinite loop", len(rules))
		}

		// Idempotence: second call should make no progress.
		progress2, converged2 := FixpointApply(rules, ref, 5)
		if !converged2 {
			t.Fatalf("second FixpointApply call didn't converge — non-deterministic rule fire")
		}
		if progress2 != 0 {
			t.Fatalf("second FixpointApply call grew the Reference by %d (rule isn't idempotent at convergence)", progress2)
		}

		// Initial member preserved.
		members := ref.Members()
		if len(members) == 0 || members[0] != initialMember {
			t.Fatalf("initial member not preserved at index 0 (members[0]=%T, initial=%T)", members[0], initialMember)
		}
	})
}

func buildFuzzExpression(b []byte, start, depth int) expressions.RelationalExpression {
	if depth >= 3 || len(b) == 0 {
		return expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	}
	op := b[start%len(b)] % 10
	switch op {
	case 0:
		return expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	case 1:
		// Filter over a random child.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		pT := predicates.NewConstantPredicate(predicates.TriTrue)
		return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
	case 2:
		// Distinct over a random child.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.NewLogicalDistinctExpression(q)
	case 3:
		// Projection over a random child (single column = inner's
		// flowed object — identity projection, exercises ProjectionElim).
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.NewLogicalProjectionExpression(
			[]values.Value{q.GetFlowedObjectValue()}, q)
	case 4:
		// TypeFilter over a random child.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.NewLogicalTypeFilterExpression([]string{"X"}, q)
	case 5:
		// Union of two random children — exercises UnionMerge,
		// UnionSingletonElim, and any future Union-aware rule.
		left := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		right := buildFuzzExpression(b, (start+2)%len(b), depth+1)
		ql := expressions.ForEachQuantifier(expressions.InitialOf(left))
		qr := expressions.ForEachQuantifier(expressions.InitialOf(right))
		return expressions.NewLogicalUnionExpression([]expressions.Quantifier{ql, qr})
	case 6:
		// Single-child Union — exercises UnionSingletonElim directly.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.NewLogicalUnionExpression([]expressions.Quantifier{q})
	case 7:
		// Intersection over two random children with a single
		// FieldValue comparison key — exercises IntersectionMerge,
		// IntersectionSingletonElim, PushFilterThroughIntersection.
		left := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		right := buildFuzzExpression(b, (start+2)%len(b), depth+1)
		ql := expressions.ForEachQuantifier(expressions.InitialOf(left))
		qr := expressions.ForEachQuantifier(expressions.InitialOf(right))
		keys := []values.Value{&values.FieldValue{Field: "k", Typ: values.UnknownType}}
		return expressions.NewLogicalIntersectionExpression([]expressions.Quantifier{ql, qr}, keys)
	case 8:
		// GroupBy over a random child — exercises GroupByExpression
		// integration with cost model and ordering property.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.NewGroupByExpression(
			[]values.Value{&values.FieldValue{Field: "g", Typ: values.UnknownType}},
			[]expressions.AggregateSpec{{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}}},
			q,
		)
	default:
		// UnsortedSort over a random child.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.UnsortedLogicalSortExpression(q)
	}
}

func selectRules(b []byte) []ExpressionRule {
	all := DefaultExpressionRules()
	if len(b) < 1 {
		return all
	}
	mask := b[0]
	out := make([]ExpressionRule, 0, len(all))
	for i, r := range all {
		if mask&(1<<uint(i%8)) != 0 {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		// At least one rule — exercises the no-progress path differently.
		return all[:1]
	}
	return out
}
