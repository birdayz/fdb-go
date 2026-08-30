package factory_test

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// The plan-diversity oracle compares two plans' rows for one query. That is
// only sound where SQL determines the row SET, and `LIMIT n` without ORDER BY
// does not: any n of the qualifying rows is a correct answer, so a different
// access path legitimately returns a different n.
//
// This is pinned as a unit test rather than left to the corpus because the
// classification decides whether a disagreement is reported as an ENGINE BUG.
// Getting it wrong in one direction floods the hunt with false findings (it
// did: 70 of 70 over seeds 500001..501200 were this exact shape); getting it
// wrong in the other silently downgrades every LIMIT query to a row-count
// check and the hunt goes quiet without anything looking broken.
//
// Driving every arm explicitly matters because a sweep only exercises the
// arms its seeds happen to reach, and `offset without ORDER BY` is currently
// unreachable through the generator — rowdiff only ever draws an offset
// alongside a limit and an ORDER BY. An arm no corpus reaches is an untested
// branch, and its first real firing would be read as a finding.
func TestUnstableSubsetClassification(t *testing.T) {
	t.Parallel()
	order := []rowdiff.OrderKey{{Col: "ID"}}

	cases := []struct {
		name string
		q    rowdiff.Query
		want bool
		why  string
	}{
		{
			name: "limit without order by is unstable",
			q:    rowdiff.Query{Limit: 2},
			want: true,
			why:  "any 2 qualifying rows is a correct answer, so two plans may differ",
		},
		{
			name: "limit WITH order by is stable",
			q:    rowdiff.Query{Limit: 2, OrderBy: order},
			want: false,
			why:  "the generator suffixes sort keys with the primary key, so the order is TOTAL and the first 2 rows are unique",
		},
		{
			name: "offset without order by is unstable",
			q:    rowdiff.Query{Offset: 3},
			want: true,
			why:  "skipping a prefix of an unspecified order selects an unspecified subset",
		},
		{
			name: "offset WITH order by is stable",
			q:    rowdiff.Query{Offset: 3, OrderBy: order},
			want: false,
			why:  "a total order makes the skipped prefix determined",
		},
		{
			name: "limit and offset without order by is unstable",
			q:    rowdiff.Query{Limit: 2, Offset: 3},
			want: true,
			why:  "both freedoms compose",
		},
		{
			name: "plain query is stable",
			q:    rowdiff.Query{},
			want: false,
			why:  "no windowing, so the full qualifying set is the answer",
		},
		{
			name: "plain query with order by is stable",
			q:    rowdiff.Query{OrderBy: order},
			want: false,
			why:  "ordering alone never makes the row SET unspecified",
		},
	}

	sawTrue, sawFalse := 0, 0
	for _, tc := range cases {
		got := unstableSubset(tc.q)
		if got != tc.want {
			t.Errorf("%s: unstableSubset = %v, want %v — %s", tc.name, got, tc.want, tc.why)
		}
		if tc.want {
			sawTrue++
		} else {
			sawFalse++
		}
	}
	// Guard both populations. A table that only ever asserts one verdict is
	// satisfied by a constant function, and a constant `false` is exactly the
	// regression that reintroduces the 70 false findings.
	if sawTrue == 0 || sawFalse == 0 {
		t.Fatalf("the table must exercise BOTH verdicts (true=%d false=%d); a single-verdict "+
			"table passes against a constant classifier", sawTrue, sawFalse)
	}
}

// TestCountOnlyDiffStillDetects pins what SURVIVES the downgrade. Classifying
// a query as unstable must not turn its oracle off: the row COUNT is fully
// determined at min(limit, |qualifying|), and a plan returning the wrong
// NUMBER of rows is the wrong-window defect class this repo has shipped once.
func TestCountOnlyDiffStillDetects(t *testing.T) {
	t.Parallel()
	row := func(v int64) []any { return []any{v} }

	if d := countOnlyDiff([][]any{row(1), row(2)}, [][]any{row(9), row(8)}); d != "" {
		t.Errorf("same cardinality, different rows: want no finding (which rows is unspecified), got %q", d)
	}
	d := countOnlyDiff([][]any{row(1), row(2)}, [][]any{row(1)})
	if d == "" {
		t.Fatal("different cardinality must still be a finding — HOW MANY rows a LIMIT returns is " +
			"determined even when WHICH rows is not; losing this check is how a wrong-window bug ships")
	}
	if want := "base=2 alt=1"; !strings.Contains(d, want) {
		t.Errorf("finding text = %q, want it to name the counts (%q)", d, want)
	}
}

// The benign-error allowlist decides whether an execution failure under a
// perturbation is REPORTED AS A BUG or silently counted, so it gets a unit pin
// rather than relying on whatever a sweep happens to provoke.
//
// It is an allowlist and not a denylist on purpose: a denylist fails OPEN, so
// the first unfamiliar error class would be tolerated without anyone deciding
// to tolerate it. This test drives both verdicts, because a table that only
// ever asserts one is satisfied by a constant function — and a constant `true`
// here restores exactly the blindness the allowlist was added to remove.
func TestBenignPerturbationErrorAllowlist(t *testing.T) {
	t.Parallel()

	benign := []struct{ name, msg string }{
		{"scanned-rows budget", "54F01: leaf cursor scan limit exceeded: scan limit reached: scanned-records limit exceeded"},
		{"planner starved by a disabled rule", "0AF00: Cascades planner could not plan query"},
	}
	defects := []struct{ name, msg string }{
		{
			name: "the QOV binding failure this session fixed",
			msg:  `resolution error 46 at qov.binding: exact QOV "q$2265" (RECORD) has no declared runtime binding`,
		},
		{"an internal assertion", "internal error: unreachable branch in plan extraction"},
		{"a type resolution failure", "42804: argument of WHERE must be type boolean"},
		{"an unknown class", "some error nobody has classified yet"},
	}

	for _, c := range benign {
		if !benignPerturbationError(errors.New(c.msg)) {
			t.Errorf("%s: want BENIGN (an expected consequence of the perturbation), got reported "+
				"as a finding — this would make every forced-paging sweep red", c.name)
		}
	}
	for _, c := range defects {
		if benignPerturbationError(errors.New(c.msg)) {
			t.Errorf("%s: want REPORTED, got silently tolerated. The allowlist must not grow to "+
				"cover defect classes; %q is how a real engine failure becomes a counter nobody reads",
				c.name, c.msg)
		}
	}
	if !benignPerturbationError(nil) {
		t.Error("nil must be benign")
	}
	if len(benign) == 0 || len(defects) == 0 {
		t.Fatal("both verdicts must be exercised; a single-verdict table passes against a constant")
	}
}
