package embedded

import "testing"

// The schema these measurements are taken against: one STRING column
// with a value index on it, so an equality or inequality on that
// column has a covering access path available and a LIKE has the same
// one available in principle.
const likePrefixSchema = `CREATE TABLE t2 (id BIGINT, status STRING, PRIMARY KEY(id))
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
// MEASUREMENTS behind TODO.md CQ-33. Both are NEGATIVE results — they
// record defects that are live at HEAD — so each assertion's failure
// message names what a fix means rather than claiming a bug.
//
// A negative result carried only in prose is the exact defect class
// `yamsql/testdata/like_prefix_pushdown.yaml` exhibits: it asserts a
// pushdown that never existed and carries no assertion able to detect
// one either way. These two facts are what make CQ-33's design
// question live, so they are asserted against the planner here rather
// than described somewhere.
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
// it, so the flag is dropped. Java (4.12.11.0) has no such failure mode:
// coveringness there is a distinct class,
// `RecordQueryCoveringIndexPlan`, which does not implement
// `RecordQueryPlanWithIndex` but HOLDS one as a field, so an
// intervening `Filter` cannot lose it. Go collapsed the two into a
// `covering bool` on `RecordQueryIndexPlan`, which is what makes the
// flag droppable at all. (The Java source is a gitignored sibling
// checkout, absent from `git ls-files` — that reading cannot be
// re-checked from this tree, so it is INSPECTION, not a measurement.)
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
			why: "CQ-33's defect: a LIKE conjunct cannot bind an index placeholder. " +
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
			name: "residual_below_the_fetch_keeps_the_covering_stamp",
			sql:  "SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%'",
			want: "Project([ID#0], PredicatesFilter(IndexScan(IDX_STATUS, [<>] COVERING), [1 preds]))",
			why: "RFC-220's target. This shape used to LOSE the COVERING stamp: same " +
				"index, same projected columns, same covering entry, but a residual sat " +
				"between the fetch and the scan and the rules that STAMPED coveringness " +
				"could not descend through it. Coveringness is now a plan TYPE built at " +
				"the access path, so there is nothing left to recognise and no operator " +
				"pushed below the fetch can defeat it. If this string LOSES COVERING " +
				"again, coveringness has gone back to being decided downstream — fix " +
				"that, do not update this expectation.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := PlanQueryForTest(c.sql, likePrefixSchema, nil)
			if err != nil {
				t.Fatalf("planning %q failed: %v", c.sql, err)
			}
			if got != c.want {
				t.Fatalf("%s\n  query: %s\n  got:   %s\n  want:  %s\n\n%s",
					c.name, c.sql, got, c.want, c.why)
			}
		})
	}

	t.Run("no_downstream_rule_can_remove_coveringness", func(t *testing.T) {
		t.Parallel()

		// PART 3, INVERTED BY RFC-220 — and the inversion is the point.
		//
		// This experiment used to establish a CAUSAL claim about two redundant
		// STAMPERS: MergeProjectionAndFetchRule and ImplementProjectionRule each
		// stamped coveringness onto the scan, either alone sufficed, and with both
		// disabled the stamp vanished. That claim was true, and it was the disease:
		// coveringness had to be RECOGNISED downstream, so it could be lost — which
		// is exactly what a residual pushed below the fetch did.
		//
		// Coveringness is now a plan TYPE constructed at the access path. There is
		// no stamper left to disable, so the disabling experiment cannot say
		// anything about stamping. What it CAN say is the architectural claim that
		// replaced it: no downstream rule participates in the decision, so no
		// downstream rule can take COVERING off a scan that has it.
		//
		// READ THE THIRD EXPECTATION CAREFULLY, because it is NOT "the marker
		// survives" and an earlier version of this comment claimed it was. Two
		// different things respond to these rules and only one of them is
		// coveringness:
		//
		//   - COVERINGNESS is decided at the access path. Nothing downstream can
		//     remove it. That is what the first two configurations show: with one
		//     fetch-eliding rule gone the other still elides, and the covering scan
		//     is untouched.
		//   - WHICH PLAN WINS is a cost question, and fetch elimination is a
		//     genuinely downstream decision. With BOTH eliders disabled, no ancestor
		//     can remove the fetch that coveringness exists to make removable, so
		//     the covering path buys nothing and loses on cost to a bare fetching
		//     index scan. The covering plan is not damaged; it is not chosen.
		//
		// So the third configuration asserts a plan with NO covering scan — and no
		// Fetch node either, which is the other thing that comment got wrong:
		// MergeFetchIntoCoveringIndexRule collapses Fetch(Covering(Index)) into one
		// bare fetching IndexScan, so nothing renders a separate Fetch. Since
		// RFC-220 a bare `IndexScan(…)` already resolves its own records by primary
		// key; a `Fetch(` node renders only above a COVERING scan, which is exactly
		// what this configuration does not have.
		//
		// SCOPE: the direct, no-residual control ONLY, so the shape difference
		// between configurations stays legible. The residual shape is pinned above.
		const sql = "SELECT id FROM t2 WHERE status > 'act'"
		const merged = "Project([ID#0], IndexScan(IDX_STATUS, [<>] COVERING))"
		// With BOTH downstream rules off, NOTHING can elide the fetch — and
		// MergeFetchIntoCoveringIndexRule then collapses Fetch(Covering(Index))
		// into a bare fetching index scan, which is sound (a bare index plan
		// resolves its own records by primary key) and one node cheaper. So the
		// plan legitimately uses no covering scan: coveringness buys nothing when
		// no ancestor can remove the fetch it exists to make removable.
		const collapsedToFetchingScan = "Project([ID#0], IndexScan(IDX_STATUS, [<>]))"

		exps := []struct {
			name     string
			disabled []string
			want     string
			why      string
		}{
			{
				name: "merge_alone_off_covering_survives", disabled: []string{ruleMergeProjectionAndFetch},
				want: merged,
				why: "ImplementProjectionRule still removes the fetch. Coveringness is " +
					"not at stake in either rule.",
			},
			{
				name: "implement_projection_alone_off_covering_survives", disabled: []string{ruleImplementProjection},
				want: merged,
				why: "MergeProjectionAndFetchRule still removes the fetch. Same as above " +
					"in the other direction.",
			},
			{
				name: "both_off_collapses_to_a_fetching_scan",
				disabled: []string{
					ruleMergeProjectionAndFetch, ruleImplementProjection,
				},
				want: collapsedToFetchingScan,
				why: "The CONTROL for the two assertions above: with every fetch-eliding " +
					"rule disabled, coveringness correctly buys nothing and the plan " +
					"collapses to a single fetching index scan. Together with those two, " +
					"this pins that the covering scan above is chosen because an ancestor " +
					"can ELIDE the fetch — not because coveringness is stamped or " +
					"preferred unconditionally. " +
					"If planning fails outright instead, DisabledRules stopped being able " +
					"to express this experiment and the two assertions above went vacuous " +
					"— an unrecognized rule name is INERT, so a rename would silently turn " +
					"both into 'disabling nothing changes nothing'.",
			},
		}
		for _, e := range exps {
			t.Run(e.name, func(t *testing.T) {
				t.Parallel()
				got, err := PlanQueryForTestWithDisabledRules(sql, likePrefixSchema, nil, e.disabled)
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
