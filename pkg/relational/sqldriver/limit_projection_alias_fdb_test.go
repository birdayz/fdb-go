package sqldriver_test

// Column-alias pin: pushing a LIMIT below a projection must carry the
// projection's output ALIASES across the rebuild (found by the RFC-182
// rowdiff harness once its generator learned self-joins).
//
// PushLimitThroughProjectionRule rebuilt the projection with the
// non-aliased constructor, so `SELECT l.id AS l_id, r.id AS r_id … LIMIT k`
// reported two columns both named ID. Rows stayed correct — only the result
// METADATA was wrong — which is exactly the kind of divergence a row-only
// corpus never sees. The same query without LIMIT (no push, no rebuild)
// kept its aliases, so the two must be pinned together.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_LimitThroughProjection_KeepsAliases(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/testdb_limalias"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE limalias CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, c BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_c ON t (c)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE limalias")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1,2,2),(2,2,3),(3,0,1)")

	for _, tc := range []struct {
		name     string
		query    string
		wantCols []string
	}{
		{
			// The failing shape: a self-join whose two sides both project ID,
			// distinguished only by their aliases, under a LIMIT.
			name:     "self_join_aliases_under_limit",
			query:    "SELECT l.id AS l_id, r.id AS r_id FROM t AS l JOIN t AS r ON l.a = r.c LIMIT 20",
			wantCols: []string{"L_ID", "R_ID"},
		},
		{
			name:     "self_join_aliases_under_order_by_limit",
			query:    "SELECT l.id AS l_id, r.id AS r_id FROM t AS l JOIN t AS r ON l.a = r.c ORDER BY l.id LIMIT 20",
			wantCols: []string{"L_ID", "R_ID"},
		},
		{
			// Control: no LIMIT means no push and no rebuild.
			name:     "self_join_aliases_without_limit",
			query:    "SELECT l.id AS l_id, r.id AS r_id FROM t AS l JOIN t AS r ON l.a = r.c",
			wantCols: []string{"L_ID", "R_ID"},
		},
		{
			// Single-table alias under LIMIT — the same rule, simpler shape.
			name:     "single_table_alias_under_limit",
			query:    "SELECT id AS renamed FROM t LIMIT 2",
			wantCols: []string{"RENAMED"},
		},
		{
			name:     "multi_alias_under_order_by_limit",
			query:    "SELECT a AS x, id AS y FROM t ORDER BY id LIMIT 2",
			wantCols: []string{"X", "Y"},
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
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns: %v", err)
			}
			if len(cols) != len(tc.wantCols) {
				t.Fatalf("columns = %v, want %v", cols, tc.wantCols)
			}
			for i, want := range tc.wantCols {
				if cols[i] != want {
					t.Errorf("column %d = %q, want %q", i, cols[i], want)
				}
			}
			// Read through so a metadata/row misalignment also trips.
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("scan: %v", err)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
		})
	}
}
