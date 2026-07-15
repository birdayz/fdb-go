package sqldriver_test

import (
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// TestJoinUnnestExistsPlanSmoke is a broad PLAN-ONLY smoke test: every join /
// unnest / EXISTS shape below must keep planning cleanly. Planning-only.
func TestJoinUnnestExistsPlanSmoke(t *testing.T) {
	t.Parallel()
	md := existsGatherSchemaMetadata(t)
	count := func(sql string) (int, error) {
		_, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		return 0, err
	}

	// Every shape below must plan cleanly.
	zeroed := []struct{ name, sql string }{
		{"join2", `SELECT A."K", B."K" FROM A, B`},
		{"join2_on", `SELECT A."K" FROM A JOIN B ON A."AID" = B."BID"`},
		{"leftjoin", `SELECT A."K" FROM A LEFT JOIN B ON A."AID" = B."BID"`},
		{"fulljoin", `SELECT A."K" FROM A FULL OUTER JOIN B ON A."AID" = B."BID"`},
		{"join3", `SELECT A."K" FROM A, B, EE`},
		{"unnest1", `SELECT "X" FROM A, A."ARR" AS "X"`},
		{"unnest_multi", `SELECT "X" FROM A, B, A."ARR" AS "X"`},
		{"unnest_box", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`},
		{"groupby_join", `SELECT A."K", COUNT(*) FROM A, B GROUP BY A."K"`},
		{"groupby_unnest", `SELECT "X", COUNT(*) FROM A, A."ARR" AS "X" GROUP BY "X"`},
		{"count_join", `SELECT COUNT(*) FROM A, B`},
		{"scalar_sub", `SELECT A."AID", (SELECT COUNT(*) FROM EE WHERE EE."CK" = A."K") FROM A`},
		{"orderby_join", `SELECT A."K" FROM A, B ORDER BY A."K"`},
		{"distinct_join", `SELECT DISTINCT A."K" FROM A, B`},
		{"exists_box", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"exists_inner", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},                       // E-1a
		{"exists_inner_conj", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},  // E-1b (was the flip-sentinel)
		{"exists_inner_elem_conj", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE "X" = 7 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`}, // E-1b element conjunct
		// A CTE whose body is a BARE-PROJECTED unnest cluster (SELECT "X" …), used
		// as a JOIN LEG under the outer D⋈EEV join: the body translates
		// independently and the parent reads D's projected row directly, without
		// re-enumerating the CTE's internals.
		{"enclosed_e1b_cte_leg", `WITH D AS (SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" > "X" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."X" FROM D, EEV`},
		// A conjunct carrying a scalar subquery, alongside EXISTS:
		// `A.K = (SELECT MIN(EE.CK) FROM EE) AND EXISTS(…)`. classifyLegConjunct
		// classifies the scalar-subquery conjunct as a leaf value left untouched by
		// the bake (its result is already registered in the statement's
		// scalar-subquery pre-eval), while the sibling leg refs resolve by ordinal.
		// Row correctness pinned in TestFDB_BoxJoinMultiExistsGather
		// (subquery_conjunct_* arms).
		{"subquery_inner_conj", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" = (SELECT MIN(EE."CK") FROM EE) AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		// Multiple EXISTS over the same unnest cluster: admitExistentialGather
		// admits >1 EXISTS, and the gathered wrap `[box, Existential, Existential]`
		// physicalizes via PartitionSelectRule's existential peel. Row correctness:
		// the multi-EXISTS aggregate-counts test + TestFDB_CorrelatedExistsProbe
		// (sibling_*).
		{"exists_multi_esq", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
		{"grouped_multi_esq", `SELECT "X", COUNT(*) FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X") GROUP BY "X"`},
		// Multiple EXISTS over a LEFT/RIGHT/FULL box unnest: the `[seed, ∃, ∃]`
		// wrap peels to a nested-loop join and the existential correlations bake
		// positionally at plan time (multiEsqPeelBox). Row correctness:
		// TestFDB_BoxJoinMultiExistsGather multiesq_{leftbox,fullbox}_projection.
		{"exists_leftbox_multi_esq", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
		{"exists_fullbox_multi_esq", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
		// A box unnest with multiple EXISTS plus a bakeable leg conjunct
		// (`A.K = 100 AND EXISTS AND EXISTS`): the non-EXISTS conjunct bakes over
		// the box's recorded leg types, and the existential correlations bake via
		// multiEsqPeelBox. Row correctness: TestFDB_BoxJoinMultiExistsGather
		// multiesq_leftbox_conjunct.
		{"exists_leftbox_multi_esq_conj", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
	}
	for _, tc := range zeroed {
		if _, err := count(tc.sql); err != nil {
			t.Errorf("%s: plan error (should plan cleanly): %v", tc.name, err)
		}
	}
}
