package sqldriver_test

// Column metadata over a COVERING index leaf.
//
// A covering index scan holds its index plan as a FIELD, not as a child
// (RFC-220 criterion C1), so GetChildren() returns nothing and any walk that
// discovers leaves by recursing through children stops at the covering node
// without ever seeing the scan underneath it.
//
// Two such walks feed result metadata, and each fails in its own direction:
//
//   - the one that finds the index leaf to read record types from. Missing it
//     yields an empty column list, so `SELECT *` reports ZERO columns and every
//     database/sql Scan fails with "expected 0 destination arguments".
//     TestFDB_ProjectionOverSetOp_ColumnDerivation covers that direction.
//
//   - the one that collects leaf DESCRIPTORS, which is what a projection
//     resolves each column's TYPE against.
//
// This test pins the TYPES over a covering leaf. Read what it does and does
// not establish: removing the covering arm from the descriptor walk does NOT
// turn this test red, because projection type resolution then falls through to
// its type-inheritance chain and reaches the same answer. That was measured,
// not assumed — the walk is reached with a covering leaf 436 times across this
// target, and disabling its arm leaves the target's failing set byte-identical
// to the control run. So this test guards the OUTCOME (types stay correct over
// a covering leaf) and not that one arm; do not read a pass here as evidence
// the descriptor walk handles the covering type.
//
// The query is chosen so the projection sits DIRECTLY over the covering scan
// with no fetch between them — `SELECT category ... WHERE category = ?` plans
// as Project([CATEGORY], IndexScan(IDX_CAT, [=] COVERING)). A query whose
// projection needs a non-covered column plans a plain scan instead and cannot
// express the defect.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/conformance/coveringleaf"
)

func TestFDB_CoveringLeafKeepsColumnTypeMetadata(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/testdb_covleafmeta"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	// The schema is the SHARED definition. The planner-side pin
	// (TestCoveringLeafMetadataQueriesStillPlanAsCoveringLeaves, package
	// embedded_test) asserts the plan SHAPE these queries take against the very
	// same DDL — which is the premise this test rests on, and which used to be
	// a hand-kept copy on each side.
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE covleafmeta "+coveringleaf.DDL)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE covleafmeta")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO products VALUES (1,2,100,'widget'),(2,2,200,'gadget'),(3,3,150,'gizmo')")

	// The queries and their expected metadata come from the shared probe table,
	// alongside the coveringness the planner-side pin asserts for each. The
	// control case is marked there (Covering=false) rather than only in a
	// comment here, so "which case is the control" is one fact both sides read.
	if len(coveringleaf.Probes) == 0 {
		t.Fatal("coveringleaf.Probes is empty — this test would report PASS having asserted nothing")
	}
	for _, tc := range coveringleaf.Probes {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.Query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			cts, err := rows.ColumnTypes()
			if err != nil {
				t.Fatalf("columnTypes: %v", err)
			}
			got := make([]string, len(cts))
			for i, ct := range cts {
				got[i] = ct.Name() + "|" + ct.DatabaseTypeName()
			}
			if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", tc.ColumnMeta) {
				t.Fatalf("column metadata for %q\n got=%v\nwant=%v\n"+
					"an UNKNOWN type here means a leaf walk failed to reach the index plan held inside the covering scan",
					tc.Query, got, tc.ColumnMeta)
			}
		})
	}
}
