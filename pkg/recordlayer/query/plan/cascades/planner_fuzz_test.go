package cascades

import (
	"errors"
	"fmt"
	"strings"
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

// FuzzPlanner_Determinism pins that two fresh Planners over two copies of the
// same tree explore the same memo (members in order) and choose the same plan.
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
	f.Add([]byte{0xff, 5, 1, 3, 2, 7})
	f.Add([]byte{0xff, 6, 3, 1, 4, 8, 2})
	f.Add([]byte{35, 4, 4, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		rules := selectRules(b)
		explore := func() string {
			ref := expressions.InitialOf(buildFuzzExpression(b, 0, 0))
			if _, converged := exploreRewriting(NewPlanner(rules, nil), ref); !converged {
				t.Fatal("exploration did not converge — possible non-terminating rule interaction")
			}
			return renderMemoForDeterminism(ref)
		}
		if a, b := explore(), explore(); a != b {
			t.Fatalf("two explorations of the same tree differ:\n--- A ---\n%s\n--- B ---\n%s", a, b)
		}

		plan := func() string {
			ref := expressions.InitialOf(buildFuzzExpression(b, 0, 0))
			p := NewPlanner(rules, nil).
				WithPlanningExpressionRules(BatchAExpressionRules()).
				WithImplementationRules(DefaultImplementationRules())
			p.MaxTasks = 100_000
			best, _, err := p.Plan(ref)
			if err != nil {
				return "<error: " + err.Error() + ">"
			}
			return canonicalPipelineExplain(ExplainPhysicalPlan(best))
		}
		if a, b := plan(), plan(); a != b {
			t.Fatalf("two plannings of the same tree chose different plans:\n  A: %s\n  B: %s", a, b)
		}
	})
}

// renderMemoForDeterminism renders every Reference reachable from root, in
// first-visit order, as its members in insertion order: each member's type,
// result value, predicates and child References.
func renderMemoForDeterminism(root *expressions.Reference) string {
	ids := map[*expressions.Reference]int{}
	var refs []*expressions.Reference
	var visit func(*expressions.Reference) int
	visit = func(ref *expressions.Reference) int {
		if id, ok := ids[ref]; ok {
			return id
		}
		ids[ref] = len(refs)
		refs = append(refs, ref)
		for _, member := range ref.AllMembers() {
			for _, q := range member.GetQuantifiers() {
				if child := q.GetRangesOver(); child != nil {
					visit(child)
				}
			}
		}
		return ids[ref]
	}
	visit(root)

	var out strings.Builder
	for id, ref := range refs {
		fmt.Fprintf(&out, "R%d:\n", id)
		for _, member := range ref.AllMembers() {
			fmt.Fprintf(&out, "  %T", member)
			if rv := member.GetResultValue(); rv != nil {
				fmt.Fprintf(&out, " result=%s", values.ExplainValue(rv))
			}
			if wp, ok := member.(expressions.RelationalExpressionWithPredicates); ok {
				for _, pred := range wp.GetPredicates() {
					fmt.Fprintf(&out, " pred=%s", pred.Explain())
				}
			}
			for _, q := range member.GetQuantifiers() {
				if child := q.GetRangesOver(); child != nil {
					fmt.Fprintf(&out, " %s->R%d", q.GetAlias(), ids[child])
				}
			}
			out.WriteByte('\n')
		}
	}
	return canonicalPipelineExplain(out.String())
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
	// leg's never-explored inner via constant-true filter elimination,
	// inherited the right leg's exploration progress, so the union reached
	// PLANNING unfinalized. Fixed in Reference.Absorb (see
	// asymmetric_union_planning_test.go).
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
		return mustPlannerFuzzConstruct(newBlockSelectForTest(
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
