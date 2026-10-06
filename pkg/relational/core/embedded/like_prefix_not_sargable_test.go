package embedded

import "testing"

// The schema these measurements are taken against: one STRING column
// with a value index on it, so an equality or inequality on that
// column has a covering access path available and a LIKE has the same
// one available in principle.
const likePrefixSchema = `CREATE TABLE t2 (id BIGINT, status STRING, PRIMARY KEY(id))
CREATE INDEX idx_status ON t2 (status)`

// The rule the PART 3 disabling experiment names: Java's one fetch-elider for
// a query block, which pushes the block's Map through the fetch. An
// unrecognized name is INERT under DisabledRules, so a typo here turns the
// experiment into "disabling nothing changes nothing" — which the experiment's
// own expectation (a different plan) then catches.
const rulePushMapThroughFetch = "PushMapThroughFetchRule"

// TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost pins the two
// MEASUREMENTS behind TODO.md CQ-33. PART 1 is a NEGATIVE result, live at
// HEAD, so its failure messages name what a fix means rather than claiming a
// bug; PART 2's defect is fixed (RFC-220) and its arm pins the fix.
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
// PART 2 — the covering stamp used to be lost through an intervening
// residual: once `PushFilterThroughFetchRule` pushed a residual below the
// fetch, the downstream rules that stamped coveringness could not descend
// through the `RecordQueryPredicatesFilterPlan`. Coveringness is now a plan
// type built at the access path, as Java's `RecordQueryCoveringIndexPlan`,
// so no operator pushed below the fetch can drop it.
//
// PART 3 (subtest) — the disabling experiment showing the covering plan wins
// because an ancestor can elide the fetch, not because it is preferred.
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
			want: "Map(PredicatesFilter(IndexScan(IDX_STATUS, [*] COVERING), [1 preds]), {ID: _current.ID#0})",
			why: "The scan is UNBOUNDED ([*]) with the LIKE applied above it: PREFER_INDEX " +
				"reads the covering index whole, Java's `COVERING(IDX_STATUS <,>) | FILTER " +
				"_.STATUS LIKE` (F-7c). " +
				"CQ-33's defect: a LIKE conjunct cannot bind an index placeholder. " +
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
			want: "Map(PredicatesFilter(IndexScan(IDX_STATUS, [*] COVERING), [1 preds]), {ID: _current.ID#0})",
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
			want: "Map(IndexScan(IDX_STATUS, [=] COVERING), {ID: _current.ID#0})",
			why: "The control that makes the two full scans above meaningful. If this " +
				"stops using IDX_STATUS the schema no longer offers the access path the " +
				"LIKE cases are being denied, and they prove nothing.",
		},
		{
			name: "inequality_keeps_the_covering_stamp",
			sql:  "SELECT id FROM t2 WHERE status > 'act'",
			want: "Map(IndexScan(IDX_STATUS, [<>] COVERING), {ID: _current.ID#0})",
			why: "The covering control for PART 2: with no residual between the fetch and " +
				"the scan, the direct stamping branches fire and the stamp survives. " +
				"Also the subject of the PART 3 disabling experiment.",
		},
		{
			name: "residual_below_the_fetch_keeps_the_covering_stamp",
			sql:  "SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%'",
			want: "Map(PredicatesFilter(IndexScan(IDX_STATUS, [<>] COVERING), [1 preds]), {ID: _current.ID#0})",
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

		// PART 3, INVERTED BY RFC-220. Coveringness is a plan TYPE constructed at
		// the access path, so no downstream rule participates in the decision and
		// none can take COVERING off a scan that has it. What a downstream rule
		// does decide is WHICH PLAN WINS: fetch elimination. The block's Map is
		// pushed through the fetch by PushMapThroughFetchRule (Java's elider);
		// with it disabled, no ancestor can remove the fetch that coveringness
		// exists to make removable, so the covering path buys nothing and loses
		// on cost to a bare fetching index scan. The covering plan is not damaged;
		// it is not chosen.
		//
		// MergeFetchIntoCoveringIndexRule collapses Fetch(Covering(Index)) into
		// one bare fetching IndexScan, so nothing renders a separate Fetch: a bare
		// `IndexScan(…)` resolves its own records by primary key since RFC-220.
		//
		// SCOPE: the direct, no-residual control ONLY, so the shape difference
		// between configurations stays legible. The residual shape is pinned above.
		const sql = "SELECT id FROM t2 WHERE status > 'act'"
		const merged = "Map(IndexScan(IDX_STATUS, [<>] COVERING), {ID: _current.ID#0})"
		// With the elider off, NOTHING can elide the fetch — and
		// MergeFetchIntoCoveringIndexRule then collapses Fetch(Covering(Index))
		// into a bare fetching index scan, which is sound (a bare index plan
		// resolves its own records by primary key) and one node cheaper. So the
		// plan legitimately uses no covering scan: coveringness buys nothing when
		// no ancestor can remove the fetch it exists to make removable.
		const collapsedToFetchingScan = "Map(IndexScan(IDX_STATUS, [<>]), {ID: _current.ID#0})"

		exps := []struct {
			name     string
			disabled []string
			want     string
			why      string
		}{
			{
				name: "nothing_off_covering_survives", disabled: nil,
				want: merged,
				why:  "PushMapThroughFetchRule removes the fetch above the covering scan.",
			},
			{
				name:     "push_map_off_collapses_to_a_fetching_scan",
				disabled: []string{rulePushMapThroughFetch},
				want:     collapsedToFetchingScan,
				why: "The CONTROL for the assertion above: with the fetch-eliding rule " +
					"disabled, coveringness correctly buys nothing and the plan " +
					"collapses to a single fetching index scan. Together with the arm " +
					"above, this pins that the covering scan is chosen because an " +
					"ancestor can ELIDE the fetch — not because coveringness is stamped " +
					"or preferred unconditionally. " +
					"If planning fails outright instead, DisabledRules stopped being able " +
					"to express this experiment — an unrecognized rule name is INERT, so " +
					"a rename would silently turn this into 'disabling nothing changes " +
					"nothing'.",
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
