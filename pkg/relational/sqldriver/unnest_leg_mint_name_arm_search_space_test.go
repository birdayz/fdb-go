package sqldriver_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// The SEARCH SPACE for the three seed-tested name-arm call sites of
// `rebaseUnnestOuterLegPredicate` — the buried `!seedWindowed` arm, the EXISTS
// JoinPredicate `!seedWindowed` arm, and the chained `!ordinalSeed` arm.
//
// WHY THIS FILE EXISTS. The unnest leg-mint census reports those three arms at
// ZERO while their branch points are reached, which reads as "reached and
// converted on the other side". That reading answers a corpus question, not a
// reachability one: nothing said whether SQL EXISTS that takes the name arm. The
// three arms cannot be deleted on a corpus zero alone, because two of them are
// non-empty-legs by construction whenever reached, so a deletion there would
// remove the only rebase on an arm guaranteed to have real work — and the
// failure mode is SILENT (an unbound `QOV(leg)` under the existential's merged
// binding evaluates NULL and drops every row an EXISTS should have kept).
//
// WHAT WAS MEASURED, and it is a NEGATIVE result deliberately pinned. Each query
// below was derived from a DECLINE LEVER read out of the guards' own code — not
// from their comments — that would make the seed non-windowed or non-RC:
//
//   - unnestExistsSeedSafe's three decline arms: a box-leg conjunct over a
//     multi-alias non-spine outer; an EXISTS inner scope collision; a
//     multi-alias outer that is not a fresh-gating box.
//   - admitExistentialGather's arms: a FULL box at single-esq with a
//     verdict-None conjunct; an Unbakeable leg conjunct on any box kind.
//   - chainedUnnestOrdinalGate's declines: an inadmissible spine; an
//     exists-unsafe tip base; an impure bottom whose box does not build
//     positional.
//   - enclosure: the cluster used as a CTE leg / derived table, and under an
//     aggregate or DISTINCT wrapper.
//
// NONE of them reaches a name arm. The MECHANISM — which is what makes this
// worth pinning rather than writing down — is that the name-model FlatMap
// fallback these declines used to land on WAS DELETED: an unnest whose seed does
// not ordinalize is now untranslatable and LOUD-rejects (cascades_translator.go,
// "there is no name-model FlatMap fallback left"), and the chained twin
// loud-rejects the same way. So every lever that would produce a non-windowed
// seed produces an ERROR instead of a plan, and the name arm never runs.
//
// THE ASSERTION IS THE OUTCOME PARTITION, not the census counts: the census is a
// process global and this package's tests run in parallel, so counters cannot be
// read here. `declines` is the load-bearing half. A query that moves from
// DECLINE to PLAN means a name-model path was restored under it, which re-arms
// the name arm and RE-OPENS the reachability question this file answered — and
// it would otherwise re-open silently, since the arm is unreachable today only
// because the decline is loud.
//
// SCOPE OF THE NEGATIVE READING, so it can be seen to go stale: at the 40
// queries below, over the two schemas they use, and against the whole
// `./pkg/relational/...` corpus (20435 `=== RUN` subtests, measured with
// unconditional panics wired at all three name arms AND at the two seed
// conditions themselves — zero hits). This is a FAILURE TO CONSTRUCT plus a
// mechanism, NOT a proof of unreachability: no enumeration shows that every
// non-nil return of the unnest lowering carries a windowed ordinal seed. The
// arms therefore stay.
//
// MUTATION TEETH, per arm, stated because an arm nothing can redden is not
// coverage. Three of the four arms redden under a SINGLE source mutation, and
// each reddens a DIFFERENT arm — they test separate sites, not one site four
// times:
//
//   - `declines`   — disabling existsInnerScopeCollidesOuter makes 3 of its 4
//     levers plan; `plans` stays green.
//   - `plans`      — making admitExistentialGather always decline reddens 14 of
//     its entries; `declines` stays green.
//   - `chainPlans` — making chainedUnnestOrdinalGate always decline reddens all
//     12; nothing else moves.
//   - `chainDeclines` — NO single source mutation was found that reddens it.
//     Its levers are walled off by at least FIVE independent loud declines
//     stacked in series (the hoisted box-leg-straddle reject, the impure-bottom
//     coherence guard, the spine-admission Unbakeable arm, the enclosed-parent
//     ordinalize gate, and the FULL-box join-leg reject); lifting three of them
//     together still leaves the shape declining at the fourth. That arm is
//     therefore an OUTCOME assertion whose teeth are undemonstrated, not a
//     verified sentinel — read it as the weakest half of this file.
//
// Planning-only, so it needs no FDB / Docker.
func TestUnnestLegMintNameArmSearchSpace(t *testing.T) {
	t.Parallel()

	// The shapes that PLAN. Each reaches a seed-tested branch point and takes a
	// non-name arm (ordinal-twin, planTimeBake, or leg-relative).
	plans := []struct{ name, sql string }{
		{"single_src_exists", `SELECT "X" FROM A, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"inner_cluster", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		// admitExistentialGather's FULL default arm: verdict None at single esq
		// does NOT gather, yet the seed is still windowed.
		{"full_box_none_single_esq", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"right_box_none", `SELECT "X" FROM A RIGHT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		// Unbakeable / spanning leg conjuncts — the unnestExistsSeedSafe
		// box-leg-conjunct decline arm.
		{"leftbox_unbakeable_conj", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE B."K" IS NOT NULL AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"leftbox_spanning_conj", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" > B."K" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"fullbox_spanning_conj", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" > B."K" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"rightbox_spanning_conj", `SELECT "X" FROM A RIGHT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" > B."K" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"inner_cluster_spanning_conj", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" > B."K" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"inner_cluster_elem_span_conj", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE B."K" > "X" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		// The buried subquery-internal outer-only conjunct — the second name-arm
		// site's own branch point.
		{"buried_fullbox_none", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K" AND A."K" > 3)`},
		{"buried_leftbox_spanning", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" > B."K" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K" AND A."K" > 3)`},
		{"buried_cte_leg", `WITH D AS (SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K" AND A."K" > 3)) SELECT D."X" FROM D, EEV`},
		// Enclosure levers: the cluster as a CTE leg, and under wrappers.
		{"cte_leg_enclosed", `WITH D AS (SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."X" FROM D, EEV`},
		{"cte_leg_enclosed_box", `WITH D AS (SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."X" FROM D, EEV`},
		{"fullbox_cte_leg", `WITH D AS (SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."X" FROM D, EEV`},
		{"fullbox_agg", `SELECT COUNT(*) FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"fullbox_distinct", `SELECT DISTINCT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		// A CTE / derived-table OUTER for the unnest, rather than a base scan.
		{"cte_outer", `WITH C AS (SELECT A."AID" AS "AID", A."K" AS "K", A."ARR" AS "ARR" FROM A) SELECT "X" FROM C, C."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = C."K")`},
		{"derived_outer", `SELECT "X" FROM (SELECT A."AID" AS "AID", A."K" AS "K", A."ARR" AS "ARR" FROM A) AS "D", "D"."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = "D"."K")`},
		// Polarity, ordinality, extra legs, and an uncorrelated multi-source
		// EXISTS inner beside a leg conjunct.
		{"not_exists_fullbox", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"at_ordinality_fullbox", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" AT "O" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"fullbox_3way", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", EEV, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"multisrc_inner_beside_conj", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" = 1 AND EXISTS (SELECT 1 FROM EE, EEV WHERE EE."CK" = EEV."VK" AND EE."CK" = A."K")`},
		// A multi-source EXISTS inner whose aliases do NOT collide with the
		// outer legs — the near-miss of the scope-collision lever.
		{"multisrc_inner_no_collision", `SELECT "X" FROM A, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM B, EE WHERE B."K" = EE."CK" AND EE."CK" = A."K")`},
	}

	// The shapes that LOUD-DECLINE. This is the half that keeps the name arms
	// unreachable: each is a decline lever that once landed on the name model
	// and now has no fallback to land on.
	declines := []struct{ name, sql string }{
		// EXISTS inner scope collision (unnestExistsSeedSafe / the gather).
		{"collide_reuse_A_inner", `SELECT "X" FROM A, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM A, EE WHERE A."K" = EE."CK")`},
		{"collide_reuse_A_box", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM A, EE WHERE A."K" = EE."CK")`},
		{"collide_reuse_B_multisrc", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM B, EE WHERE B."K" = EE."CK" AND EE."CK" = A."K")`},
		{"buried_collide", `SELECT "X" FROM A, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM A, EE WHERE A."K" = EE."CK" AND A."K" > 3)`},
	}

	md := existsGatherSchemaMetadata(t)
	for _, tc := range plans {
		if _, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil); err != nil {
			t.Errorf("%s: expected a plan, got %v\n  sql: %s", tc.name, err, tc.sql)
		}
	}
	for _, tc := range declines {
		_, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		if err == nil {
			t.Errorf("%s: PLANNED, and it used to LOUD-DECLINE.\n"+
				"  A decline lever that starts planning means a non-windowed seed now has a\n"+
				"  path through the unnest lowering. That is exactly what would make the\n"+
				"  `!seedWindowed` name arms of rebaseUnnestOuterLegPredicate reachable, and\n"+
				"  the failure at those arms is SILENT (an unbound leg QOV evaluates NULL and\n"+
				"  EXISTS drops every row). Re-run the reachability measurement before\n"+
				"  touching those arms.\n  sql: %s", tc.name, tc.sql)
		}
	}

	// The CHAINED site's own levers, on the chained schema. The impure-bottom
	// decline is loud (its wording is pinned by
	// TestOuterJoinUnderChainedUnnestDeclines); what matters here is that the
	// admitted chained shapes PLAN — i.e. carry an RC seed — so the
	// `!ordinalSeed` name arm is not the one they take.
	cmd := buildChainedUnnestMetadata(t)
	chainPlans := []struct{ name, sql string }{
		{"chain_nonpushable", `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" > "Y"`},
		{"chain_pushable", `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" > 3`},
		{"chain_or", `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" > "Y" OR T4."SUB" = 1`},
		{"chain_3link", `SELECT "Z" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y", "Y"."DEEP" AS "Z" WHERE T4."ID" > "Z"`},
		{"chain_at_ord", `SELECT "Y" FROM T4, T4."SARR" AS "X" AT "OX", "X"."SUB" AS "Y" WHERE T4."ID" > "OX"`},
		{"chain_multisrc_bottom", `SELECT "Y" FROM T4 AS "A", T4 AS "B", "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" > "Y"`},
		{"chain_multisrc_bottom_far", `SELECT "Y" FROM T4 AS "A", T4 AS "B", "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "B"."ID" > "Y"`},
		{"chain_cte_leg", `WITH D AS (SELECT "Y", T4."ID" AS "TID" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" > "Y") SELECT D."Y" FROM D, T4`},
		{"chain_derived", `SELECT "D"."Y" FROM (SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" > "Y") AS "D"`},
		{"chain_agg", `SELECT COUNT(*) FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" > "Y"`},
		{"chain_group", `SELECT "Y", COUNT(*) FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" > "Y" GROUP BY "Y"`},
		{"chain_fork", `SELECT "Y", "Z" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y", "X"."SUBSTRUCT" AS "Z" WHERE T4."ID" > "Y"`},
	}
	for _, tc := range chainPlans {
		if _, err := embedded.PlanRecordQueryWithMetadata(tc.sql, cmd, nil); err != nil {
			t.Errorf("%s: expected a plan, got %v\n  sql: %s", tc.name, err, tc.sql)
		}
	}
	chainDeclines := []struct{ name, sql string }{
		{"chain_leftbox_bottom", `SELECT "Y" FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" = "B"."ID", "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" > "Y"`},
		{"chain_fullbox_bottom", `SELECT "Y" FROM T4 AS "A" FULL OUTER JOIN T4 AS "B" ON "A"."ID" = "B"."ID", "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" > "Y"`},
		{"chain_rightbox_bottom", `SELECT "Y" FROM T4 AS "A" RIGHT JOIN T4 AS "B" ON "A"."ID" = "B"."ID", "A"."SARR" AS "X", "X"."SUB" AS "Y" WHERE "A"."ID" > "Y"`},
		{"chain_under_exists", `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE EXISTS (SELECT 1 FROM T4 AS "E" WHERE "E"."ID" = T4."ID")`},
	}
	for _, tc := range chainDeclines {
		_, err := embedded.PlanRecordQueryWithMetadata(tc.sql, cmd, nil)
		if err == nil {
			t.Errorf("%s: PLANNED, and it used to LOUD-DECLINE. See the plans/declines note "+
				"above — a chained decline lever that starts planning can carry a non-RC seed "+
				"into the chained `!ordinalSeed` name arm.\n  sql: %s", tc.name, tc.sql)
			continue
		}
		// The declines must be UNSUPPORTED-class, not an accidental syntax or
		// resolution error: a typo in the SQL would decline too, and would make
		// this half of the partition vacuous.
		if !strings.HasPrefix(err.Error(), "0A") {
			t.Errorf("%s: declined with a non-unsupported error %v — the lever may not be "+
				"exercising the path it names.\n  sql: %s", tc.name, err, tc.sql)
		}
	}
	for _, tc := range declines {
		_, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		if err != nil && !strings.HasPrefix(err.Error(), "0A") {
			t.Errorf("%s: declined with a non-unsupported error %v — the lever may not be "+
				"exercising the path it names.\n  sql: %s", tc.name, err, tc.sql)
		}
	}
}
