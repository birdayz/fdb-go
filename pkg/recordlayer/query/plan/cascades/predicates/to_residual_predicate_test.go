package predicates

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The residual conversion has an arm per predicate type that cannot answer for a
// row, and every one of them is driven here rather than left to whatever the
// corpus happens to reach. Two arms are unreachable from any SQL the engine
// accepts today — the sargable and the placeholder, because no planner rule
// mints a PredicateWithValueAndRanges or leaves a Placeholder in a physical
// filter. An arm no corpus reaches is exactly the arm whose first real firing
// gets read as a finding instead of as an untested branch, which is how the
// EXISTS arm this file exists for shipped in the first place.
//
// The recursion arms ARE reachable: `NOT EXISTS` is
// NotPredicate(ExistentialValuePredicate) in an ordinary flat predicate list.

func mustResidual(t testing.TB, p QueryPredicate) QueryPredicate {
	t.Helper()
	got, err := ToResidualPredicate(p)
	if err != nil {
		t.Fatalf("ToResidualPredicate(%T): unexpected error %v", p, err)
	}
	return got
}

// TestToResidualPredicate_ExistentialBecomesNotNull is the defect this whole
// conversion was found by. An ExistentialValuePredicate names the existential
// quantifier's alias; once the subplan has been lowered to a FirstOrDefault no
// physical row carries that alias, so evaluating the EXISTS form as a row filter
// is UNKNOWN for every row and the filter silently drops the entire stream.
//
// Ports Java's ExistentialValuePredicate.toResidualPredicate()
// (ExistentialValuePredicate.java:107-109).
func TestToResidualPredicate_ExistentialBecomesNotNull(t *testing.T) {
	t.Parallel()
	exists := mustExistentialAlias(t, values.NamedCorrelationIdentifier("subq1"))

	cmp, ok := mustResidual(t, exists).(*ComparisonPredicate)
	if !ok {
		t.Fatalf("ToResidualPredicate(EXISTS) is not a *ComparisonPredicate — the EXISTS form " +
			"reaching a PredicatesFilter evaluates UNKNOWN per row and drops every row while " +
			"reporting success")
	}
	if cmp.Comparison.Type != ComparisonIsNotNull {
		t.Fatalf("residual comparison = %v, want ComparisonIsNotNull", cmp.Comparison.Type)
	}
	if cmp.Operand != exists.Value {
		t.Fatalf("residual operand = %v, want the predicate's own value %v — a re-derived operand "+
			"would not be the QOV the FirstOrDefault binds", cmp.Operand, exists.Value)
	}
}

// TestToResidualPredicate_ExistentialComparisonIsMinted pins that the residual's
// comparison is CONSTRUCTED, never read off the predicate. Java is unconditional
// (`new NullComparison(NOT_NULL)`), and Go must be too: nothing enforces the
// struct's comparison field — MustNewExistentialValuePredicate asserts only that
// the value is a QuantifiedObjectValue, and replace_values.go builds the struct
// literal directly with a translated comparison. Reusing the field would make
// the residual's meaning depend on a value no constructor guards.
func TestToResidualPredicate_ExistentialComparisonIsMinted(t *testing.T) {
	t.Parallel()
	qov := mustExistentialAlias(t, values.NamedCorrelationIdentifier("subq1")).Value
	// A predicate carrying the WRONG comparison, which the struct permits.
	wrong := &ExistentialValuePredicate{Value: qov, Comparison: Comparison{Type: ComparisonIsNull}}

	cmp, ok := mustResidual(t, wrong).(*ComparisonPredicate)
	if !ok {
		t.Fatalf("residual is not a *ComparisonPredicate")
	}
	if cmp.Comparison.Type != ComparisonIsNotNull {
		t.Fatalf("residual comparison = %v, want ComparisonIsNotNull minted unconditionally; "+
			"reading the predicate's own field makes EXISTS mean whatever that field happens to say",
			cmp.Comparison.Type)
	}
}

