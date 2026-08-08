package embedded

import "testing"

// The schema RFC-216 measures against: one STRING column with a
// value index on it, so an equality or inequality on that column has
// a covering access path available and a LIKE has the same one
// available in principle.
const rfc216Schema = `CREATE TABLE t2 (id BIGINT, status STRING, PRIMARY KEY(id))
CREATE INDEX idx_status ON t2 (status)`

// TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost pins the two
// MEASUREMENTS RFC-216 rests on. Both are NEGATIVE results — they
// record defects that are live at HEAD — so each assertion's failure
// message names what a fix means rather than claiming a bug.
//
// A negative result cited only in prose is the exact defect class
// RFC-216 §1.1 documents (`like_prefix_pushdown.yaml` asserts a
// pushdown that never existed and cannot detect one). These two facts
// are what make RFC-216's design question live, so they are asserted
// against the planner rather than described.
//
// PART 1 — `LIKE 'prefix%'` is not sargable. `predicates.ComparisonLike`
// is admitted by neither `isSargableComparisonForMatch` nor
// `isScanRangeCompatible`, and nothing produces a `ComparisonStartsWith`
// from a LIKE, so the conjunct can never bind an index placeholder and
// the query full-scans at every table size. The `=` control proves the
// index is reachable for this column, so the full scan is about the
// comparison type and not about the schema.
//
// PART 2 — the covering stamp is lost through an intervening residual.
// `MergeProjectionAndFetchRule` stamps covering only when the fetch's
// inner is DIRECTLY a `RecordQueryIndexPlan`
// (rule_merge_projection_and_fetch.go:91); once `PushFilterThroughFetchRule`
// has pushed a residual below the fetch the inner is a
// `RecordQueryPredicatesFilterPlan`, the rule takes the fallback at
// :117-126, and the flag is dropped. Java has no such failure mode:
// coveringness there is a distinct class,
// `RecordQueryCoveringIndexPlan`, which deliberately does not implement
// `RecordQueryPlanWithIndex`, so `Filter(CoveringIndexPlan)` keeps it.
//
// This matters beyond cosmetics because `isSingularIndexScanWithFetch`
// keys on `indexScanCount==1`, so an unstamped index scan counts as
// "singular index scan with fetch" even at `fetchCount==0` and loses
// cost-model criterion #7 to a primary scan. It does not change THIS
// query's plan — criterion #3 separates the two candidates on residual
// count long before #7 — which is precisely why the defect has stayed
// invisible.
func TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  string
		want string
		why  string
	}{
		{
			name: "like_prefix_full_scans",
			sql:  "SELECT id FROM t2 WHERE status LIKE 'act%'",
			want: "Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))",
			why: "RFC-216's defect: a LIKE conjunct cannot bind an index placeholder. " +
				"If this now plans an IndexScan, a LIKE->range producer has landed and " +
				"RFC-216's premise is closed — check that the residual LIKE is STILL " +
				"applied above the scan (predicates_test.go's " +
				"TestLikeMatch_NoPatternYieldsATightPrefixRange says it must be).",
		},
		{
			name: "like_suffix_full_scans",
			sql:  "SELECT id FROM t2 WHERE status LIKE '%act'",
			want: "Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))",
			why: "A leading-% LIKE has an EMPTY constant prefix, so no range exists for it " +
				"in any design. If this ever plans an IndexScan the range is unbounded and " +
				"the plan is wrong, not merely different.",
		},
		{
			name: "equality_control_uses_the_index",
			sql:  "SELECT id FROM t2 WHERE status = 'active'",
			want: "Project([ID#0], IndexScan(IDX_STATUS, [=] COVERING))",
			why: "The control that makes the two full scans above meaningful. If this " +
				"stops using IDX_STATUS the schema no longer offers the access path the " +
				"LIKE cases are being denied, and they prove nothing.",
		},
		{
			name: "inequality_keeps_the_covering_stamp",
			sql:  "SELECT id FROM t2 WHERE status > 'act'",
			want: "Project([ID#0], IndexScan(IDX_STATUS, [<>] COVERING))",
			why: "The covering control for PART 2: with no residual between the fetch and " +
				"the scan, MergeProjectionAndFetchRule's direct branch fires and the stamp " +
				"survives.",
		},
		{
			name: "residual_below_the_fetch_drops_the_covering_stamp",
			sql:  "SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%'",
			want: "Project([ID#0], PredicatesFilter(IndexScan(IDX_STATUS, [<>]), [1 preds]))",
			why: "The defect itself: same index, same projected columns, same covering " +
				"entry — but a residual now sits between the fetch and the scan and the " +
				"COVERING stamp is gone. If this string gains COVERING the stamp is being " +
				"preserved through the residual, which is the fix RFC-216 §4.1 names as a " +
				"prerequisite for CQ-33 on secondary indexes; update that section rather " +
				"than this expectation.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := PlanQueryForTest(c.sql, rfc216Schema, nil)
			if err != nil {
				t.Fatalf("planning %q failed: %v", c.sql, err)
			}
			if got != c.want {
				t.Fatalf("%s\n  query: %s\n  got:   %s\n  want:  %s\n\n%s",
					c.name, c.sql, got, c.want, c.why)
			}
		})
	}
}
