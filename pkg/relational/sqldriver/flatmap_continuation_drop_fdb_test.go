package sqldriver_test

import (
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_FlatMap_MidInnerContinuation_NoDrop pins the FlatMap mid-inner
// continuation: a correlated join where one outer row matches MULTIPLE inner
// rows, driven under a tiny per-page budget, must by internal continuation
// resume yield the SAME rows as an unpaginated read. The bug: the value-emit
// path encoded the advanced outer continuation (next outer) and dropped the
// inner position, so a page boundary landing between two inner rows of the same
// outer silently lost that outer's remaining inner rows (Java
// FlatMapPipelinedCursor.Continuation always pairs priorOuter + inner).
func TestFDB_FlatMap_MidInnerContinuation_NoDrop(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := testkit.RFC128DB(t, "flatmap_midinner")

	// t a (1..10) × t2 b (1..8) WHERE b.id > a.id is a correlated NLJ (FlatMap):
	// each outer a has several inner b matches (a=1 → b 2..8, a=2 → b 3..8, ...).
	// No ORDER BY — we compare as multisets so the FlatMap continuation, not a
	// sort above it, is what's under test.
	const join = "SELECT a.id, b.id FROM t a, t2 b WHERE b.id > a.id"

	// Reference: unpaginated (no per-page budget).
	unpaged := sortPairs(testkit.ReadIDPairs(t, ctx, db, join))
	if len(unpaged) == 0 {
		t.Fatalf("setup: join returned no rows")
	}

	// Same query under a tiny scanned-rows budget so the engine paginates across
	// inner boundaries and resumes via the FlatMapContinuation internally.
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 2).
			Build())
	})
	paged := sortPairs(testkit.ReadIDPairsConn(t, ctx, conn, join))

	if fmt.Sprint(paged) != fmt.Sprint(unpaged) {
		t.Fatalf("FlatMap mid-inner continuation dropped/duplicated rows:\n paged (%d) = %v\n unpaged (%d) = %v",
			len(paged), paged, len(unpaged), unpaged)
	}
}

func sortPairs(p [][2]int64) [][2]int64 {
	sort.Slice(p, func(i, j int) bool {
		if p[i][0] != p[j][0] {
			return p[i][0] < p[j][0]
		}
		return p[i][1] < p[j][1]
	})
	return p
}
