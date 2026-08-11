package factory_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// nestComponent returns the `nest=` component of a feature vector, or "" when
// the vector carries none (which is what a FLAT case must produce).
func nestComponent(fv string) string {
	for _, part := range strings.Split(fv, ";") {
		if v, ok := strings.CutPrefix(part, "nest="); ok {
			return v
		}
	}
	return ""
}

func nestArms(fv string) map[string]bool {
	out := map[string]bool{}
	n := nestComponent(fv)
	if n == "" || n == "schema-only" {
		return out
	}
	for _, a := range strings.Split(n, "+") {
		out[a] = true
	}
	return out
}

// TestNestTagDrivesEveryArm is the unit pin the census arm needs.
//
// The corpus reading cannot stand in for it. A full batch exercises only the
// arms its seeds happen to reach, so `agg-arg` and `group-key` read 0 in the
// baseline — not because the arms are wrong but because NestedCandidates
// applies tlpEligible first, which rejects every aggregate before FeatureVector
// is ever called on one. An arm whose first real firing is a corpus change is
// an untested branch being read as a finding.
//
// Every case drives ONE arm and asserts the others stay quiet, because the
// failure that shipped here was an arm firing on the wrong input, not an arm
// that never fired.
func TestNestTagDrivesEveryArm(t *testing.T) {
	t.Parallel()
	nested := rowdiff.GenerateNested(1)
	eq := predicates.ComparisonEquals
	leaf := func(col string) *rowdiff.BoolNode {
		return &rowdiff.BoolNode{Leaf: &rowdiff.Pred{Col: col, Op: eq, Lit: int64(1)}}
	}

	for _, tc := range []struct {
		name string
		q    rowdiff.Query
		proj []string
		want string
	}{
		// --- the select arm, both directions -----------------------------
		// This is the pair the whole change is about. A keys-only JOIN
		// projection is dotted in every entry and nested in none of them; the
		// dot-reading form called it `select` and put 39 committed scenarios
		// on a SELECT-nesting axis they do not sit on.
		{
			name: "keys-only join projection is FLAT",
			q:    rowdiff.Query{Join: &rowdiff.JoinSpec{Inner: true, LeftCol: "ID", RightCol: "ID"}},
			proj: []string{"L.ID", "R.ID"},
			want: "schema-only",
		},
		{
			name: "keys-only three-way join projection is FLAT",
			q:    rowdiff.Query{ThreeWay: &rowdiff.ThreeWayJoinSpec{}},
			proj: []string{"L.ID", "M.ID", "R.ID"},
			want: "schema-only",
		},
		{
			name: "a join projection naming a nested leaf DOES nest",
			q:    rowdiff.Query{Join: &rowdiff.JoinSpec{Inner: true, LeftCol: "ID", RightCol: "ID"}},
			proj: []string{"L.ID", "R.N.A"},
			want: "select",
		},
		{
			name: "a join projection naming a depth-3 leaf DOES nest",
			q:    rowdiff.Query{Join: &rowdiff.JoinSpec{Inner: true, LeftCol: "ID", RightCol: "ID"}},
			proj: []string{"L.ID", "L.N.DP.A"},
			want: "select",
		},
		{
			name: "an unqualified nested projection DOES nest",
			proj: []string{"ID", "N.A"},
			want: "select",
		},
		{
			name: "an unqualified flat projection does not",
			proj: []string{"ID", "A", "C"},
			want: "schema-only",
		},

		// --- every other arm, one at a time ------------------------------
		{
			name: "where",
			q:    rowdiff.Query{Where: leaf("N.A")},
			proj: []string{"ID"},
			want: "where",
		},
		{
			name: "a flat WHERE does not nest",
			q:    rowdiff.Query{Where: leaf("A")},
			proj: []string{"ID"},
			want: "schema-only",
		},
		{
			name: "order",
			q:    rowdiff.Query{OrderBy: []rowdiff.OrderKey{{Col: "N.A"}}},
			proj: []string{"ID"},
			want: "order",
		},
		{
			name: "a flat ORDER BY does not nest",
			q:    rowdiff.Query{OrderBy: []rowdiff.OrderKey{{Col: "A", Qual: "L"}}},
			proj: []string{"ID"},
			want: "schema-only",
		},
		// agg-arg and group-key are UNREACHABLE through NestedCandidates —
		// tlpEligible rejects every aggregate before FeatureVector sees it — so
		// these two cases are the only thing that has ever driven them. They are
		// kept rather than deleted because the exclusion is a property of the TLP
		// ORACLE, not of the feature vector: FeatureVector is the corpus's
		// structural-identity function and rowdiff's own Oracle M does evaluate
		// aggregates, so a vector that could not describe a nested aggregate
		// would be lying about a query the generator emits.
		{
			name: "agg-arg",
			q:    rowdiff.Query{Agg: &rowdiff.AggSpec{Func: rowdiff.AggMax, Col: "N.A"}},
			want: "agg-arg",
		},
		{
			name: "a flat aggregate argument does not nest",
			q:    rowdiff.Query{Agg: &rowdiff.AggSpec{Func: rowdiff.AggMax, Col: "A"}},
			want: "schema-only",
		},
		{
			name: "group-key",
			q:    rowdiff.Query{Agg: &rowdiff.AggSpec{Func: rowdiff.AggCountStar, GroupBy: []string{"N.A"}}},
			want: "group-key",
		},
		{
			name: "a flat group key does not nest",
			q:    rowdiff.Query{Agg: &rowdiff.AggSpec{Func: rowdiff.AggCountStar, GroupBy: []string{"A"}}},
			want: "schema-only",
		},
		{
			name: "exists",
			q:    rowdiff.Query{Exists: &rowdiff.ExistsSpec{CorrCol: "N.A", CorrOp: eq}},
			proj: []string{"ID"},
			want: "exists",
		},
		{
			name: "scalarsub",
			q:    rowdiff.Query{ScalarSub: &rowdiff.ScalarSubSpec{OuterCol: "N.A", Op: eq, Func: rowdiff.AggMax, Col: "A"}},
			proj: []string{"ID"},
			want: "scalarsub",
		},
		{
			name: "a flat scalar-subquery outer column does not nest",
			q:    rowdiff.Query{ScalarSub: &rowdiff.ScalarSubSpec{OuterCol: "A", Op: eq, Func: rowdiff.AggMax, Col: "A"}},
			proj: []string{"ID"},
			want: "schema-only",
		},

		// --- the combination form ----------------------------------------
		// The tag is a SORTED SET, and recording the set rather than a bare
		// nest=1 is the axis's whole point: the nested defects were clauses
		// disagreeing with each other.
		{
			name: "several clauses at once, sorted",
			q: rowdiff.Query{
				Where:   leaf("N.A"),
				OrderBy: []rowdiff.OrderKey{{Col: "N.DP.A"}},
			},
			proj: []string{"ID", "N.S"},
			want: "order+select+where",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nestComponent(factory.FeatureVector(nested, tc.q, tc.proj))
			if got != tc.want {
				t.Errorf("nest = %q, want %q\n  full vector: %s",
					got, tc.want, factory.FeatureVector(nested, tc.q, tc.proj))
			}
		})
	}
}

