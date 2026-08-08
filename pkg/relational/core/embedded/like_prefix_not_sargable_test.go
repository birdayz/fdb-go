package embedded

import "testing"

// The schema RFC-216 measures against: one STRING column with a
// value index on it, so an equality or inequality on that column has
// a covering access path available and a LIKE has the same one
// available in principle.
const rfc216Schema = `CREATE TABLE t2 (id BIGINT, status STRING, PRIMARY KEY(id))
CREATE INDEX idx_status ON t2 (status)`

// The two rules the PART 3 disabling experiment names. Hoisted because
// DisabledRules treats an unrecognized name as INERT: a typo in one row's
// literal turns that row into "disabling nothing changes nothing", which is
// green. Sharing one constant per rule means a typo cannot be isolated to a
// single-rule row — it also hits the both-off row, which then fails.
const (
	ruleMergeProjectionAndFetch = "MergeProjectionAndFetchRule"
	ruleImplementProjection     = "ImplementProjectionRule"
)

// TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost pins the two
// MEASUREMENTS RFC-216 rests on. Both are NEGATIVE results — they
// record defects that are live at HEAD — so each assertion's failure
// message names what a fix means rather than claiming a bug.
//
// A negative result cited only in prose is the exact defect class
// RFC-216 documents in `like_prefix_pushdown.yaml`, which asserts a
// pushdown that never existed and carries no assertion able to detect
// one. These two facts are what make RFC-216's design question live,
// so they are asserted against the planner rather than described.
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
// TWO rules stamp covering for this shape, redundantly, and both fail on
// the same structural condition:
//
//   - `ImplementProjectionRule` — a PLANNING-phase expression rule, via
//     `findIndexScanPlan` (rule_implement_projection.go:73);
//   - `MergeProjectionAndFetchRule` — a PLANNING-phase implementation
//     rule, via a direct `*RecordQueryIndexPlan` type assertion
//     (rule_merge_projection_and_fetch.go:91), falling through to the
//     :103 fallback when the assertion misses.
//
// Once `PushFilterThroughFetchRule` has pushed a residual below the
// fetch, the fetch's inner is a `RecordQueryPredicatesFilterPlan`, and
// neither the direct assertion nor `findIndexScanPlan` descends through
// it, so the flag is dropped. Java has no such failure mode:
// coveringness there is a distinct class,
// `RecordQueryCoveringIndexPlan`, which does not implement
// `RecordQueryPlanWithIndex` but HOLDS one as a field
// (RecordQueryCoveringIndexPlan.java:74-78), so an intervening
// `Filter` cannot lose it.
//
// PART 3 (subtest) — the disabling experiment that makes "TWO rules,
// redundantly" a measurement rather than a reading of the source.
//
// It matters beyond cosmetics because `isSingularIndexScanWithFetch`
// (planning_cost_model.go:1389) returns true on `indexScanCount == 1`
// before it ever consults `fetchCount`, so an unstamped index scan
// counts as "singular index scan with fetch" at `fetchCount == 0` and
// enters the cost model's contested tier. Whether that flips any
// particular comparison is NOT asserted here and was never measured.
// It does not change THIS query's plan, which is why the lost stamp
// has stayed invisible.
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
				"If this now plans an IndexScan, SOMETHING has given the LIKE an access " +
				"path — but an IndexScan alone does not establish that a LIKE->range " +
				"producer landed, and does not by itself establish a bug either: an " +
				"all-residual match over a full index scan is a legal plan " +
				"(rule_match_intermediate.go:1082-1089 says so in as many words; the " +
				"match is created at :1178 however many predicates stayed residual). " +
				"Check what the scan's BOUND is and " +
				"whether the residual LIKE is still applied above it — " +
				"TestLikeMatch_NoPatternYieldsATightPrefixRange in " +
				"cascades/predicates/comparisons_test.go says no LIKE pattern yields a " +
				"tight range, so the residual may never be dropped whatever the bound is.",
		},
		{
			name: "like_suffix_full_scans",
			sql:  "SELECT id FROM t2 WHERE status LIKE '%act'",
			want: "Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))",
			why: "A leading-% LIKE has an EMPTY constant prefix, so no LIKE-derived range " +
				"exists for it in any design. If this plans an IndexScan, the question to " +
				"answer is whether the scan carries a bound DERIVED FROM THE LIKE (which " +
				"would be wrong, not merely different) or is an unbounded all-residual " +
				"index scan the cost model happened to pick (legal, and only a costing " +
				"question).",
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
				"the scan, the direct stamping branches fire and the stamp survives. " +
				"Also the subject of the PART 3 disabling experiment.",
		},
		{
			name: "residual_below_the_fetch_drops_the_covering_stamp",
			sql:  "SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%'",
			want: "Project([ID#0], PredicatesFilter(IndexScan(IDX_STATUS, [<>]), [1 preds]))",
			why: "The defect itself: same index, same projected columns, same covering " +
				"entry — but a residual now sits between the fetch and the scan and the " +
				"COVERING stamp is gone. If this string gains COVERING the stamp is being " +
				"preserved through the residual — the fix RFC-216's covering-stamp " +
				"blocker asks for, so update the RFC rather than this expectation.",
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

	t.Run("two_rules_stamp_covering_redundantly", func(t *testing.T) {
		t.Parallel()

		// PART 3. The cases above pin the OBSERVABLE (this shape has the
		// stamp, that shape lost it). They cannot pin the CAUSAL claim
		// RFC-216's covering-stamp blocker rests on — that TWO rules
		// stamp it and either one alone suffices — because they stay
		// green if one of the two redundant stampers disappears
		// entirely. A causal claim needs a disabling experiment, so
		// this is one.
		//
		// SCOPE: the direct, no-residual covering control ONLY. On the
		// residual query the experiment measures something else —
		// disabling MergeProjectionAndFetchRule there changes the FETCH
		// shape (Fetch(PredicatesFilter(IndexScan)) instead of
		// PredicatesFilter(IndexScan)) rather than the stamp, which is
		// already absent. So "disabling either alone changes nothing" is a
		// statement about this control, not about the defect shape.
		const sql = "SELECT id FROM t2 WHERE status > 'act'"
		const stamped = "Project([ID#0], IndexScan(IDX_STATUS, [<>] COVERING))"
		// With BOTH stampers off, the projection no longer absorbs the
		// fetch at all: the fetch survives and its inner scan is unstamped.
		const unstamped = "Project([ID#0], Fetch(IndexScan(IDX_STATUS, [<>])))"

		exps := []struct {
			name     string
			disabled []string
			want     string
			why      string
		}{
			{
				name: "merge_alone_off_stamp_survives", disabled: []string{ruleMergeProjectionAndFetch},
				want: stamped,
				why: "ImplementProjectionRule stamps it independently. If this now " +
					"differs, the two stampers are no longer redundant and RFC-216's " +
					"two-rule attribution needs re-deriving — the covering-stamp fix " +
					"then has one site to change, not two.",
			},
			{
				name: "implement_projection_alone_off_stamp_survives", disabled: []string{ruleImplementProjection},
				want: stamped,
				why: "MergeProjectionAndFetchRule stamps it independently. Same " +
					"consequence as above in the other direction.",
			},
			{
				name: "both_off_stamp_is_gone",
				disabled: []string{
					ruleMergeProjectionAndFetch, ruleImplementProjection,
				},
				want: unstamped,
				why: "The load-bearing half: it is these TWO rules and nothing else that " +
					"stamp covering for this shape. If the stamp survives with both " +
					"disabled there is a THIRD stamper the RFC does not account for; if " +
					"planning fails outright, DisabledRules stopped being able to express " +
					"this experiment and the two assertions above became vacuous — an " +
					"unrecognized rule name is INERT, so a rename would silently turn " +
					"both of them into 'disabling nothing changes nothing'.",
			},
		}
		for _, e := range exps {
			t.Run(e.name, func(t *testing.T) {
				t.Parallel()
				got, err := PlanQueryForTestWithDisabledRules(sql, rfc216Schema, nil, e.disabled)
				if err != nil {
					t.Fatalf("planning %q with %v disabled failed: %v", sql, e.disabled, err)
				}
				if got != e.want {
					t.Fatalf("%s\n  query:    %s\n  disabled: %v\n  got:      %s\n  want:     %s\n\n%s",
						e.name, sql, e.disabled, got, e.want, e.why)
				}
			})
		}
	})
}
