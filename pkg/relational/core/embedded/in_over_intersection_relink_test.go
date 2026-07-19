package embedded

// Extraction-relink pins for IN predicates over a compensated pk-intersection
// (found by the RFC-182 rowdiff harness once its generator learned IN/LIMIT).
//
// Before the fix these shapes extracted `PredicatesFilter(<nil>)` /
// `InUnion(PredicatesFilter(<nil>))` and failed the XX000 plan invariant; on
// the tree BEFORE compensated intersections existed, the very same queries
// silently DROPPED the residual instead (wrong rows). Three defects combined:
//   - a set operation was missing from isLeafReplaceable, so a filter over an
//     intersection refused to relink and kept its nil-inner snapshot;
//   - the IN-union wrapper had NO relink at all, so it held the stale
//     pre-relink snapshot of its (correctly relinked) child;
//   - the nil-inner shell guard was Fetch-only, so nil-inner filter shells
//     were picked as if they were valid plans.

import (
	"strings"
	"testing"
)

const inRelinkSchema = `
CREATE TABLE T_RD (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, s STRING, PRIMARY KEY (id))
CREATE INDEX idx_c ON T_RD (c)
CREATE INDEX idx_b ON T_RD (b)`

func TestInOverIntersection_RelinksResidual(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		query string
		preds string // expected residual count marker in the filter
	}{
		{
			name:  "in_residual",
			query: "SELECT * FROM t_rd WHERE (b = 1) AND (c = 9) AND (a IN (1, 3, 7))",
			preds: "[1 preds]",
		},
		{
			name:  "in_plus_scalar_residual",
			query: "SELECT * FROM t_rd WHERE (b = 1) AND (c = 9) AND (s = 'x') AND (a IN (1, 3))",
			preds: "[2 preds]",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanQueryForTest(tc.query, inRelinkSchema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if strings.Contains(plan, "<nil>") {
				t.Fatalf("relink dropped an inner: %s", plan)
			}
			if !strings.Contains(plan, "Intersection(") {
				t.Errorf("want the pk-intersection retained, got: %s", plan)
			}
			if !strings.Contains(plan, "PredicatesFilter(") || !strings.Contains(plan, tc.preds) {
				t.Errorf("want the residual reapplied as %s, got: %s", tc.preds, plan)
			}
		})
	}
}

// TestNestedIn_OverIntersection pins the shape that used to extract
// `InJoin(<nil>)` and fail the XX000 plan invariant: TWO IN predicates plus
// an indexed equality. It began life as a GATE PIN asserting that loud
// decline; the gap is now closed, so it asserts the real plan instead.
//
// The gap was a nil-inner "shell": an `InUnion` holding an `InJoin` whose
// own inner was nil, to be relinked at extraction. RFC-183 removed that
// state at its source — the push-through rules now construct the parent
// with its concrete child, the way Java evaluates memoizePlan as a
// constructor argument — so no repair-at-extraction is involved and this
// test now pins the plan a fully-linked build produces directly.
func TestNestedIn_OverIntersection(t *testing.T) {
	t.Parallel()
	const q = "SELECT * FROM t_rd WHERE (b IN (7,5)) AND (c = 5) AND (a IN (5,9,3,10)) LIMIT 12"
	plan, err := PlanQueryForTest(q, inRelinkSchema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if strings.Contains(plan, "<nil>") {
		t.Fatalf("relink left a nil inner: %s", plan)
	}
	// This assertion USED to demand `InJoin(` here, and that expectation
	// encoded a wrong-rows bug rather than a healthy plan.
	//
	// Master planned this as
	// `Fetch(InUnion(InJoin(PredicatesFilter(IndexScan(IDX_C,[=]),[2 preds]),
	// binding),…))` — BOTH INs evaluated below the fetch on an idx_c index
	// entry. A is not indexed at all, and B has its own index (idx_b) but that
	// is irrelevant here: the entry being read is idx_c's, and it carries
	// neither A nor B. So reaching the nested-InJoin shape required pushing
	// predicates onto an index entry lacking their columns — see the
	// covered-column check in tryTranslateValueRec
	// (rule_push_filter_through_fetch.go). With the push correctly refused,
	// the sound plan drives the indexed equality and re-applies both INs above
	// the fetch. Asserting the sound shape is the point;
	// TestInJoinBelowFetch_RelinksRealChild below keeps a real InJoin shape
	// under test so that path itself stays exercised.
	for _, want := range []string{"IndexScan(IDX_C", "PredicatesFilter(", "[2 preds]"} {
		if !strings.Contains(plan, want) {
			t.Errorf("want %s in the plan, got: %s", want, plan)
		}
	}
}

// TestInJoinBelowFetch_RelinksRealChild is the sentinel
// TestNestedIn_OverIntersection used to be: it keeps
// PushInJoinThroughFetchRule's relink — the path that produced `InJoin(<nil>)`
// — under test, on a query where the shape is SOUND.
//
// An IN over an indexed column pushes below the fetch as
// `Fetch(InJoin(IndexScan(...)))`: the InJoin runs on index entries and the
// fetch is lifted above it, so the whole scan is fetched once instead of once
// per IN value. Every level must carry a real child.
//
// Deliberately a SINGLE IN. The nested two-IN shape is not soundly reachable
// on any schema tried: with one index per column the planner falls back to
// `PredicatesFilter(Scan(T))`, and with a composite (a,b) index it does the
// same. Its former nested `InJoin(InJoin(...))` existed only because the A and
// B predicates were being pushed onto an index entry that carried neither —
// so demanding the nested shape would demand the bug back.
func TestInJoinBelowFetch_RelinksRealChild(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T_AB (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_a ON T_AB (a)
CREATE INDEX idx_b ON T_AB (b)`
	const q = "SELECT id, a FROM t_ab WHERE a IN (1,2) ORDER BY id"
	plan, err := PlanQueryForTest(q, schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if strings.Contains(plan, "<nil>") {
		t.Fatalf("relink left a nil inner: %s", plan)
	}
	// The exact nesting matters: the InJoin must sit BELOW the fetch and hold
	// the index scan. `InJoin(` alone would also match the un-pushed shape.
	if !strings.Contains(plan, "Fetch(InJoin(IndexScan(IDX_A") {
		t.Errorf("want Fetch(InJoin(IndexScan(IDX_A…))) with real children, got: %s", plan)
	}
}