// TestFlatCaseCarriesNoNestComponent pins the absence, which is what keeps the
// 7000 committed FLAT feature vectors byte-identical: a `nest=` component on a
// flat vector would move every one of them into a different family file.
func TestFlatCaseCarriesNoNestComponent(t *testing.T) {
	t.Parallel()
	flat := rowdiff.Generate(1)
	for _, q := range flat.Queries {
		for _, proj := range flat.ProjectionsFor(q) {
			fv := factory.FeatureVector(flat, q, proj)
			if n := nestComponent(fv); n != "" {
				t.Fatalf("a flat case produced nest=%q: %s", n, fv)
			}
		}
	}
}

// TestSchemaOnlyIsReachable pins the arm that used to be dead.
//
// `schema-only` read 0 across the whole committed corpus, and it was not rare —
// it was UNREACHABLE for any join candidate, because the dot-reading select arm
// fired on the alias of every join projection entry before it could be
// considered. A zero that means "structurally impossible" and a zero that means
// "did not come up" are different facts, and the census could not tell them
// apart.
func TestSchemaOnlyIsReachable(t *testing.T) {
	t.Parallel()
	tags := map[string]int{}
	for seed := uint64(1); seed <= 200; seed++ {
		for _, c := range factory.NestedCandidates(seed) {
			tags[nestComponent(c.FeatureVector)]++
		}
	}
	if tags["schema-only"] == 0 {
		t.Errorf("no generated nested candidate is schema-only, so a nested case whose query nests NOWHERE "+
			"cannot be told from one that nests in its SELECT: %v", tags)
	}
	if tags[""] != 0 {
		t.Errorf("a nested candidate carries no nest= component at all: %v", tags)
	}
	t.Logf("nest tags over NestedCandidates(1..200): %v", tags)
}

// TestNestedScalarSubIsReachable pins a fact the committed corpus does not
// show, and that is exactly why it is pinned here.
//
// No committed scenario carries `nest=…scalarsub…`: all three blessed
// `scalarsub=1` nested scenarios happen to compare a FLAT outer column. That is
// a sampling outcome of the dedup/stratifier, not a property of the arm — the
// generator produces nested-outer-column scalar subqueries readily. Without
// this pin, a change that stopped generating them would look exactly like the
// corpus already looks, and nothing would go red.
func TestNestedScalarSubIsReachable(t *testing.T) {
	t.Parallel()
	var flat, nested int
	for seed := uint64(1); seed <= 200; seed++ {
		for _, c := range factory.NestedCandidates(seed) {
			if !strings.Contains(c.FeatureVector, "scalarsub=1") {
				continue
			}
			if nestArms(c.FeatureVector)["scalarsub"] {
				nested++
			} else {
				flat++
			}
		}
	}
	if nested == 0 {
		t.Errorf("no generated candidate correlates a scalar subquery against a NESTED outer column "+
			"(%d scalar-subquery candidates, all flat) — the nest=scalarsub arm is dead", flat)
	}
	// The flat population matters too: if it collapsed, the arm would be firing
	// on every scalar subquery and would no longer be discriminating anything.
	if flat == 0 {
		t.Errorf("every scalar-subquery candidate reads a nested outer column (%d) — the arm no longer "+
			"distinguishes a nested correlation from a flat one", nested)
	}
	t.Logf("scalar-subquery nested candidates over 200 seeds: %d nested outer column, %d flat", nested, flat)
}
