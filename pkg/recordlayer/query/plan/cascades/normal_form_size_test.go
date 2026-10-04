package cascades

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestNormalFormSize_IsNegateAware asserts the metric POSITIVELY, by named
// value, rather than by differential against the negate-blind walk it replaces
// — that walk is deleted, so a differential against it could only ever compare
// the new code to itself.
//
// The cost model reads this number: Java's countNormalizedConjuncts
// (NormalizedResidualPredicateProperty.java:81-90) is
// getMetrics(p).getNormalFormFullSize(), consumed at PlanningCostModel.java:142-143
// and RewritingCostModel.java:92-93. Go's designated_final.go and
// planning_cost_model.go are the ports of those readers.
//
// The load-bearing row is NOT(a OR b) under CNF. Under negation the Or is sized
// as a MAJOR, so its children SUM to 2; the retired walk recursed through the
// NOT without swapping roles and multiplied to 1. Every other row is here to
// show the swap is a swap and not a blanket increase.
func TestNormalFormSize_IsNegateAware(t *testing.T) {
	t.Parallel()
	a, b, c := normalFormTestLeaves(t)

	cases := []struct {
		name string
		pred predicates.QueryPredicate
		cnf  int64
		dnf  int64
	}{
		// A bare leaf, and a NOT over a leaf: no connective, so no swap.
		{"leaf", a, 1, 1},
		{"not-leaf", predicates.NewNot(a), 1, 1},

		// The positive forms. CNF majors (And) sum; CNF minors (Or) multiply.
		{"and", predicates.NewAnd(a, b), 2, 1},
		{"or", predicates.NewOr(a, b), 1, 2},
		{"and-of-ors", predicates.NewAnd(predicates.NewOr(a, b), predicates.NewOr(b, c)), 2, 4},

		// THE ROW THIS CHANGE IS ABOUT. Negated, the roles swap.
		{"not-or", predicates.NewNot(predicates.NewOr(a, b)), 2, 1},
		{"not-and", predicates.NewNot(predicates.NewAnd(a, b)), 1, 2},

		// A doubled NOT restores the un-negated sizing, which is what proves
		// the flag is carried rather than merely consulted once.
		{"not-not-or", predicates.NewNot(predicates.NewNot(predicates.NewOr(a, b))), 1, 2},

		// NOT over a nested shape: the swap recurses. Both expectations are
		// derived from the normal form itself rather than from walking the
		// code, because walking the code is how you re-assert whatever it
		// already does — an earlier draft of this row guessed 1 and 3 and both
		// were wrong.
		//
		//   NOT(a AND (b OR c))  ==  NOT a OR (NOT b AND NOT c)
		//     DNF majors (Or terms):  NOT a | (NOT b AND NOT c)          -> 2
		//     CNF, distributed:       (NOT a OR NOT b) AND (NOT a OR NOT c) -> 2
		{"not-and-of-or", predicates.NewNot(predicates.NewAnd(a, predicates.NewOr(b, c))), 2, 2},
	}

	swapped := 0
	for _, tc := range cases {
		if got := normalFormSize(tc.pred, false, normalFormCNF); got != tc.cnf {
			t.Errorf("%s: normalFormSize(%s, false, CNF) = %d, want %d",
				tc.name, tc.pred.Explain(), got, tc.cnf)
		}
		if got := normalFormSize(tc.pred, false, normalFormDNF); got != tc.dnf {
			t.Errorf("%s: normalFormSize(%s, false, DNF) = %d, want %d",
				tc.name, tc.pred.Explain(), got, tc.dnf)
		}
		if tc.cnf != tc.dnf {
			swapped++
		}
	}

	// Non-vacuity: if every row answered the same in both modes, the mode
	// parameter would be inert and this table would pass against a normalizer
	// that ignored it entirely.
	if swapped < 4 {
		t.Fatalf("only %d of %d rows distinguish CNF from DNF — the table does not "+
			"exercise the major/minor swap", swapped, len(cases))
	}
}

func normalFormTestLeaves(t *testing.T) (x, y, z predicates.QueryPredicate) {
	t.Helper()
	bld := &predicateBuilder{script: []byte{0}}
	col0 := bld.column(0)
	col1 := bld.column(1)
	return predicates.NewComparisonPredicate(col0,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))),
		predicates.NewComparisonPredicate(col1,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2))),
		predicates.NewComparisonPredicate(col0,
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)))
}

