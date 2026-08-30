package factory_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// The rewrites are checked as LOGIC here, before they are ever allowed to
// accuse the engine of anything.
//
// A predicate-equivalence oracle has a failure mode the row-comparison oracles
// do not: an unsound rewrite reports a CORRECT engine as broken, and the report
// looks exactly like a real finding — same shape, same row diff, same
// confidence. It happened on this oracle's first real run. `Pred` carries its
// own Negated flag, so `lo := *n.Leaf` copied the negation into BOTH halves and
// turned
//
//	NOT (b BETWEEN 7 AND 10)        ==  b < 7 OR b > 10
//
// into
//
//	NOT (b >= 7) AND NOT (b <= 10)  ==  b < 7 AND b > 10
//
// a contradiction, which produced 4 findings against an engine that was right.
// De Morgan: negating the arms must flip the connective.
//
// These assertions are on the RENDERED SQL rather than on tree shape, because
// the rendering is where the negation is actually placed — rowdiff's renderBool
// wraps a Negated leaf in `NOT (…)`, and a tree-shape assertion would have
// agreed with the broken version.
func between(col string, lo, hi any, negated bool) *rowdiff.BoolNode {
	return &rowdiff.BoolNode{Leaf: &rowdiff.Pred{
		Col: col, Op: predicates.ComparisonGreaterThanEq,
		Lit: lo, BetweenHi: hi, IsBetween: true, Negated: negated,
	}}
}

func inList(col string, vals []any, negated bool) *rowdiff.BoolNode {
	return &rowdiff.BoolNode{Leaf: &rowdiff.Pred{
		Col: col, Op: predicates.ComparisonIn, InList: vals, Negated: negated,
	}}
}

func applyByName(t *testing.T, name string, n *rowdiff.BoolNode) (string, bool) {
	t.Helper()
	for _, r := range rewrites {
		if r.name != name {
			continue
		}
		out, ok := r.apply(n)
		if !ok {
			return "", false
		}
		return rowdiff.PredicateSQL(out), true
	}
	t.Fatalf("no rewrite named %q", name)
	return "", false
}

func TestRewriteSoundness(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		rewrite string
		in      *rowdiff.BoolNode
		want    []string // substrings the rendered SQL must contain
		reject  []string // substrings it must NOT contain
	}{
		{
			name:    "plain BETWEEN becomes a conjunction",
			rewrite: "between-to-conjunction",
			in:      between("b", int64(7), int64(10), false),
			want:    []string{"b >= 7", "b <= 10", "AND"},
			reject:  []string{"NOT", " OR "},
		},
		{
			name:    "NEGATED BETWEEN becomes a DISJUNCTION of negated halves",
			rewrite: "between-to-conjunction",
			in:      between("b", int64(7), int64(10), true),
			want:    []string{"NOT (b >= 7)", "NOT (b <= 10)", " OR "},
			// The bug: keeping AND while negating both arms is a contradiction.
			reject: []string{") AND ("},
		},
		{
			name:    "plain IN becomes a disjunction of equalities",
			rewrite: "in-to-or-chain",
			in:      inList("c", []any{int64(1), int64(2)}, false),
			want:    []string{"c = 1", "c = 2", " OR "},
			reject:  []string{"NOT", " AND "},
		},
		{
			name:    "NEGATED IN becomes a CONJUNCTION of negated equalities",
			rewrite: "in-to-or-chain",
			in:      inList("c", []any{int64(1), int64(2)}, true),
			want:    []string{"NOT (c = 1)", "NOT (c = 2)", " AND "},
			reject:  []string{" OR "},
		},
	}

	for _, tc := range cases {
		got, ok := applyByName(t, tc.rewrite, tc.in)
		if !ok {
			t.Errorf("%s: rewrite %q did not apply", tc.name, tc.rewrite)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: rendered %q, want it to contain %q", tc.name, got, w)
			}
		}
		for _, r := range tc.reject {
			if strings.Contains(got, r) {
				t.Errorf("%s: rendered %q, must NOT contain %q — this is the De Morgan error that "+
					"reported a correct engine as broken", tc.name, got, r)
			}
		}
	}

	// A NULL in the IN list must DECLINE, not silently rewrite: `x IN (1,NULL)`
	// and an OR-chain over the same values differ once UNKNOWN is in play, and
	// an oracle wrong about three-valued logic is worse than no oracle.
	if _, ok := applyByName(t, "in-to-or-chain", inList("c", []any{int64(1), nil}, false)); ok {
		t.Error("in-to-or-chain applied to a NULL-bearing IN list; it must decline")
	}

	// eq-to-degenerate-range must DECLINE on a boolean, and this is a TYPE
	// soundness question rather than a value one. `f = TRUE` is legal SQL;
	// `f BETWEEN TRUE AND TRUE` is not, because BETWEEN is an ORDERING
	// comparison and this engine does not order booleans — it answers 42804,
	// correctly. Equality is defined on every comparable type and BETWEEN only
	// on ordered ones, so "a point and a degenerate range denote the same set"
	// holds for the VALUES while failing on the TYPES.
	//
	// Unguarded, this produced 72 findings in 40 seeds, every one of them the
	// oracle emitting SQL the engine was right to reject.
	eqLit := func(col string, lit any) *rowdiff.BoolNode {
		return &rowdiff.BoolNode{Leaf: &rowdiff.Pred{
			Col: col, Op: predicates.ComparisonEquals, Lit: lit,
		}}
	}
	if _, ok := applyByName(t, "eq-to-degenerate-range", eqLit("f", true)); ok {
		t.Error("eq-to-degenerate-range applied to a BOOLEAN equality; BETWEEN is an ordering " +
			"comparison and this engine rejects it on booleans with 42804, so the rewrite must decline")
	}
	if got, ok := applyByName(t, "eq-to-degenerate-range", eqLit("b", int64(5))); !ok {
		t.Error("eq-to-degenerate-range declined an ordered-type equality; it must apply there or " +
			"the rewrite covers nothing")
	} else if !strings.Contains(got, "BETWEEN 5 AND 5") {
		t.Errorf("eq-to-degenerate-range rendered %q, want a degenerate range with equal endpoints", got)
	}

	// Commutativity must actually REORDER, or it compares a query to itself and
	// agrees for free. AND/OR commute in Kleene logic exactly as in two-valued
	// logic, so no NULL guard is needed — this is the one rewrite whose
	// soundness needs no argument, which makes "did it change anything" the
	// only thing worth asserting.
	twoKids := &rowdiff.BoolNode{And: true, Kids: []*rowdiff.BoolNode{
		eqLit("b", int64(1)), eqLit("c", int64(2)),
	}}
	got, ok := applyByName(t, "commute-connectives", twoKids)
	if !ok {
		t.Fatal("commute-connectives declined a two-child AND")
	}
	if strings.Index(got, "c = 2") > strings.Index(got, "b = 1") {
		t.Errorf("commute-connectives rendered %q without reordering; an unchanged rewrite "+
			"compares a query to itself and agrees vacuously", got)
	}

	// Double negation must be a no-op on the ANSWER, so it must at least still
	// mention the original predicate rather than dropping it.
	if dn, ok := applyByName(t, "double-negation", between("b", int64(7), int64(10), false)); !ok || !strings.Contains(dn, "BETWEEN") {
		t.Errorf("double-negation rendered %q, want the original predicate preserved", dn)
	}
}
