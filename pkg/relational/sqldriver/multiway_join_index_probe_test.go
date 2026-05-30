package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
)

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func mwjoMustExec(t *testing.T, db execer, ctx context.Context, query string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func mwjoExplainer(t *testing.T, db *sql.DB, ctx context.Context) func(string) string {
	return func(query string) string {
		t.Helper()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("db.Conn: %v", err)
		}
		defer conn.Close()
		var plan string
		if err := conn.Raw(func(driverConn any) error {
			ec, ok := driverConn.(*embedded.EmbeddedConnection)
			if !ok {
				t.Fatalf("expected *embedded.EmbeddedConnection, got %T", driverConn)
			}
			p, err := ec.PlanExplain(ctx, query)
			if err != nil {
				return err
			}
			plan = p
			return nil
		}); err != nil {
			t.Fatalf("PlanExplain(%q): %v", query, err)
		}
		return plan
	}
}

// TestFDB_MultiwayJoinIndexProbe pins that a 3-way chain join on indexed FK
// columns plans an index-nested-loop probe of the inner tables — the inner of a
// re-enumerated multi-way join uses the secondary index instead of a full scan.
// This exercises the full RFC-042 L3 stack: REWRITING flattens to a flat seed
// (L1); PartitionSelectRule re-enumerates associativities; the join predicates
// canonicalize to bare columns (composeFieldOverJoinMerge, so the correlated
// equi-predicate SARGs the index); the inner Select matches the index candidate
// in PLANNING (matchSingleSourceAgainstSelect); and the index-prefix FlatMap
// builds the correlated IndexScan whose reduced cardinality wins the cost model
// (the FlatMap inner is the index scan, so maxDataAccessCardinality reflects the
// probe). Both FROM-orders must index-probe T3 via t3_by_t2 — i.e. neither
// full-scans the 200-row table.
//
// (Full FROM-order byte-invariance is a stronger, separate property — tracked
// in RFC-042; this test pins the index-nested-loop capability that the prior Go
// port lacked entirely.)
func TestFDB_MultiwayJoinIndexProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_mwjip")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mwjip")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE mwjip_tmpl "+
			"CREATE TABLE t1 (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT NOT NULL, t1_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t3 (id BIGINT NOT NULL, t2_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t2_by_t1 ON t2 (t1_id) "+
			"CREATE INDEX t3_by_t2 ON t3 (t2_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mwjip/s WITH TEMPLATE mwjip_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_mwjip?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO t1 VALUES (1)")
	for i := 1; i <= 20; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t2 VALUES (%d, 1)", i))
	}
	for i := 1; i <= 200; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t3 VALUES (%d, %d)", i, (i%20)+1))
	}

	planExplain := mwjoExplainer(t, db, ctx)

	for _, q := range []string{
		"SELECT t1.id FROM t3, t2, t1 WHERE t3.t2_id = t2.id AND t2.t1_id = t1.id",
		"SELECT t1.id FROM t1, t2, t3 WHERE t3.t2_id = t2.id AND t2.t1_id = t1.id",
	} {
		plan := planExplain(q)
		up := strings.ToUpper(plan)
		if !strings.Contains(up, "INDEXSCAN(T3_BY_T2") {
			t.Errorf("plan does not index-probe T3 via t3_by_t2:\n  %s\n  %s", q, plan)
		}
		if strings.Contains(up, "SCAN(T3)") {
			t.Errorf("plan full-scans the 200-row T3 instead of index-probing:\n  %s\n  %s", q, plan)
		}
	}
}
