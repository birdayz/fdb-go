package sqldriver_test

// Pins the UNION boundary discovered by the RFC-182 rowdiff harness: the engine
// supports UNION ALL (no dedup) but rejects plain UNION (set dedup) with 0AF00
// "only UNION ALL is supported". That is a capability the engine lacks, not a
// soundness gap — the rowdiff generator emits only UNION ALL. This test locks in
// both arms: UNION ALL returns the right multiset (duplicates preserved across
// overlapping branches), and plain UNION declines LOUDLY rather than silently
// deduping wrong or mis-planning. If UNION-dedup ever lands, the reject arm goes
// red and the oracle's already-written dedup path can be wired into generation.
import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_UnionDedup_Unsupported(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uniondd")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uniondd")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE uniondd "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uniondd/s WITH TEMPLATE uniondd")
	dsn := fmt.Sprintf("fdbsql:///testdb_uniondd?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// id=1 a=1 ; id=2 a=2 ; id=3 a=1
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a) VALUES (1,1),(2,2),(3,1)")

	count := func(q string) (int, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n, rows.Err()
	}

	// UNION ALL keeps duplicates across overlapping branches: a>=1 yields all 3,
	// a<=2 yields all 3 → 6 rows.
	t.Run("union_all_keeps_dups", func(t *testing.T) {
		n, err := count("SELECT id FROM t WHERE a >= 1 UNION ALL SELECT id FROM t WHERE a <= 2")
		if err != nil {
			t.Fatalf("UNION ALL must be supported, got: %v", err)
		}
		if n != 6 {
			t.Errorf("UNION ALL over two full-table branches = %d rows, want 6", n)
		}
	})

	// Plain UNION (dedup) must decline LOUDLY (0AF00), never silently.
	t.Run("plain_union_rejected", func(t *testing.T) {
		_, err := count("SELECT id FROM t WHERE a >= 1 UNION SELECT id FROM t WHERE a <= 2")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Errorf("plain UNION error = %v, want 0AF00 (dedup unsupported, use UNION ALL)", err)
		}
	})
}