// Unmarked connectives are not normal-form variables; leaf predicates are.
func TestNormalFormVariable_MatchesTheConcreteTypes(t *testing.T) {
	t.Parallel()
	a, _, _ := normalFormTestLeaves(t)
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("nf_variable"), predicateSemanticsRowType())
	if err != nil {
		t.Fatalf("construct QOV: %v", err)
	}

	cases := []struct {
		pred       predicates.QueryPredicate
		isVariable bool
	}{
		// The three connectives — the only predicates with children.
		{predicates.NewAnd(a, a), false},
		{predicates.NewOr(a, a), false},
		{predicates.NewNot(a), false},

		// Everything else is a variable.
		{a, true},
		{predicates.NewConstantPredicate(predicates.TriTrue), true},
		{predicates.NewValuePredicate(root), true},
		{predicates.NewPlaceholder(values.NamedCorrelationIdentifier("ph"), root), true},
		{predicates.NewPredicateWithValueAndRanges(root, nil), true},
		{predicates.NewDatabaseObjectDependenciesPredicate(nil), true},
		{predicates.NewCompatibleTypeEvolutionPredicate(nil), true},
		{predicates.MustNewExistentialValuePredicate(root, predicates.Comparison{
			Type: predicates.ComparisonIsNotNull,
		}), true},
	}

	for _, tc := range cases {
		if got := normalFormVariable(tc.pred); got != tc.isVariable {
			t.Errorf("normalFormVariable(%T) = %v, want %v", tc.pred, got, tc.isVariable)
		}
		if hasChildren := len(tc.pred.Children()) > 0; hasChildren == tc.isVariable {
			t.Errorf("%T: Children() non-empty = %v but classified variable = %v — "+
				"the structural test and the child count disagree, which is exactly "+
				"when normalFormVariable stops matching Java's "+
				"isAtomic()||LeafQueryPredicate",
				tc.pred, hasChildren, tc.isVariable)
		}
		// normalFormVariableOrNot must agree with its own definition: a
		// variable, or a NOT over one. Pinned here because it is the function
		// isInNormalForm actually calls, and it is kept mode-independent to
		// match Java's static isNormalFormVariableOrNotPredicate (:494-504).
		wantOrNot := tc.isVariable
		if n, isNot := tc.pred.(*predicates.NotPredicate); isNot {
			wantOrNot = normalFormVariable(n.Child)
		}
		if got := normalFormVariableOrNot(tc.pred); got != wantOrNot {
			t.Errorf("normalFormVariableOrNot(%T) = %v, want %v", tc.pred, got, wantOrNot)
		}
	}

	// Population guard, stated against the ACTUAL population rather than a
	// round number. The eleven concrete QueryPredicate implementations in
	// pkg/.../cascades/predicates are: And, Or, Not, ComparisonPredicate,
	// ConstantPredicate, ValuePredicate, Placeholder,
	// PredicateWithValueAndRanges, DatabaseObjectDependenciesPredicate,
	// CompatibleTypeEvolutionPredicate and ExistentialValuePredicate — every
	// one of which is above.
	//
	// The floor was 10 against a list of 10 while the comment claimed "every
	// concrete implementation", which was a scope sentence exceeding its
	// coverage by exactly one type — in the test written to close a
	// scope-sentence finding. ExistentialValuePredicate was the missing one.
	const concretePredicateTypes = 11
	seen := map[reflect.Type]struct{}{}
	for _, tc := range cases {
		seen[reflect.TypeOf(tc.pred)] = struct{}{}
	}
	if len(seen) != concretePredicateTypes {
		t.Fatalf("exercised %d distinct predicate types, expected %d — a type was "+
			"added to or removed from the predicates package and this enumeration "+
			"no longer covers it", len(seen), concretePredicateTypes)
	}
}

func TestNormalFormAtomicConnectiveBarrier(t *testing.T) {
	t.Parallel()
	a, b, c := normalFormTestLeaves(t)
	fixed := predicates.WithAtomicity(predicates.NewOr(a, b), true)
	input := predicates.NewAnd(fixed, predicates.NewOr(c, predicates.NewNot(c)))
	normalized, changed := NormalizeDNF(input, 2)
	if !changed {
		t.Fatal("unfixed OR was not distributed")
	}
	disjunction, ok := normalized.(*predicates.OrPredicate)
	if !ok || len(disjunction.SubPredicates) != 2 {
		t.Fatalf("want two legs, got %s", normalized.Explain())
	}
	for _, term := range disjunction.SubPredicates {
		found := false
		for _, factor := range term.Children() {
			if factor == fixed {
				found = true
			}
		}
		if !found {
			t.Fatalf("fixed atomic factor lost: %s", term.Explain())
		}
	}
	if got := normalFormExpansionSize(input, false, normalFormDNF); got != 2 {
		t.Fatalf("expansion size = %d, want 2", got)
	}
	if !normalFormVariable(fixed) {
		t.Fatal("atomic connective must be a normal-form variable")
	}
	if got := normalFormSize(input, false, normalFormDNF); got != 4 {
		t.Fatalf("full cost size = %d, want 4", got)
	}
}