// TestToResidualPredicate_NestedExistentialIsReached pins the recursion. A
// NOT EXISTS is NotPredicate(ExistentialValuePredicate), so an implementation
// that converted only a top-level existential would leave every negated
// semi-join filtering on the un-evaluable form.
func TestToResidualPredicate_NestedExistentialIsReached(t *testing.T) {
	t.Parallel()
	exists := mustExistentialAlias(t, values.NamedCorrelationIdentifier("subq1"))
	other := NewComparisonPredicate(
		&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
		Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
	)

	for _, tc := range []struct {
		name  string
		build func(QueryPredicate) QueryPredicate
		find  func(QueryPredicate) QueryPredicate
	}{
		{
			name:  "not",
			build: func(p QueryPredicate) QueryPredicate { return NewNot(p) },
			find:  func(p QueryPredicate) QueryPredicate { return p.(*NotPredicate).Child },
		},
		{
			name:  "and",
			build: func(p QueryPredicate) QueryPredicate { return NewAnd(other, p) },
			find:  func(p QueryPredicate) QueryPredicate { return p.(*AndPredicate).SubPredicates[1] },
		},
		{
			name:  "or",
			build: func(p QueryPredicate) QueryPredicate { return NewOr(other, p) },
			find:  func(p QueryPredicate) QueryPredicate { return p.(*OrPredicate).SubPredicates[1] },
		},
		{
			name:  "and_of_not",
			build: func(p QueryPredicate) QueryPredicate { return NewAnd(other, NewNot(p)) },
			find: func(p QueryPredicate) QueryPredicate {
				return p.(*AndPredicate).SubPredicates[1].(*NotPredicate).Child
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := tc.find(mustResidual(t, tc.build(exists)))
			if _, stillExists := inner.(*ExistentialValuePredicate); stillExists {
				t.Fatalf("%s: the nested EXISTS survived residualisation — a filter carrying it "+
					"drops every row", tc.name)
			}
			if _, ok := inner.(*ComparisonPredicate); !ok {
				t.Fatalf("%s: nested residual = %T, want *ComparisonPredicate", tc.name, inner)
			}
		})
	}
}

// TestToResidualPredicate_PlaceholderIsConverted drives the arm Java gets for
// free by inheritance — `Placeholder extends PredicateWithValueAndRanges`
// (Placeholder.java:48) — and Go must name explicitly, because its Placeholder
// is a standalone struct. Its Eval is an unconditional TriUnknown
// (placeholder.go:85), so a Placeholder reaching a filter fails exactly as the
// EXISTS form does. An unconstrained placeholder is Java's tautology case.
func TestToResidualPredicate_PlaceholderIsConverted(t *testing.T) {
	t.Parallel()
	operand := &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
	ph := NewPlaceholder(values.NamedCorrelationIdentifier("p0"), operand)

	if got, err := ph.Eval(nil); err != nil || got != TriUnknown {
		t.Fatalf("Placeholder Eval = (%v, %v), want (TriUnknown, nil) — the premise of this "+
			"conversion is that a placeholder cannot answer for a row", got, err)
	}

	residual := mustResidual(t, ph)
	if _, still := residual.(*Placeholder); still {
		t.Fatal("Placeholder survived residualisation — it would drop every row in a filter")
	}
	// NewPlaceholder starts with an EMPTY range: unconstrained, which is Java's
	// tautology case for a placeholder.
	if c, ok := residual.(*ConstantPredicate); !ok || c.Value != TriTrue {
		t.Fatalf("unconstrained placeholder residual = %#v, want ConstantPredicate(TRUE)", residual)
	}
}

// TestToResidualPredicate_SargableBecomesDNF drives the other arm nothing in the
// engine can currently reach. PredicateWithValueAndRanges.Eval is an
// unconditional TriUnknown, so a sargable reaching a PredicatesFilter fails the
// same way. Java converts it to the DNF its ranges denote
// (PredicateWithValueAndRanges.java:483-492).
func TestToResidualPredicate_SargableBecomesDNF(t *testing.T) {
	t.Parallel()
	operand := &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
	lit := func(n int64) values.Value { return &values.ConstantValue{Value: n, Typ: values.NotNullLong} }
	rangeA := NewRangeConstraints([]Comparison{
		{Type: ComparisonGreaterThan, Operand: lit(1)},
		{Type: ComparisonLessThan, Operand: lit(9)},
	}, nil)
	rangeB := NewRangeConstraints([]Comparison{{Type: ComparisonEquals, Operand: lit(42)}}, nil)
	sargable := NewPredicateWithValueAndRanges(operand, []*RangeConstraints{rangeA, rangeB})

	if got, err := sargable.Eval(nil); err != nil || got != TriUnknown {
		t.Fatalf("sargable Eval = (%v, %v), want (TriUnknown, nil)", got, err)
	}

	or, ok := mustResidual(t, sargable).(*OrPredicate)
	if !ok {
		t.Fatal("ToResidualPredicate(sargable) is not an *OrPredicate (one disjunct per range)")
	}
	if len(or.SubPredicates) != 2 {
		t.Fatalf("disjuncts = %d, want 2 (one per range)", len(or.SubPredicates))
	}
	// Range A has two comparisons → an AND node. Range B has one → Java unwraps
	// the singleton (AndPredicate.java:201-203) rather than wrapping it, and the
	// difference is not cosmetic: a singleton wrapper changes the conjunct count
	// the cost model reads.
	and, ok := or.SubPredicates[0].(*AndPredicate)
	if !ok {
		t.Fatalf("disjunct 0 = %T, want *AndPredicate for a two-comparison range", or.SubPredicates[0])
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("disjunct 0 conjuncts = %d, want 2", len(and.SubPredicates))
	}
	single, ok := or.SubPredicates[1].(*ComparisonPredicate)
	if !ok {
		t.Fatalf("disjunct 1 = %T, want a bare *ComparisonPredicate — a one-comparison range "+
			"must not be wrapped in a singleton AND", or.SubPredicates[1])
	}
	if single.Operand != operand {
		t.Fatalf("disjunct 1 re-attached to %v, want the sargable's own value", single.Operand)
	}
}

