package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Regression pins for a WRONG-ROWS bug: PushFilterThroughFetchRule pushed
// predicates beneath a FetchFromPartialRecordPlan without checking that the
// index actually covers the columns they read.
//
// The mechanism was a shortcut in tryTranslateValueRec
// (rule_push_filter_through_fetch.go). It treats "not correlated to the source
// alias" as "foreign value, nothing to translate, push it through unchanged".
// That is right for a literal or an outer correlation — but AFTER
// ORDINALIZATION a bare column reference is a FieldValue with Child == nil and
// an EMPTY correlation set. Such a value took the shortcut and rode below the
// fetch untouched, asserting the column existed on the INDEX ENTRY. The
// covered-column check in buildTranslateValueFunction was never consulted, and
// tryTranslateValue's final guard could not catch it either: it only looks for
// residual correlation to the source alias, which an ordinalized field never
// has.
//
// So with INDEX idx_k ON t(k), `k = 5 AND (a > 1 OR b < 2)` planned as
//
//	Fetch(PredicatesFilter(IndexScan(IDX_K), [(A > 1 OR B < 2)]))
//
// evaluating A and B against an index entry that carries only K and the
// primary key.
//
// WHY IT SHIPPED GREEN: the bug is in which plans are GENERATED, and the
// malformed alternative carried a nil-inner "shell" whose stub cost lost the
// comparison, so it was rarely selected. Every test asserted rows or asserted
// a substring of the WINNING plan — no test asserted that an unsound
// alternative is never generated in the first place. RFC-183's baking of real
// children gave the alternative a real (cheaper) cost and it began to win,
// which is how it surfaced. The unprobed dimension was "is the pushed
// predicate evaluable on the partial record", so that is what these pin.
//
// The pins are on plan SHAPE rather than on rows because rows only expose the
// bug when the cost model happens to select the bad plan. Shape catches it
// whether or not it wins.

// findPredicatesByFetchDepth splits every PredicatesFilter's predicates by
// whether it sits BELOW a Fetch (evaluated on partial records / index entries)
// or ABOVE one (evaluated on full records).
func findPredicatesByFetchDepth(p plans.RecordQueryPlan, belowFetch bool, above, below *[]string) {
	if p == nil {
		return
	}
	if pf, ok := p.(*plans.RecordQueryPredicatesFilterPlan); ok {
		for _, pred := range pf.GetPredicates() {
			e, ok := pred.(interface{ Explain() string })
			if !ok {
				continue
			}
			if belowFetch {
				*below = append(*below, e.Explain())
			} else {
				*above = append(*above, e.Explain())
			}
		}
	}
	_, isFetch := p.(*plans.RecordQueryFetchFromPartialRecordPlan)
	for _, c := range p.GetChildren() {
		findPredicatesByFetchDepth(c, belowFetch || isFetch, above, below)
	}
}

// findPushedBelowFetch returns only the predicates evaluated on partial
// records.
func findPushedBelowFetch(p plans.RecordQueryPlan, belowFetch bool, out *[]string) {
	var above []string
	findPredicatesByFetchDepth(p, belowFetch, &above, out)
}

func TestPushFilter_NeverPushesUncoveredColumnBelowFetch(t *testing.T) {
	t.Parallel()
	// idx_k covers K (+ the primary key ID) and nothing else. A, B and M are
	// only readable from the full record, i.e. ABOVE the fetch.
	const schema = `CREATE TABLE t (id bigint, k bigint, a bigint, b bigint, m bigint, PRIMARY KEY (id))
		CREATE INDEX idx_k ON t(k)`

	for _, tc := range []struct {
		name    string
		sql     string
		uncov   []string
		wantTop string
	}{
		{
			name:    "or_residual",
			sql:     "SELECT * FROM t WHERE k = 5 AND (a > 1 OR b < 2)",
			uncov:   []string{"A", "B"},
			wantTop: "PredicatesFilter(Fetch(IndexScan(IDX_K",
		},
		{
			name:    "in_residual",
			sql:     "SELECT * FROM t WHERE k = 5 AND m IN (1, 2, 3)",
			uncov:   []string{"M"},
			wantTop: "PredicatesFilter(Fetch(IndexScan(IDX_K",
		},
		{
			name:  "simple_residual",
			sql:   "SELECT * FROM t WHERE k = 5 AND a > 1",
			uncov: []string{"A"},
		},
		{
			name:  "not_residual",
			sql:   "SELECT * FROM t WHERE k = 5 AND NOT (a > 1)",
			uncov: []string{"A"},
		},
		{
			name:  "and_of_uncovered",
			sql:   "SELECT * FROM t WHERE k = 5 AND a > 1 AND b < 2",
			uncov: []string{"A", "B"},
		},
		{
			name:  "arithmetic_over_uncovered",
			sql:   "SELECT * FROM t WHERE k = 5 AND a + b > 3",
			uncov: []string{"A", "B"},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := PlanPhysicalForTest(tc.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			var pushed []string
			findPushedBelowFetch(p, false, &pushed)
			joined := strings.Join(pushed, " ; ")
			for _, col := range tc.uncov {
				// Word-ish match: the Explain renders columns as `A#2`.
				if strings.Contains(joined, col+"#") {
					t.Errorf("uncovered column %s is evaluated BELOW the fetch on a %s index entry — "+
						"wrong rows.\n  pushed: %s\n  plan:   %s",
						col, "k-only", joined, p.Explain())
				}
			}
			if tc.wantTop != "" && !strings.Contains(p.Explain(), tc.wantTop) {
				t.Errorf("want the residual re-applied above the fetch (%s), got: %s", tc.wantTop, p.Explain())
			}
		})
	}
}

