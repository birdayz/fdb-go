package predicates

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// NonResidualPredicateError reports a predicate that cannot be converted into
// an executable filter. Java expresses the same condition as a
// `Verify.verify(!disjuncts.isEmpty())` inside `OrPredicate.of`
// (OrPredicate.java:439) — a thrown assertion, not a value — so the Go mapping
// is an error rather than a degenerate predicate. It matters that this is loud:
// the whole point of the conversion is that a predicate which cannot answer for
// a row must never reach a filter, and returning `OR()` here would hand the
// caller a predicate whose Eval is an unconditional FALSE.
type NonResidualPredicateError struct {
	Detail string
}

func (e *NonResidualPredicateError) Error() string {
	return "predicate cannot be residualised: " + e.Detail
}

// ToResidualPredicate returns a predicate equivalent to p that can be evaluated
// as a RESIDUAL — a filter applied to rows that have already been produced —
// rather than as an index search argument or an instruction to the matching
// machinery.
//
// Ports Java's `QueryPredicate.toResidualPredicate()`: the interface default
// (`QueryPredicate.java:252-260`) and its overrides on the existential predicate
// (`ExistentialValuePredicate.java:107-109`) and the sargable
// (`PredicateWithValueAndRanges.java:483-492`). Java spells it as a default
// method; Go has no interface defaults, so it is a package-level function over
// the interface — the shape `IsTautology` and `ReplaceValues` already use.
//
// Three predicate types cannot answer for a row, and all three have an
// unconditional non-answer as their Eval: `ExistentialValuePredicate` names an
// alias no physical row carries once its subplan is a FirstOrDefault,
// `PredicateWithValueAndRanges` is an index search argument
// (`Eval` → TriUnknown), and `Placeholder` is a matching-time marker
// (`Eval` → TriUnknown). Leaving any of them in a filter is not a slower plan,
// it is a wrong one: the filter drops the entire stream while reporting
// success.
//
// Pointer-stable: a predicate needing no rewrite is returned as itself.
func ToResidualPredicate(p QueryPredicate) (QueryPredicate, error) {
	if p == nil {
		return nil, nil
	}
	switch pred := p.(type) {
	case *ExistentialValuePredicate:
		// Java: `new ValuePredicate(getValue(), new NullComparison(NOT_NULL))` —
		// unconditional, never a read of the predicate's own comparison. Go mints
		// it the same way. The struct's comparison field is NOT reused: nothing
		// enforces it is NOT_NULL (MustNewExistentialValuePredicate asserts only
		// that the value is a QuantifiedObjectValue, and replace_values.go builds
		// the struct literal directly with a translated comparison), so reusing it
		// would make the residual's meaning depend on a field no constructor
		// guards.
		return NewComparisonPredicate(pred.Value, Comparison{Type: ComparisonIsNotNull}), nil

	case *Placeholder:
		// Java's `Placeholder extends PredicateWithValueAndRanges`
		// (Placeholder.java:48), so it inherits the sargable conversion below.
		// Go's Placeholder is a standalone struct carrying a single
		// ComparisonRange rather than a list of RangeConstraints, so the
		// inheritance is spelled out here instead. A nil or empty range is
		// "unconstrained", which is Java's tautology case for a placeholder
		// (AndPredicate.java:190-195 retains placeholders precisely because their
		// tautology means "no range constraint") and residualises to TRUE.
		if pred.CompRange == nil || pred.CompRange.IsEmpty() {
			return NewConstantPredicate(TriTrue), nil
		}
		// ComparisonRange hands back pointers where RangeConstraints hands back
		// values; the predicate owns a copy either way.
		cmps := pred.CompRange.GetComparisons()
		flat := make([]Comparison, 0, len(cmps))
		for _, cmp := range cmps {
			if cmp != nil {
				flat = append(flat, *cmp)
			}
		}
		return residualConjunction(pred.Value, flat), nil

	case *PredicateWithValueAndRanges:
		// A sargable is an index search argument, not a filter. Java turns it into
		// the DNF its ranges denote — OR over ranges of AND over that range's
		// comparisons, each comparison re-attached to the same value
		// (PredicateWithValueAndRanges.java:483-492, where Java's
		// `getValue().withComparison(c)` mints the ValuePredicate that Go spells
		// as a ComparisonPredicate).
		ranges := pred.GetRanges()
		if len(ranges) == 0 {
			// Java reaches `OrPredicate.of([])` here and trips its
			// `Verify.verify(!disjuncts.isEmpty())`. A rangeless sargable denotes
			// no constraint at all and there is no honest predicate to return:
			// TRUE would widen the query and FALSE would empty it.
			return nil, &NonResidualPredicateError{
				Detail: "sargable carries no ranges, so it denotes no comparison to filter on",
			}
		}
		dnf := make([]QueryPredicate, 0, len(ranges))
		for _, rng := range ranges {
			dnf = append(dnf, residualConjunction(pred.GetValue(), rng.GetComparisons()))
		}
		return residualDisjunction(dnf), nil

	case *AndPredicate:
		subs, changed, err := residualChildren(pred.SubPredicates)
		if err != nil {
			return nil, err
		}
		if !changed {
			return p, nil
		}
		return residualConjunctionOf(subs), nil

	case *OrPredicate:
		subs, changed, err := residualChildren(pred.SubPredicates)
		if err != nil {
			return nil, err
		}
		if !changed {
			return p, nil
		}
		return residualDisjunction(subs), nil

	case *NotPredicate:
		newChild, err := ToResidualPredicate(pred.Child)
		if err != nil {
			return nil, err
		}
		if newChild == pred.Child {
			return p, nil
		}
		return NewNot(newChild), nil

	default:
		// Leaves and predicates that are already residual answer for themselves,
		// matching Java's default for an empty child iterable.
		return p, nil
	}
}

