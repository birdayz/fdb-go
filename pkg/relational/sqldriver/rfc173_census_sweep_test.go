package sqldriver_test

import (
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// TestRFC173CensusSweep is the ordinalized-corpus REACH sweep: every family the
// RFC-173 demolition ordinalized must keep PLANNING cleanly. The name-model
// producers (and their census observer) are DELETED (RFC-173 S4 item B), so a
// regression cannot fall back silently anymore — it surfaces as a LOUD plan
// error, which this sweep trips on. Planning-only.
func TestRFC173CensusSweep(t *testing.T) {
	t.Parallel()
	md := slice3B2bMetadata(t)
	count := func(sql string) (int, error) {
		_, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		return 0, err
	} // plan-success probe (the producer census observer is deleted)

	// ORDINALIZED families — must plan AND fire 0 P4/P5 producers. A regression to
	// the name model (or a plan strand) trips this.
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
		// ENCLOSED E-1b as a name-model CTE JOIN LEG. The BARE-PROJECTED unnest-
		// cluster derived boundary (SELECT "X" …) is an OPAQUE ordinal leg
		// (derivedBodyOpaqueOrdinalLeg): the outer D⋈EEV GATES, so D's body
		// translates FRESH and its unnest cluster gathers+ordinalizes, and the
		// parent reads D's projected row opaquely — no anchored re-enumeration, no
		// name-model producer. (Formerly the flip-sentinel that had to STAY
		// name-model; the enclosed-CTE P1 0-row failure came from forcing the INNER
		// cluster to gate under a still-name-model parent — the opposite fix.)
		{"enclosed_e1b_cte_leg", `WITH D AS (SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" > "X" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."X" FROM D, EEV`},
		// SUBQUERY-carrying conjunct + EXISTS (RFC-173 B — the last P5 residual, now
		// CLOSED): `A.K = (SELECT MIN(EE.CK) FROM EE) AND EXISTS(…)`. classifyLegConjunct
		// sees only splitNonExistsPredicates (the EXISTS is handled by the gather's own
		// machinery), so it classifies the pure scalar-subquery conjunct — a leaf
		// ScalarSubqueryValue the bake leaves untouched while the sibling leg refs
		// ordinalize, its result already registered in t.scalarSubqueries for the
		// statement pre-eval. So it BAKES → the gather owns it → 0 producers. Row
		// correctness pinned in TestFDB_RFC173Slice3B2bFaceA (subquery_conjunct_* arms).
		{"subquery_inner_conj", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" = (SELECT MIN(EE."CK") FROM EE) AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		// MULTI-ESQ now ORDINALIZES (RFC-173): admitExistentialGather admits >1 EXISTS
		// and the gathered wrap `[box, Existential, Existential]` physicalizes via
		// PartitionSelectRule's existential peel — the box unnest gathers instead of
		// name-modeling. Row correctness: TestFDB_RFC173Slice3E1a (agg_multiexists_counts)
		// + TestFDB_CorrelatedExistsProbe (sibling_*). (Was the last name-model box shape.)
		{"exists_multi_esq", `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
		{"grouped_multi_esq", `SELECT "X", COUNT(*) FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X") GROUP BY "X"`},
		// MULTI-ESQ BOX (LEFT/RIGHT/FULL) now ordinalizes too: the `[seed, ∃, ∃]` wrap
		// peels to NLJ and the existential correlations bake positionally at plan time
		// (multiEsqPeelBox), so the box gather owns them. Row correctness:
		// TestFDB_RFC173Slice3B2bFaceA multiesq_{leftbox,fullbox}_projection.
		{"exists_leftbox_multi_esq", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
		{"exists_fullbox_multi_esq", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
		// MULTI-ESQ BOX + a BAKEABLE leg conjunct (`A.K = 100 AND EXISTS AND EXISTS`):
		// the non-EXISTS conjunct bakes over the box's RECORDED legTypes arm while the
		// existential correlations bake via multiEsqPeelBox — both channels gather, 0
		// producers. Row correctness: TestFDB_RFC173Slice3B2bFaceA multiesq_leftbox_conjunct.
		{"exists_leftbox_multi_esq_conj", `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
	}
	for _, tc := range zeroed {
		if _, err := count(tc.sql); err != nil {
			t.Errorf("%s: plan error (should ordinalize cleanly): %v", tc.name, err)
		}
	}
}
