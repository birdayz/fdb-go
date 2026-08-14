package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustPlannerFuzzConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct planner-fuzz fixture: " + err.Error())
	}
	return value
}

func plannerFuzzRowType() values.Type {
	return values.NewRecordType("PlannerFuzzRow", false, []values.Field{
		{Name: "K", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "g", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "x", FieldType: values.NullableLong, Ordinal: 2},
	})
}

func plannerFuzzRoot(q expressions.Quantifier) values.QuantifiedObjectValue {
	flowedType := mustPlannerFuzzConstruct(q.GetFlowedObjectType())
	return mustPlannerFuzzConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), flowedType))
}

func plannerFuzzField(q expressions.Quantifier, ordinal int) values.Value {
	root := plannerFuzzRoot(q)
	recordType, ok := root.FlowedType().(*values.RecordType)
	if !ok || len(recordType.Fields) == 0 {
		panic("planner-fuzz field input does not have a non-empty exact record type")
	}
	if ordinal >= len(recordType.Fields) {
		ordinal = len(recordType.Fields) - 1
	}
	return mustPlannerFuzzConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func plannerFuzzProjectedFields(q expressions.Quantifier) []values.Value {
	root := plannerFuzzRoot(q)
	recordType, ok := root.FlowedType().(*values.RecordType)
	if !ok {
		panic("planner-fuzz projection input does not have an exact record type")
	}
	projected := make([]values.Value, len(recordType.Fields))
	for i := range recordType.Fields {
		projected[i] = mustPlannerFuzzConstruct(values.ResolveFieldOrdinals(root, []int{i}))
	}
	return projected
}

func plannerFuzzSameResultType(left, right expressions.Quantifier) bool {
	leftType := mustPlannerFuzzConstruct(left.GetFlowedObjectType())
	rightType := mustPlannerFuzzConstruct(right.GetFlowedObjectType())
	return leftType.Equals(rightType)
}

// FuzzPlanner_Determinism pins that the task-stack Planner produces
// the SAME final Reference state on identical inputs. Two fresh
// Planner instances, same expression tree, same rules, same order →
// same member count.
//
// Catches non-determinism bugs (map iteration order leaking into
// results, pointer-address-dependent behavior, etc.).
//
// Note: the Planner is NOT rule-order-independent — the Memo's
// cross-Reference sharing makes earlier rules' yields visible to
// later rules within the same round (Memo state grows during rule
// execution). This matches Java's Cascades (fixed rule registration
// order, deterministic result). The invariant tested here is: given
// the same inputs and same rule order, the result is identical.
func FuzzPlanner_Determinism(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		exprA := buildFuzzExpression(b, 0, 0)
		exprB := buildFuzzExpression(b, 0, 0)
		refA := expressions.InitialOf(exprA)
		refB := expressions.InitialOf(exprB)
		rules := selectRules(b)

		// Driver A: fresh Planner.
		pA := NewPlanner(rules, nil)
		if _, convA := exploreRewriting(pA, refA); !convA {
			t.Fatal("Planner A did not converge — possible non-terminating rule interaction")
		}

		// Driver B: fresh Planner, same rules same order.
		pB := NewPlanner(rules, nil)
		if _, convB := exploreRewriting(pB, refB); !convB {
			t.Fatal("Planner B did not converge on input where Planner A did")
		}

		// Member counts must be identical (determinism).
		if a, b := len(refA.Members()), len(refB.Members()); a != b {
			t.Fatalf("run A produced %d members; run B produced %d — non-determinism", a, b)
		}
	})
}

// FuzzPlanner_Idempotence pins that exploration is idempotent: a
// second drive over the same Reference doesn't grow the member set
// (Reference.CommitExploration marks converged groups done).
func FuzzPlanner_Idempotence(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)
		p := NewPlanner(rules, nil)

		if _, convA := exploreRewriting(p, ref); !convA {
			t.Fatal("first exploration did not converge — possible non-terminating rule interaction")
		}
		size1 := len(ref.Members())

		if _, convB := exploreRewriting(p, ref); !convB {
			t.Fatal("second exploration did not converge")
		}
		if got := len(ref.Members()); got != size1 {
			t.Fatalf("second exploration grew Reference from %d to %d (non-idempotent)", size1, got)
		}
	})
}

// FuzzPlanner_PlanFullPipeline pins that the full Plan() entry
// point (REWRITING + PLANNING) doesn't panic on random inputs and
// returns either a valid plan or ErrPlannerCapHit. Catches
// pathological inputs that break the OPTIMIZE phase or the
// extract recursion.
func FuzzPlanner_PlanFullPipeline(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	// Regression seed: decodes to
	//   Union(TypeFilter(TypeFilter(Scan)), TypeFilter(Filter(Scan)))
	// — asymmetric union legs. The right leg's group, merged with the left
	// leg's inner via constant-true filter elimination, crossed
	// REWRITING→PLANNING through the no-finals stage path and never reset
	// its exploration state, so it never implemented and the union produced
	// no winner while Plan() reported success. Fixed by
	// AdvanceStagePreservingMembers (see asymmetric_union_planning_test.go).
	f.Add([]byte{35, 4, 4, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)
		p := NewPlanner(rules, nil).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		p.MaxTasks = 100_000

		plan, _, err := p.Plan(ref)
		if err != nil && !errors.Is(err, ErrPlannerCapHit) {
			t.Fatalf("Plan: unexpected err %v", err)
		}
		if err == nil && plan == nil {
			t.Fatal("Plan returned nil plan + nil error — invariant break")
		}
		// Plan called BestMember internally; verify it's populated
		// for the root.
		if err == nil && !p.HasBestMember(ref) {
			t.Fatal("Plan succeeded but root Reference has no BestMember stamp")
		}
	})
}

