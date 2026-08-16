package embedded

import (
	"strings"
	"testing"
)

// vectorLimitProjectionDDL is the un-partitioned HNSW fixture, matching
// TestVectorPlan_NoResidualFoldsToSelfLimiting's schema (a) so the two tests
// differ only in the query, never in what the index can offer.
const vectorLimitProjectionDDL = `CREATE TABLE docs (
		doc_id string, embedding vector(3, half),
		PRIMARY KEY (doc_id))
	CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
		OPTIONS (METRIC = EUCLIDEAN_METRIC)`

// TestVectorPlan_ExplicitLimitEqualToRankStillFolds pins that deleting the
// Go-only PushLimitThroughProjectionRule did not cost the vector fold its
// reachable path.
//
// The worry is specific and not obviously wrong: SinkLimitIntoVectorScanRule
// needs a Limit DIRECTLY above the ordered scan and declines when ANY operator
// sits between them, a Projection included. The deleted rule was the only thing
// that ever moved a Limit below a Projection.
//
// Two neighbours already answer most of it, so this covers the gap between them
// rather than repeating either. TestVectorPlan_NoResidualFoldsToSelfLimiting
// proves a non-star projection does not block the fold.
// TestVectorPlan_TighterOuterLimitDoesNotFold proves a tighter explicit LIMIT is
// deliberately NOT folded. Neither drives an explicit LIMIT EQUAL to the rank
// cap — the one case the fold's equality gate (limit == the scan's adjusted k)
// would accept from an OUTER limit, and therefore the only shape where a
// Projection standing between could cost anything.
//
// The measured plan says why nothing was lost: the fold's Limit is NOT the SQL
// LIMIT. The QUALIFY rank cap's own synthesized Limit sits under the projection,
// directly over the scan, so the fold fires there and leaves the scan in
// self-limiting `rank<=3` mode. The explicit `LIMIT 3` stays above the
// projection as a redundant cap over a scan that already returns at most three
// rows. The deleted rule would have pushed that outer Limit down too; all that
// costs is one cursor wrapper, and it buys back every covering-index rewrite
// under a LIMIT (TestLimitOverProjectionKeepsTheCoveringRewrite).
//
// IF THIS FAILS with an `ordered` scan, the fold declined and the deletion DID
// cost this shape. Fix it by teaching SinkLimitIntoVectorScanRule to see through
// a Projection — NOT by restoring the deleted rule, which pruned the un-pushed
// shape out of REWRITING before PLANNING could offer it the covering
// alternative.
func TestVectorPlan_ExplicitLimitEqualToRankStillFolds(t *testing.T) {
	t.Parallel()

	const sql = `SELECT doc_id FROM docs
		QUALIFY ROW_NUMBER() OVER (
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])
		) <= 3
		LIMIT 3`

	got, err := PlanQueryForTest(sql, vectorLimitProjectionDDL, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// The WHOLE plan, because the arrangement is the finding: an outer Limit,
	// then the Projection, then a scan that is nonetheless self-limiting. A
	// substring pin on the scan alone could not tell that apart from a plan
	// where the projection had been elided or the Limit had moved.
	const want = "Limit(3, Project([_current.DOC_ID#0], VectorIndexScan(DOC_IDX, BY_DISTANCE, prefix=[], rank<=3)))"
	if got != want {
		t.Fatalf("plan = %q,\nwant %q", got, want)
	}

	// Stated separately so a cosmetic change to the plan printer cannot quietly
	// turn the whole-text compare above into a test of the printer while the
	// decline it exists to forbid comes back.
	//
	// ONE assertion, not two: `ordered` and `rank<=` are the SAME slot in
	// RecordQueryVectorIndexPlan's explain (vector_index_scan.go:287-296 — the
	// two arms of `if p.orderedStream`). Checking both would read as independent
	// corroboration and be one fact stated twice.
	if !strings.Contains(got, "rank<=3") {
		t.Errorf("scan is not self-limiting: the fold declined with a Projection between the "+
			"QUALIFY Limit and the scan, which is what deleting PushLimitThroughProjectionRule "+
			"was suspected of causing:\n%s", got)
	}

	// Vacuity guard: this test means something only while the query really does
	// project a subset. A `SELECT *` rewrite upstream would delete the very
	// operator whose presence is the point, and both checks above would then
	// pass while probing nothing.
	if !strings.Contains(got, "Project(") {
		t.Fatalf("no Projection in the plan, so the between-the-Limit-and-the-scan case this "+
			"test exists for is not present:\n%s", got)
	}
}