// residualChildren residualises each element, reporting whether any changed so
// the caller can preserve pointer identity.
func residualChildren(subs []QueryPredicate) ([]QueryPredicate, bool, error) {
	out := make([]QueryPredicate, len(subs))
	changed := false
	for i, sub := range subs {
		converted, err := ToResidualPredicate(sub)
		if err != nil {
			return nil, false, fmt.Errorf("conjunct/disjunct %d: %w", i, err)
		}
		out[i] = converted
		changed = changed || out[i] != sub
	}
	return out, changed, nil
}

// residualConjunction attaches every comparison in cmps to value and ANDs the
// results — Java's `range.getComparisons().stream().map(c -> getValue().withComparison(c))`
// fed to `AndPredicate.and`.
func residualConjunction(value values.Value, cmps []Comparison) QueryPredicate {
	conjuncts := make([]QueryPredicate, 0, len(cmps))
	for _, cmp := range cmps {
		conjuncts = append(conjuncts, NewComparisonPredicate(value, cmp))
	}
	return residualConjunctionOf(conjuncts)
}

// residualConjunctionOf ports `AndPredicate.and` (AndPredicate.java:189-205):
// an empty conjunction is TRUE, a singleton is itself, and only two or more
// conjuncts build a node. Go's NewAnd is a bare struct literal that does none of
// this, and the difference is not cosmetic — an empty *AndPredicate is a silent
// TRUE only by accident of Eval, and a singleton wrapper changes the conjunct
// count the cost model reads (predicates.go CountConjuncts).
//
// Java also filters tautologies here. That is deliberately NOT done: Java's
// filter carries an explicit placeholder exemption because dropping a
// no-range placeholder would break index matching, and this function is reached
// with predicates already residualised — where a tautology is a legitimate
// answer (an unconstrained placeholder) rather than noise to remove.
func residualConjunctionOf(conjuncts []QueryPredicate) QueryPredicate {
	switch len(conjuncts) {
	case 0:
		return NewConstantPredicate(TriTrue)
	case 1:
		return conjuncts[0]
	default:
		return NewAnd(conjuncts...)
	}
}

// residualDisjunction ports `OrPredicate.of` (OrPredicate.java:438-444): a
// singleton is itself and two or more build a node. The empty case is Java's
// `Verify.verify` and is unreachable here — every caller either checked, or is
// rebuilding a disjunction that already had children.
func residualDisjunction(disjuncts []QueryPredicate) QueryPredicate {
	switch len(disjuncts) {
	case 0:
		// An OrPredicate with no disjuncts evaluates to FALSE, which would drop
		// every row — the exact defect this file exists to prevent. Reaching here
		// means a caller built an empty disjunction; TRUE is the safe direction
		// (it filters nothing) and the structural-predicate invariant at the
		// physical filter constructor is what actually catches the mistake.
		return NewConstantPredicate(TriTrue)
	case 1:
		return disjuncts[0]
	default:
		return NewOr(disjuncts...)
	}
}

// ToResidualPredicates converts a whole predicate list, the shape every
// filter-building rule needs. Java writes this as
// `predicates.stream().map(QueryPredicate::toResidualPredicate)` at each of its
// three filter sites (ImplementFilterRule.java:90,
// ImplementSimpleSelectRule.java:169, ImplementNestedLoopJoinRule.java:157);
// having one helper keeps Go from growing three spellings of it.
//
// It is atomic: a failure returns no list rather than a partially converted one.
func ToResidualPredicates(preds []QueryPredicate) ([]QueryPredicate, error) {
	if preds == nil {
		return nil, nil
	}
	out := make([]QueryPredicate, len(preds))
	for i, p := range preds {
		converted, err := ToResidualPredicate(p)
		if err != nil {
			return nil, fmt.Errorf("predicate %d: %w", i, err)
		}
		out[i] = converted
	}
	return out, nil
}
