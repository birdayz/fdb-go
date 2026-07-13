package sqldriver_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
	rquery "fdb.dev/pkg/relational/core/query"
)

// TestRFC173CensusSweep is a demolition-progress guard: it pins that the shape
// families whose result values have been ORDINALIZED (off the name model) fire
// ZERO P4/P5 name-model producers — so a future slice that regresses one back to
// the name model trips this red. Serial (process-global observer), planning-only.
// It also documents the KNOWN-still-firing family (the E-1b target) so the census
// scope toward the S4 atomic demolition is legible in one place.
func TestRFC173CensusSweep(t *testing.T) { //nolint:paralleltest // process-global observer, must be serial
	md := slice3B2bMetadata(t)
	count := func(sql string) (int, error) {
		n := 0
		rquery.SetProducerCensusObserver(func(rquery.ProducerCensusRecord) { n++ })
		defer rquery.SetProducerCensusObserver(nil)
		_, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		return n, err
	}

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
	}
	for _, tc := range zeroed {
		n, err := count(tc.sql)
		if err != nil {
			t.Errorf("%s: plan error (should ordinalize cleanly): %v", tc.name, err)
			continue
		}
		if n != 0 {
			t.Errorf("%s: fired %d P4/P5 producer(s), want 0 (regressed to the name model)", tc.name, n)
		}
	}

	// UNBAKEABLE conjunct (subquery-carrying) STAYS name-model (correct-or-loud):
	// classifyFlatLegConjunct returns Unbakeable → admitExistentialGather declines
	// → the name model owns it → producers fire. Flip-sentinel: if a future slice
	// ordinalizes the subquery-conjunct shape, this goes red.
	if n, err := count(`SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" = (SELECT MIN(EE."CK") FROM EE) AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`); err != nil {
		t.Errorf("exists_inner_unbakeable_conj: unexpected plan error: %v", err)
	} else if n == 0 {
		t.Error("exists_inner_unbakeable_conj now fires 0 — a subquery-carrying conjunct should stay name-model (Unbakeable decline)")
	}

	// exists_multi_esq: fires during translation but STRANDS at physicalization (a
	// pre-existing multi-esq-under-EXISTS limitation, orthogonal to the census).
	if _, err := count(`SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`); err == nil || !strings.Contains(err.Error(), "not a physical plan") {
		t.Errorf("exists_multi_esq: expected the pre-existing physicalization strand, got: %v", err)
	}
}