// FuzzPlanner_MemoConsistency pins that after exploration, the Memo's
// internal index is consistent: every Reference in the Memo has all
// its members' child References also in the Memo.
func FuzzPlanner_MemoConsistency(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{3, 7, 2, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)
		p := NewPlanner(rules, nil)
		p.MaxTasks = 100_000

		if _, conv := exploreRewriting(p, ref); !conv {
			t.Fatal("exploration did not converge — possible non-terminating rule interaction")
		}
		memo := p.Memo()
		if memo == nil {
			t.Fatal("Memo is nil after exploration")
		}
		// Verify every Reference in the Memo has its children also indexed.
		for mRef := range memo.References() {
			for _, member := range mRef.Members() {
				for _, q := range member.GetQuantifiers() {
					child := q.GetRangesOver()
					if child == nil {
						continue
					}
					if !memo.ContainsReference(child) {
						t.Fatalf("Memo inconsistency: child Reference of %T not in Memo", member)
					}
				}
			}
		}
	})
}

// FuzzPlanner_InitialMemberPreserved pins that the original input
// expression is never removed from the Reference — rules can ADD
// alternatives but not REPLACE the input.
func FuzzPlanner_InitialMemberPreserved(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		initial := ref.Get()
		rules := selectRules(b)
		p := NewPlanner(rules, nil)

		if _, conv := exploreRewriting(p, ref); !conv {
			t.Fatal("exploration did not converge — possible non-terminating rule interaction")
		}
		members := ref.Members()
		if len(members) == 0 || members[0] != initial {
			t.Fatalf("initial member not preserved at index 0 (members[0]=%T, initial=%T)", members[0], initial)
		}
	})
}

// buildFuzzExpression constructs a small random expression tree from a
// byte stream. Shared by the planner and cost fuzzers.
func buildFuzzExpression(b []byte, start, depth int) expressions.RelationalExpression {
	if depth >= 3 || len(b) == 0 {
		return mustPlannerFuzzConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"T"}, plannerFuzzRowType()))
	}
	op := b[start%len(b)] % 10
	switch op {
	case 0:
		return mustPlannerFuzzConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"T"}, plannerFuzzRowType()))
	case 1:
		// Filter over a random child.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		pT := predicates.NewConstantPredicate(predicates.TriTrue)
		return mustPlannerFuzzConstruct(expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{pT}, q))
	case 2:
		// Distinct over a random child.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return mustPlannerFuzzConstruct(expressions.NewLogicalDistinctExpression(q))
	case 3:
		// Per-field identity projection over a random child's exact row,
		// exercising projection planning without constructing a one-slot
		// whole-row wrapper.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return mustPlannerFuzzConstruct(expressions.NewLogicalProjectionExpression(
			plannerFuzzProjectedFields(q), q))
	case 4:
		// TypeFilter over a random child.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return mustPlannerFuzzConstruct(expressions.NewLogicalTypeFilterExpression([]string{"X"}, q))
	case 5:
		// Union of two random children — exercises UnionMerge,
		// UnionSingletonElim, and any future Union-aware rule.
		left := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		right := buildFuzzExpression(b, (start+2)%len(b), depth+1)
		ql := expressions.ForEachQuantifier(expressions.InitialOf(left))
		qr := expressions.ForEachQuantifier(expressions.InitialOf(right))
		if !plannerFuzzSameResultType(ql, qr) {
			// SQL has coerced both UNION legs to one row shape before this
			// boundary. Random bytes have not; keep the generated tree valid.
			return left
		}
		return mustPlannerFuzzConstruct(expressions.NewLogicalUnionExpression(
			[]expressions.Quantifier{ql, qr}))
	case 6:
		// Single-child Union — exercises UnionSingletonElim directly.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return mustPlannerFuzzConstruct(expressions.NewLogicalUnionExpression(
			[]expressions.Quantifier{q}))
	case 7:
		// A sorted intersection over two typed PK scans. The intersection
		// executor is a merge, so arbitrary random children are not a valid
		// fixture: every leg must emit the non-empty comparison key
		// monotonically and the runtime key must be ordinal-bakeable.
		rt := plannerFuzzRowType()
		keyRoot := mustPlannerFuzzConstruct(values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("planner_fuzz_intersection_key"), rt))
		key := mustPlannerFuzzConstruct(values.ResolveOrdinalSeedField(keyRoot, 0))
		left := mustPlannerFuzzConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, rt, false)).
			WithPrimaryKey([]values.Value{key}).
			WithKeyComponentTypes([]values.Type{values.NotNullLong})
		right := mustPlannerFuzzConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, rt, false)).
			WithPrimaryKey([]values.Value{key}).
			WithKeyComponentTypes([]values.Type{values.NotNullLong})
		ql := expressions.ForEachQuantifier(expressions.InitialOf(left))
		qr := expressions.ForEachQuantifier(expressions.InitialOf(right))
		return mustPlannerFuzzConstruct(expressions.NewLogicalIntersectionExpression(
			[]expressions.Quantifier{ql, qr},
			[]values.Value{key},
		))
	case 8:
		// GroupBy over a random child — exercises GroupByExpression
		// integration with cost model and ordering property.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return mustPlannerFuzzConstruct(expressions.NewGroupByExpression(
			[]values.Value{plannerFuzzField(q, 1)},
			[]expressions.AggregateSpec{{
				Function: expressions.AggCount,
				Operand:  plannerFuzzField(q, 2),
			}},
			q,
		))
	default:
		// UnsortedSort over a random child.
		inner := buildFuzzExpression(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return mustPlannerFuzzConstruct(expressions.UnsortedLogicalSortExpression(q))
	}
}

// selectRules picks a subset of the default rules from a byte mask.
// Shared by the planner and cost fuzzers.
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
