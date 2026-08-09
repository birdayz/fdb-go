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
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE covleafmeta CREATE TABLE products (id BIGINT, category INTEGER, price INTEGER, name STRING, PRIMARY KEY (id)) CREATE INDEX idx_cat ON products (category)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE covleafmeta")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO products VALUES (1,2,100,'widget'),(2,2,200,'gadget'),(3,3,150,'gizmo')")

	for _, tc := range []struct {
		name  string
		query string
		want  []string // "NAME|DATABASE_TYPE"
	}{
		{
			// The covering shape: the projected column IS the indexed one.
			name:  "projected_indexed_column_over_covering_leaf",
			query: "SELECT category FROM products WHERE category = 2",
			want:  []string{"CATEGORY|INTEGER"},
		},
		{
			// Still covering — the primary key rides along in the index entry.
			name:  "projected_pk_over_covering_leaf",
			query: "SELECT id FROM products WHERE category = 2",
			want:  []string{"ID|BIGINT"},
		},
		{
			// Control: a non-covered column forces the fetching shape, so this
			// one would pass even with the leaf walk broken. It is here to prove
			// the expected types are right rather than co-degraded.
			name:  "projected_noncovered_column_is_the_control",
			query: "SELECT category, price FROM products WHERE category = 2",
			want:  []string{"CATEGORY|INTEGER", "PRICE|INTEGER"},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.query)
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
			if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", tc.want) {
				t.Fatalf("column metadata for %q\n got=%v\nwant=%v\n"+
					"an UNKNOWN type here means a leaf walk failed to reach the index plan held inside the covering scan",
					tc.query, got, tc.want)
			}
		})
	}
}
