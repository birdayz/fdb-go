package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestValidatePlanInvariants_NilInnerChild is the committed detection proof for
// RFC-164 WS-2: a non-leaf plan whose required inner is nil (the IN-LIMIT bug
// shape — GetChildren masks the nil as zero children) must be rejected, while a
// genuine leaf and a well-formed operator pass. The end-to-end mutation proof
// (revert the IN-LIMIT relink fix → PlanQueryForTest reports "plan invariant
// violated: ... Fetch(<nil>)") is captured in the PR; this pins the detector.
func TestValidatePlanInvariants_NilInnerChild(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)

	// Genuine leaf — legitimately childless.
	if err := ValidatePlanInvariants(scan); err != nil {
		t.Fatalf("scan leaf must pass: %v", err)
	}
	// Non-leaf operator with a nil inner — the malformed shape.
	if err := ValidatePlanInvariants(plans.NewRecordQueryLimitPlan(nil, 5, 0)); err == nil {
		t.Fatal("a Limit with a nil inner must violate the no-nil-child invariant")
	}
	// Well-formed operator — passes.
	if err := ValidatePlanInvariants(plans.NewRecordQueryLimitPlan(scan, 5, 0)); err != nil {
		t.Fatalf("well-formed Limit must pass: %v", err)
	}
	// Nested: Limit(Limit(nil)) — the inner malformation is reached by the walk.
	if err := ValidatePlanInvariants(plans.NewRecordQueryLimitPlan(plans.NewRecordQueryLimitPlan(nil, 1, 0), 5, 0)); err == nil {
		t.Fatal("a nested nil inner must be reached and rejected")
	}
}

// TestPlanInvariants_ChildlessClassification pins the childless-allowed set
// (genuine leaves + empty n-ary set ops) against the plans package, so a future
// change to a plan type's child shape can't silently slip the invariant. An
// interim guard until RFC-164 WS-3's RecordQueryPlanVisitor makes leaf-ness
// type-encoded / compile-time.
func TestPlanInvariants_ChildlessClassification(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	// Genuine leaves legitimately have zero children.
	if err := ValidatePlanInvariants(scan); err != nil {
		t.Errorf("genuine leaf %T must be allowed childless: %v", scan, err)
	}
	// Childless NON-leaf operators must be rejected — both a unary inner-drop and
	// a zero-leg n-ary set op (the n-ary analog: degenerate, never legitimately
	// emitted, so flagging it is a true positive, not a false one).
	for _, p := range []plans.RecordQueryPlan{
		plans.NewRecordQueryLimitPlan(nil, 1, 0),
		plans.NewRecordQueryUnionPlan(nil),
	} {
		if err := ValidatePlanInvariants(p); err == nil {
			t.Errorf("childless non-leaf %T must be rejected", p)
		}
	}
}

// TestValidatePlanInvariants_InJoinSortedClaim is the red/green proof for the
// sorted-IN-join invariant (plan_invariants.go's validateInJoinSortedClaim):
// a RecordQueryInJoinPlan claiming IsSorted() must actually carry its values
// in that order. This is the landmine CQ-10f's HintOrdering step would have
// re-armed silently — before this invariant, a plan could claim sorted=true
// over an arbitrarily-ordered list and nothing would catch it until a reader
// of the claim (like HintOrdering) shipped visibly wrong rows.
//
// RED PROOF: reverting the sortInJoinValues call in
// rule_implement_in_join.go's OnMatch (so SetInValues gets the raw unsorted
// extractInValues() result while sorted stays true) makes this test fail —
// verified by hand while writing this fix, not asserted here as a tautology.
func TestValidatePlanInvariants_InJoinSortedClaim(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)

	mk := func(sorted, reverse bool, vals []any) *plans.RecordQueryInJoinPlan {
		p := plans.NewRecordQueryInJoinPlan(scan, "__in_value", sorted, reverse)
		p.SetInValues(vals)
		return p
	}

	// Ascending claim, ascending values: passes.
	if err := ValidatePlanInvariants(mk(true, false, []any{int64(1), int64(2), int64(3)})); err != nil {
		t.Errorf("ascending values under an ascending claim must pass: %v", err)
	}
	// Descending claim, descending values: passes.
	if err := ValidatePlanInvariants(mk(true, true, []any{int64(3), int64(2), int64(1)})); err != nil {
		t.Errorf("descending values under a descending claim must pass: %v", err)
	}
	// Unsorted claim: never checked, any order passes.
	if err := ValidatePlanInvariants(mk(false, false, []any{int64(3), int64(1), int64(2)})); err != nil {
		t.Errorf("an unsorted claim must never be checked: %v", err)
	}
	// Fewer than 2 values: vacuously satisfiable regardless of claim.
	if err := ValidatePlanInvariants(mk(true, false, []any{int64(1)})); err != nil {
		t.Errorf("a single value must pass under any claim: %v", err)
	}
	if err := ValidatePlanInvariants(mk(true, false, nil)); err != nil {
		t.Errorf("no values (runtime source) must pass under any claim: %v", err)
	}

	// THE LANDMINE: sorted=true but the values are NOT ascending. This is
	// exactly the shape the pre-fix rule_implement_in_join.go produced for
	// every literal IN-list that wasn't already given in sorted order.
	if err := ValidatePlanInvariants(mk(true, false, []any{int64(3), int64(1), int64(2)})); err == nil {
		t.Fatal("an InJoin claiming sorted=true over out-of-order values must be rejected")
	}
	// Same landmine, descending claim.
	if err := ValidatePlanInvariants(mk(true, true, []any{int64(1), int64(3), int64(2)})); err == nil {
		t.Fatal("an InJoin claiming sorted(reverse)=true over out-of-order values must be rejected")
	}
	// Ties are fine (non-strict): repeated values don't violate either
	// direction.
	if err := ValidatePlanInvariants(mk(true, false, []any{int64(1), int64(1), int64(2)})); err != nil {
		t.Errorf("a tie must not violate the sorted claim: %v", err)
	}
}

