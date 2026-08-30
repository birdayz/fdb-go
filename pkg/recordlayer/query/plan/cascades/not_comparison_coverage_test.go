package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestNotComparisonRewrite_CoverageIsJavasTable drives EVERY comparison type a
// NOT can sit over through the full Simplify pass and pins which ones the
// NOT-pushdown actually reaches.
//
// It exists because the claim it measures was recorded in THREE places and was
// false in two of them: the rule's doc offered `NOT(x IS NULL)` -> `x IS NOT
// NULL` as an example of what it does, ComparisonType.Negate's own doc offered
// the same, directly above the switch that refuses it, and a test's title line
// said it while the test asserted the opposite. Nothing failed, because a
// comment cannot fail. This is the thing that can.
//
// The NOT-rewritten list is written first and deliberately: it is the surprising
// half, and it is the half a reader is most likely to assume away. IS NULL and
// IS NOT NULL are each other's exact negation, and Java still refuses them
// (`invertComparisonType` opens `if (type.isUnary()) return null;`).
//
// The table is EXHAUSTIVE over the comparison types a NOT-over-comparison can
// carry, so a type added to the enum with a Negate arm and no entry here shows
// up as an unhandled case rather than as silent non-coverage.
func TestNotComparisonRewrite_CoverageIsJavasTable(t *testing.T) {
	t.Parallel()

	rowType := predicateSemanticsRowType()
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("not_coverage"), rowType)
	if err != nil {
		t.Fatalf("construct QOV: %v", err)
	}
	column, err := values.ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolve column: %v", err)
	}

	cases := []struct {
		in predicates.ComparisonType
		// want is the type the NOT collapses to, or -1 when the rule declines
		// and the NotPredicate must survive.
		want predicates.ComparisonType
	}{
		// DECLINED — the surprising half, listed first.
		{predicates.ComparisonIsNull, -1},
		{predicates.ComparisonIsNotNull, -1},
		{predicates.ComparisonNotEquals, -1},
		{predicates.ComparisonIsDistinctFrom, -1},
		{predicates.ComparisonNotDistinctFrom, -1},
		{predicates.ComparisonIn, -1},
		{predicates.ComparisonStartsWith, -1},
		{predicates.ComparisonLike, -1},

		// REWRITTEN — Java's five.
		{predicates.ComparisonEquals, predicates.ComparisonNotEquals},
		{predicates.ComparisonLessThan, predicates.ComparisonGreaterThanEq},
		{predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThan},
		{predicates.ComparisonGreaterThan, predicates.ComparisonLessThanOrEq},
		{predicates.ComparisonGreaterThanEq, predicates.ComparisonLessThan},
	}

	rewritten := 0
	declined := 0
	for _, tc := range cases {
		var inner predicates.QueryPredicate
		if tc.in.IsUnary() {
			inner = predicates.NewComparisonPredicate(column, predicates.Comparison{Type: tc.in})
		} else if tc.in == predicates.ComparisonIn {
			inner = predicates.NewComparisonPredicate(column, predicates.Comparison{
				Type: tc.in, Operand: values.LiteralValue([]any{int64(1), int64(2)}),
			})
		} else {
			inner = predicates.NewComparisonPredicate(column,
				predicates.NewLiteralComparison(tc.in, int64(5)))
		}

		out, err := Simplify(predicates.NewNot(inner), DefaultSimplifyRules())
		if err != nil {
			t.Fatalf("%s: Simplify: %v", tc.in.Symbol(), err)
		}

		if tc.want < 0 {
			declined++
			if _, stillNot := out.(*predicates.NotPredicate); !stillNot {
				t.Fatalf("NOT (x %s ...) was rewritten to %s — the rule now reaches a type "+
					"Java's invertComparisonType refuses, so Go's canonical predicate shape "+
					"has diverged from Java's for this query",
					tc.in.Symbol(), out.Explain())
			}
			continue
		}

		rewritten++
		cp, isComparison := out.(*predicates.ComparisonPredicate)
		if !isComparison {
			t.Fatalf("NOT (x %s 5) did not collapse to a comparison: %s", tc.in.Symbol(), out.Explain())
		}
		if cp.Comparison.Type != tc.want {
			t.Fatalf("NOT (x %s 5) collapsed to %s, want %s",
				tc.in.Symbol(), cp.Comparison.Type.Symbol(), tc.want.Symbol())
		}
	}

	// Both populations guarded. A zero on either side means this test stopped
	// measuring the distinction it exists to draw — all-declined would pass
	// vacuously against a rule that never fires, and all-rewritten against a
	// table with no declines left.
	if rewritten != 5 {
		t.Fatalf("expected exactly Java's 5 invertible types, saw %d", rewritten)
	}
	if declined == 0 {
		t.Fatal("no declined case ran — the decline half of the table is untested")
	}
}
