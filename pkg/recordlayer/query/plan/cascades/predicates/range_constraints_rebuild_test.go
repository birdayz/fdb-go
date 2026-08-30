package predicates

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// rebuildProbeTransform rewrites every literal, so `changed` is true and the
// rebuild path actually runs. A transform that returns its input leaves both
// functions on their pointer-stable early exit, where nothing is rebuilt and
// nothing can be dropped — the tests below would pass without exercising
// anything.
func rebuildProbeTransform(values.Value) values.Value {
	return values.LiteralValue(int64(99))
}

// TestTransformRangeConstraints_RefusesToDropAConjunct pins that rebuilding a
// RangeConstraints after a value transform cannot silently return a WEAKER set
// than it was given.
//
// Both rebuild loops called builder.AddComparisonMaybe and discarded its bool.
// That bool is false exactly when canBeUsedInScanPrefix rejects the comparison,
// and the builder then holds nothing for it — so the conjunct vanished. Measured
// before the fix: a constraint set of {> 1, != 5} came back holding only the
// `>`, with a NIL ERROR. A filter built from that returns every row with x == 5
// that the original excluded.
//
// It is unreachable from production. The only construction path is the builder,
// which applies the same gate on the way in, and transformComparisonChecked
// rewrites Operand and QueryVector while leaving Type alone, so anything that
// entered can re-enter. Tests can construct one directly via NewRangeConstraints,
// which is how this drives the path — and why the drop went unnoticed: no corpus
// reading can reach it.
func TestTransformRangeConstraints_RefusesToDropAConjunct(t *testing.T) {
	t.Parallel()

	// NOT_EQUALS is rejected by canBeUsedInScanPrefix, in Go and in Java alike.
	notEquals := NewLiteralComparison(ComparisonNotEquals, int64(5))
	greater := NewLiteralComparison(ComparisonGreaterThan, int64(1))

	t.Run("checked variant reports the drop", func(t *testing.T) {
		t.Parallel()
		rc := NewRangeConstraints([]Comparison{greater, notEquals}, nil)
		out, err := transformRangeConstraintsChecked(rc,
			func(v values.Value) (values.Value, error) { return rebuildProbeTransform(v), nil })
		if err == nil {
			t.Fatalf("rebuilding dropped a conjunct and returned no error: %d comparison(s) "+
				"in, %d out. A filter built from the result returns rows the original excluded.",
				len(rc.GetComparisons()), len(out.GetComparisons()))
		}
		if !strings.Contains(err.Error(), "silently weaken") {
			t.Errorf("error = %v, want it to name the weakening", err)
		}
	})

	t.Run("unchecked variant preserves the original", func(t *testing.T) {
		t.Parallel()
		rc := NewRangeConstraints([]Comparison{greater, notEquals}, nil)
		out, changed := transformRangeConstraints(rc, rebuildProbeTransform)
		if changed {
			t.Error("the unchecked rebuild reported a change while unable to carry every " +
				"conjunct; it has no error channel, so it must report no change instead")
		}
		if out != rc {
			t.Fatal("the unchecked rebuild must return the ORIGINAL constraints when it " +
				"cannot rebuild losslessly — leaving a value un-rebased surfaces downstream " +
				"as a loud unbaked-ref failure, while a dropped conjunct is silent")
		}
		if got := len(out.GetComparisons()); got != 2 {
			t.Errorf("preserved constraints hold %d comparison(s), want 2", got)
		}
	})
}

// TestTransformRangeConstraints_RebuildsWhenEveryConjunctSurvives is the control
// for the pair above. Without it, both would pass if the rebuild refused
// everything — "no drop" is trivially true when nothing is ever rebuilt.
func TestTransformRangeConstraints_RebuildsWhenEveryConjunctSurvives(t *testing.T) {
	t.Parallel()

	greater := NewLiteralComparison(ComparisonGreaterThan, int64(1))
	less := NewLiteralComparison(ComparisonLessThan, int64(9))
	rc := NewRangeConstraints([]Comparison{greater, less}, nil)

	out, err := transformRangeConstraintsChecked(rc,
		func(v values.Value) (values.Value, error) { return rebuildProbeTransform(v), nil })
	if err != nil {
		t.Fatalf("a set whose conjuncts all bound a scan prefix must rebuild, got %v", err)
	}
	if got := len(out.GetComparisons()); got != 2 {
		t.Errorf("rebuilt set holds %d comparison(s), want 2", got)
	}
	if out == rc {
		t.Error("the rebuild returned the input unchanged; the transform rewrites every " +
			"literal, so this path must produce a new set — otherwise the drop tests above " +
			"never reach the rebuild either")
	}

	unchecked, changed := transformRangeConstraints(rc, rebuildProbeTransform)
	if !changed || unchecked == rc {
		t.Error("the unchecked variant must also rebuild when every conjunct survives")
	}
}