// TestSortInJoinValues pins sortInJoinValues (in_source.go) against Java's
// InSource.sortValues (InSource.java:142-149): size<2 returns the input
// unchanged, size>=2 returns a freshly sorted copy (stable, ascending unless
// reverse), and an incomparable pair declines (ok=false) rather than
// picking an arbitrary order.
func TestSortInJoinValues(t *testing.T) {
	t.Parallel()

	t.Run("size under two returns input unchanged", func(t *testing.T) {
		t.Parallel()
		in := []any{int64(5)}
		got, ok := sortInJoinValues(in, false)
		if !ok {
			t.Fatal("single element must always be sortable")
		}
		if len(got) != 1 || got[0] != int64(5) {
			t.Fatalf("got %#v, want unchanged %#v", got, in)
		}
		empty, ok := sortInJoinValues(nil, false)
		if !ok || len(empty) != 0 {
			t.Fatalf("empty input: got %#v ok=%v", empty, ok)
		}
	})

	t.Run("ascending", func(t *testing.T) {
		t.Parallel()
		got, ok := sortInJoinValues([]any{int64(3), int64(1), int64(2)}, false)
		if !ok {
			t.Fatal("integers must be sortable")
		}
		want := []any{int64(1), int64(2), int64(3)}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %#v, want %#v", got, want)
			}
		}
	})

	t.Run("descending", func(t *testing.T) {
		t.Parallel()
		got, ok := sortInJoinValues([]any{int64(1), int64(3), int64(2)}, true)
		if !ok {
			t.Fatal("integers must be sortable")
		}
		want := []any{int64(3), int64(2), int64(1)}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %#v, want %#v", got, want)
			}
		}
	})

	t.Run("original slice is not mutated", func(t *testing.T) {
		t.Parallel()
		in := []any{int64(3), int64(1), int64(2)}
		_, ok := sortInJoinValues(in, false)
		if !ok {
			t.Fatal("integers must be sortable")
		}
		want := []any{int64(3), int64(1), int64(2)}
		for i := range want {
			if in[i] != want[i] {
				t.Fatalf("input slice was mutated: %#v, want %#v", in, want)
			}
		}
	})

	t.Run("incomparable pair declines rather than picking an order", func(t *testing.T) {
		t.Parallel()
		in := []any{int64(1), "not-an-int"}
		got, ok := sortInJoinValues(in, false)
		if ok {
			t.Fatalf("cross-type pair must decline sorting, got ok=true values=%#v", got)
		}
		// Declining returns the ORIGINAL slice unchanged.
		if got[0] != in[0] || got[1] != in[1] {
			t.Fatalf("declined sort must return the original slice, got %#v", got)
		}
	})

	t.Run("ties are preserved in count and position", func(t *testing.T) {
		t.Parallel()
		got, ok := sortInJoinValues([]any{int64(2), int64(1), int64(1), int64(3)}, false)
		if !ok {
			t.Fatal("integers must be sortable")
		}
		want := []any{int64(1), int64(1), int64(2), int64(3)}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %#v, want %#v", got, want)
			}
		}
	})
}

// FuzzPlanner_Invariants asserts that EVERY successfully-planned random query
// satisfies the WS-2 structural invariants — a relink that drops a child on any
// input shape is caught here, always-on, with no Java/FDB dependency.
func FuzzPlanner_Invariants(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
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
		if err != nil || plan == nil {
			return
		}
		ppe, ok := plan.(physicalPlanExpression)
		if !ok {
			return
		}
		rqp := ppe.GetRecordQueryPlan()
		if rqp == nil {
			return
		}
		if verr := ValidatePlanInvariants(rqp); verr != nil {
			t.Fatalf("planner produced a malformed plan for input %v: %v", b, verr)
		}
	})
}
