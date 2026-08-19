package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// RewriteOuterJoinRule is registered in TWO phases on purpose and re-explores
// the same Reference. Every firing mints a fresh UniqueCorrelationIdentifier, so
// a rewritten form its idempotency guard cannot RECOGNIZE is a structurally
// distinct new member on every pass — unbounded memo growth for exactly the
// shape the guard missed.
//
// The guard used to test one shape: an INNER select with the ORIGINAL arity
// carrying a top-level null-on-empty quantifier. The BOXED rewrite emits
// neither of those — it wraps the outer pair into one box quantifier, so the
// arity is 1+E and the null-on-empty sits INSIDE the box — so the guard could
// never fire on it.
//
// A corpus run reaches only the arms the corpus happens to produce, and the
// boxed arm was reachable for the first time in the change that introduced it.
// So every arm is driven here from explicit state rather than read off a
// whole-suite run.
func TestRewriteOuterJoin_IdempotencyRecognizesBothRewrittenForms(t *testing.T) {
	t.Parallel()

	scanRef := func(t *testing.T, name string) *expressions.Reference {
		t.Helper()
		rowType := values.NewRecordType(name, false, []values.Field{{
			Name: "ID", FieldType: values.NotNullLong, Ordinal: 0,
		}})
		scan, err := expressions.NewFullUnorderedScanExpression([]string{name}, rowType)
		if err != nil {
			t.Fatalf("logical %s scan: %v", name, err)
		}
		ref := expressions.InitialOf(scan)
		physical, err := plans.NewRecordQueryScanPlan([]string{name}, rowType, false)
		if err != nil {
			t.Fatalf("physical %s scan: %v", name, err)
		}
		ref.InsertFinal(physical)
		return ref
	}

	// sel builds a select over the given quantifiers, taking its result value
	// from the first so the expression is well-formed.
	sel := func(t *testing.T, jt expressions.JoinType, quants ...expressions.Quantifier) *expressions.SelectExpression {
		t.Helper()
		result, err := quants[0].RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("flowed object value: %v", err)
		}
		out, err := expressions.NewSelectExpressionWithJoinType(result, quants, nil, nil, jt)
		if err != nil {
			t.Fatalf("build select: %v", err)
		}
		return out
	}

	plainQ := func(t *testing.T, alias, table string) expressions.Quantifier {
		return expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(alias), scanRef(t, table))
	}
	nullOnEmptyQ := func(t *testing.T, alias, table string) expressions.Quantifier {
		return expressions.NamedForEachNullOnEmptyQuantifier(
			values.NamedCorrelationIdentifier(alias), scanRef(t, table))
	}
	// boxOver wraps quants into ONE ForEach quantifier over a select — the shape
	// the boxed rewrite prepends.
	boxOver := func(t *testing.T, quants ...expressions.Quantifier) expressions.Quantifier {
		t.Helper()
		return expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("BOX"),
			expressions.InitialOf(sel(t, expressions.JoinInner, quants...)))
	}

	for _, tc := range []struct {
		name             string
		build            func(t *testing.T) *expressions.SelectExpression
		originalArity    int
		existentialCount int
		want             bool
		why              string
	}{
		{
			name: "FLAT rewrite is recognized",
			build: func(t *testing.T) *expressions.SelectExpression {
				return sel(t, expressions.JoinInner, plainQ(t, "L", "LEFT"), nullOnEmptyQ(t, "R", "RIGHT"))
			},
			originalArity: 2, existentialCount: 0, want: true,
			why: "the no-existential rewrite keeps the original arity and puts the null-on-empty at top level",
		},
		{
			name: "BOXED rewrite is recognized — the arm the old guard could not see",
			build: func(t *testing.T) *expressions.SelectExpression {
				box := boxOver(t, plainQ(t, "L", "LEFT"), nullOnEmptyQ(t, "R", "RIGHT"))
				return sel(t, expressions.JoinInner, box, plainQ(t, "E", "EXQ"))
			},
			originalArity: 3, existentialCount: 1, want: true,
			why: "arity is 1+E and the null-on-empty is one level down, inside the box",
		},
		{
			name: "a NON-INNER select is never the rewritten form",
			build: func(t *testing.T) *expressions.SelectExpression {
				return sel(t, expressions.JoinLeftOuter, plainQ(t, "L", "LEFT"), nullOnEmptyQ(t, "R", "RIGHT"))
			},
			originalArity: 2, existentialCount: 0, want: false,
			why: "the rewrite always emits INNER; a LEFT-OUTER member is the ORIGINAL, and matching it would decline the rewrite entirely",
		},
		{
			name: "right arity but NO null-on-empty anywhere",
			build: func(t *testing.T) *expressions.SelectExpression {
				return sel(t, expressions.JoinInner, plainQ(t, "L", "LEFT"), plainQ(t, "R", "RIGHT"))
			},
			originalArity: 2, existentialCount: 0, want: false,
			why: "an ordinary INNER join is not this rule's output",
		},
		{
			name: "BOXED arity but the box carries no null-on-empty",
			build: func(t *testing.T) *expressions.SelectExpression {
				box := boxOver(t, plainQ(t, "L", "LEFT"), plainQ(t, "R", "RIGHT"))
				return sel(t, expressions.JoinInner, box, plainQ(t, "E", "EXQ"))
			},
			originalArity: 3, existentialCount: 1, want: false,
			why: "the arity alone must not satisfy the guard — an unrelated 2-quantifier box beside an existential is not a null-extended pair",
		},
		{
			// A box nested one level DEEPER than the rewrite ever prepends. The
			// descent looks exactly one level down, so this must not match: a
			// null-on-empty found at arbitrary depth belongs to somebody else's
			// expression, and matching it would suppress a legitimate rewrite.
			name: "a box nested TWO levels down is not this rule's output",
			build: func(t *testing.T) *expressions.SelectExpression {
				innerBox := boxOver(t, plainQ(t, "L", "LEFT"), nullOnEmptyQ(t, "R", "RIGHT"))
				outerBox := boxOver(t, innerBox)
				return sel(t, expressions.JoinInner, outerBox, plainQ(t, "E", "EXQ"))
			},
			originalArity: 3, existentialCount: 1, want: false,
			why: "the rewrite prepends its box directly, so only one level down is its own shape",
		},
		{
			// The box present but NOT first. The rewrite always prepends, so a box
			// in second position is not the form it emits.
			name: "a box sitting anywhere but FIRST is not this rule's output",
			build: func(t *testing.T) *expressions.SelectExpression {
				box := boxOver(t, plainQ(t, "L", "LEFT"), nullOnEmptyQ(t, "R", "RIGHT"))
				return sel(t, expressions.JoinInner, plainQ(t, "E", "EXQ"), box)
			},
			originalArity: 3, existentialCount: 1, want: false,
			why: "position is the claim — the descent reads quants[0], and a box elsewhere is another expression",
		},
		{
			name: "BOXED arity but the first quantifier is a SCAN, not a box",
			build: func(t *testing.T) *expressions.SelectExpression {
				return sel(t, expressions.JoinInner, plainQ(t, "L", "LEFT"), plainQ(t, "E", "EXQ"))
			},
			originalArity: 3, existentialCount: 1, want: false,
			why: "the descent must find a select to look inside; a scan reference has no quantifiers to carry the flag",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isRewrittenOuterJoinForm(tc.build(t), tc.originalArity, tc.existentialCount)
			if got != tc.want {
				t.Fatalf("isRewrittenOuterJoinForm = %t, want %t — %s.\n"+
					"  want TRUE means the rule DECLINES (the form is already in the memo); a wrong "+
					"FALSE re-fires the rule and mints a distinct twin every planning pass, and a "+
					"wrong TRUE suppresses a legitimate rewrite so the shape never gets its plan.",
					got, tc.want, tc.why)
			}
		})
	}
}