// TestToResidualPredicate_RangelessSargableIsAnError pins the case Java asserts
// on. `OrPredicate.of` opens with `Verify.verify(!disjuncts.isEmpty())`
// (OrPredicate.java:439), so a rangeless sargable throws there. Go must not
// return `OR()` instead: an OrPredicate with no disjuncts evaluates to FALSE,
// which is the silent row-dropping defect this whole file exists to prevent.
func TestToResidualPredicate_RangelessSargableIsAnError(t *testing.T) {
	t.Parallel()
	sargable := NewPredicateWithValueAndRanges(
		&values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}, nil)

	got, err := ToResidualPredicate(sargable)
	if err == nil {
		t.Fatalf("a rangeless sargable residualised to %#v; want an error — returning a "+
			"degenerate predicate here is a silent empty result", got)
	}
	if got != nil {
		t.Fatalf("error path returned a predicate %#v; it must return none", got)
	}
	var nre *NonResidualPredicateError
	if !errors.As(err, &nre) {
		t.Fatalf("error = %v (%T), want a *NonResidualPredicateError", err, err)
	}
}

// TestToResidualPredicate_ErrorPropagatesFromNesting pins that a nested failure
// aborts the whole conversion rather than yielding a partly-converted tree.
func TestToResidualPredicate_ErrorPropagatesFromNesting(t *testing.T) {
	t.Parallel()
	bad := NewPredicateWithValueAndRanges(
		&values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}, nil)
	for _, tc := range []struct {
		name string
		in   QueryPredicate
	}{
		{"and", NewAnd(NewConstantPredicate(TriTrue), bad)},
		{"or", NewOr(NewConstantPredicate(TriFalse), bad)},
		{"not", NewNot(bad)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ToResidualPredicate(tc.in)
			if err == nil || got != nil {
				t.Fatalf("%s: got (%#v, %v), want (nil, error)", tc.name, got, err)
			}
		})
	}
}

// TestToResidualPredicate_LeavesAndUnchangedTreesAreIdentical pins Java's
// default: a leaf answers for itself, and a tree needing no rewrite is returned
// as itself so callers can compare identities.
func TestToResidualPredicate_LeavesAndUnchangedTreesAreIdentical(t *testing.T) {
	t.Parallel()
	leaf := NewComparisonPredicate(
		&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
		Comparison{Type: ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
	)
	for _, tc := range []struct {
		name string
		in   QueryPredicate
	}{
		{"leaf", leaf},
		{"and_of_leaves", NewAnd(leaf, NewConstantPredicate(TriTrue))},
		{"not_of_leaf", NewNot(leaf)},
		{"nil", nil},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mustResidual(t, tc.in); got != tc.in {
				t.Fatalf("ToResidualPredicate(%s) rebuilt a tree that needed no rewrite: %v -> %v",
					tc.name, tc.in, got)
			}
		})
	}
}

// TestFindStructuralPredicate_NamesEveryUnevaluableType is the invariant's own
// pin. The physical filter constructor rejects on this predicate, so an omission
// here reopens the silent-drop hole for that type — and each of the three has an
// unconditional non-answer as its Eval, which is the property that makes it
// unevaluable.
func TestFindStructuralPredicate_NamesEveryUnevaluableType(t *testing.T) {
	t.Parallel()
	operand := &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
	structural := map[string]QueryPredicate{
		"existential": mustExistentialAlias(t, values.NamedCorrelationIdentifier("subq1")),
		"sargable":    NewPredicateWithValueAndRanges(operand, nil),
		"placeholder": NewPlaceholder(values.NamedCorrelationIdentifier("p0"), operand),
	}
	for name, p := range structural {
		p := p
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if v, err := p.Eval(nil); err == nil && v == TriTrue {
				t.Fatalf("%s answered TRUE for a row; it would not be structural", name)
			}
			if _, ok := FindStructuralPredicate(p); !ok {
				t.Fatalf("%s is not reported structural — a filter would accept it and drop "+
					"every row while reporting success", name)
			}
			// and nested, since a filter carries whole trees
			if _, ok := FindStructuralPredicate(NewAnd(NewConstantPredicate(TriTrue), NewNot(p))); !ok {
				t.Fatalf("%s nested under AND/NOT is not reported structural", name)
			}
		})
	}

	residual := NewComparisonPredicate(operand, Comparison{Type: ComparisonIsNotNull})
	if bad, ok := FindStructuralPredicate(NewAnd(residual, NewNot(residual))); ok {
		t.Fatalf("a fully residual tree was reported structural (%#v) — the invariant would "+
			"reject legitimate filters", bad)
	}
}
