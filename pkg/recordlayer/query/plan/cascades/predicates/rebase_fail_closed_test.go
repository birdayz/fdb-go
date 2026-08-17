package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRebaseReportsFailureRatherThanReturningNil pins the direction the deleted
// error-less wrapper got wrong.
//
// That wrapper returned nil when exact Value reconstruction failed and its own
// doc called that failing closed. Nil is not closed at any caller this package
// had: five appended it into a predicate LIST, where a nil element is a
// predicate that is simply not there, and the sixth used it as the
// NO-JOIN-PREDICATE sentinel of a correlated EXISTS — so a failed rebase
// produced a subquery matching every outer row, with no error and a plausible
// row count.
//
// The wrapper is gone. What has to stay true is that the surviving spelling
// distinguishes its three outcomes, because the old one collapsed two of them
// into the same nil: NOTHING TO DO returns the predicate unchanged, a NIL INPUT
// returns nil, and a FAILURE returns an error. A refactor that reintroduces a
// swallow makes the first case look like the third, and nothing else in this
// package would notice.
func TestRebaseReportsFailureRatherThanReturningNil(t *testing.T) {
	t.Parallel()

	old := values.NamedCorrelationIdentifier("OLD")
	other := values.NamedCorrelationIdentifier("OTHER")
	target := values.NamedCorrelationIdentifier("NEW")

	t.Run("an unmapped alias is unchanged, not dropped", func(t *testing.T) {
		t.Parallel()
		p := &ComparisonPredicate{
			Operand: mustQOV(t, old),
			Comparison: Comparison{
				Type:    ComparisonEquals,
				Operand: &values.ConstantValue{Value: int64(5)},
			},
		}
		// An alias map naming a correlation this predicate does not mention:
		// there is nothing to rewrite, which is NOT a failure.
		got, err := RebasePredicateChecked(p, mustAliasMap(t, values.AliasPair{Source: other, Target: target}))
		if err != nil {
			t.Fatalf("an alias map that does not touch this predicate must not fail: %v", err)
		}
		if got == nil {
			t.Fatal("a predicate with nothing to rebase came back nil — that is the swallow the " +
				"deleted wrapper performed, and every caller reads nil as an absent predicate")
		}
		cp, ok := got.(*ComparisonPredicate)
		if !ok {
			t.Fatalf("rebased predicate is %T, want *ComparisonPredicate", got)
		}
		qov, ok := values.AsQuantifiedObjectValue(cp.Operand)
		if !ok {
			t.Fatalf("operand is %T, want a QuantifiedObjectValue", cp.Operand)
		}
		if qov.Correlation() != old {
			t.Fatalf("unmapped correlation became %v, want it left as %v", qov.Correlation(), old)
		}
	})

	t.Run("a nil predicate stays nil without an error", func(t *testing.T) {
		t.Parallel()
		got, err := RebasePredicateChecked(nil, nil)
		if err != nil || got != nil {
			t.Fatalf("RebasePredicateChecked(nil, nil) = (%v, %v), want (nil, nil) — a nil INPUT is "+
				"the one place a nil result is correct, and conflating it with the failure case is "+
				"what made the old wrapper look reasonable", got, err)
		}
	})
}