// consumption says HOW a covered column must be consumed. Asserting the
// specific mechanism keeps each case falsifiable: a case whose column folds
// entirely into the scan bounds leaves both the above- and below-fetch
// predicate lists empty, so a bare "not stranded above" check passes
// vacuously no matter what the rule does.
type consumption int

const (
	// consumedByBounds: folded into the index scan's key range, so no
	// residual predicate survives anywhere.
	consumedByBounds consumption = iota
	// consumedByFilterBelow: kept as a predicate, but evaluated on the index
	// entry BELOW the fetch.
	consumedByFilterBelow
)

// TestPushFilter_StillPushesCoveredColumns is the other half: the fix must not
// over-decline. A predicate whose columns ARE covered must still push below
// the fetch — that pushdown is the whole point of the rule, and a guard that
// simply refused everything would pass the test above.
func TestPushFilter_StillPushesCoveredColumns(t *testing.T) {
	t.Parallel()
	// idx_kv covers K and V, so a predicate over either is evaluable on the
	// index entry and belongs below the fetch.
	const schema = `CREATE TABLE t (id bigint, k bigint, v bigint, other bigint, PRIMARY KEY (id))
		CREATE INDEX idx_kv ON t(k, v)`

	for _, tc := range []struct {
		name       string
		sql        string
		want       string      // the covered column
		consumedBy consumption // where it must be consumed
	}{
		// consumedBy states WHERE the covered column must end up. Without it
		// these cases were only asserting a negative ("not above the fetch"),
		// and covered_second_column folds BOTH predicates into the scan bounds
		// — `Fetch(IndexScan(IDX_KV, [=, <>]))`, above and below both EMPTY —
		// so its assertion could not fail for any implementation. A test that
		// cannot fail is not evidence.
		{name: "covered_second_column", sql: "SELECT * FROM t WHERE k = 5 AND v > 1", want: "V", consumedBy: consumedByBounds},
		{name: "covered_or", sql: "SELECT * FROM t WHERE k = 5 AND (v > 1 OR v < 0)", want: "V", consumedBy: consumedByFilterBelow},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := PlanPhysicalForTest(tc.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			plan := p.Explain()
			// A covered column may be consumed three sound ways: folded into
			// the index scan BOUNDS, evaluated in a filter BELOW the fetch, or
			// the plan goes covering and never fetches. What must NOT happen
			// is the column being stranded ABOVE the fetch as an un-pushable
			// residual — that is the signature of an over-declining guard,
			// since it means we fetched whole records to evaluate something
			// the index entry could have answered.
			var above, below []string
			findPredicatesByFetchDepth(p, false, &above, &below)
			if strings.Contains(strings.Join(above, " ; "), tc.want+"#") {
				t.Errorf("covered column %s is stranded as a residual ABOVE the fetch — "+
					"the covered-column guard is over-declining.\n  above: %s\n  plan:  %s",
					tc.want, strings.Join(above, " ; "), plan)
			}

			// Positively assert the mechanism, so neither case can pass by
			// producing nothing at all.
			switch tc.consumedBy {
			case consumedByBounds:
				// Fully folded into the key range: no residual survives
				// anywhere, above or below.
				if len(below) != 0 || len(above) != 0 {
					t.Errorf("want %s folded into the scan BOUNDS with no residual, "+
						"got above=%v below=%v\n  plan: %s", tc.want, above, below, plan)
				}
				if strings.Contains(plan, "PredicatesFilter(") {
					t.Errorf("want %s folded into the scan BOUNDS, but a PredicatesFilter survives: %s",
						tc.want, plan)
				}
			case consumedByFilterBelow:
				// Kept as a predicate, but evaluated on the index entry.
				if !strings.Contains(strings.Join(below, " ; "), tc.want+"#") {
					t.Errorf("want %s evaluated BELOW the fetch, got below=%v\n  plan: %s",
						tc.want, below, plan)
				}
			}
		})
	}
}
