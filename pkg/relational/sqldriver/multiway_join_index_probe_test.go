package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
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

// TestFDB_MultiwayJoinIndexProbe pins a 3-way chain join on indexed FK columns.
// It asserts TWO things, in order of importance:
//
//	(1) CORRECTNESS — both FROM-orders must return the right rows. Every t3
//	    (200) joins to its single t2 and that t2 to t1=1, so the chain yields
//	    200 rows all with t1.id = 1. This is the load-bearing assertion: a
//	    prior version of this test checked plan SHAPE only and never executed
//	    the query, so it stayed green while the re-enumerated big-first order
//	    silently returned 0 rows / NULL columns — a degenerate cross-product
//	    partition (PartitionSelectRule routed a spanning predicate into the
//	    lower half where its upper alias is unbound, yielding a {_0} placeholder
//	    result). PartitionSelectRule now rejects that degenerate partition (see
//	    its "Reject degenerate partitions" guard), so both orders are correct.
//
//	(2) CAPABILITY — the index-nested-loop probe fires: the small→big FROM-order
//	    plans an IndexScan(t3_by_t2) of the 200-row T3 rather than a full Scan(T3).
//
// Cost-optimal index-probing under the OPPOSITE (big→small) FROM-order — and
// full byte-identical FROM-order invariance — is a stronger, separate property
// (the re-enumerated (t2⋈t3) sub-product still prefers a cross-product NLJ over
// the index probe on cost); tracked in RFC-042. Correctness holds for both.
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

	// (1) CORRECTNESS — both FROM-orders must return 200 rows, all t1.id = 1.
	for _, q := range []string{
		"SELECT t1.id FROM t3, t2, t1 WHERE t3.t2_id = t2.id AND t2.t1_id = t1.id",
		"SELECT t1.id FROM t1, t2, t3 WHERE t3.t2_id = t2.id AND t2.t1_id = t1.id",
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		var n, bad int
		for rows.Next() {
			var id sql.NullInt64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			n++
			if !id.Valid || id.Int64 != 1 {
				bad++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		rows.Close()
		if n != 200 {
			t.Errorf("CORRECTNESS: query %q returned %d rows, want 200:\n  %s", q, n, planExplain(q))
		}
		if bad != 0 {
			t.Errorf("CORRECTNESS: query %q returned %d rows with t1.id != 1 (NULL/wrong), want 0:\n  %s", q, bad, planExplain(q))
		}
	}

	// (2) CAPABILITY — the small→big FROM-order index-probes the 200-row T3 via
	// t3_by_t2 rather than full-scanning it.
	plan := planExplain("SELECT t1.id FROM t1, t2, t3 WHERE t3.t2_id = t2.id AND t2.t1_id = t1.id")
	up := strings.ToUpper(plan)
	if !strings.Contains(up, "INDEXSCAN(T3_BY_T2") {
		t.Errorf("plan does not index-probe T3 via t3_by_t2:\n  %s", plan)
	}
	if strings.Contains(up, "SCAN(T3)") {
		t.Errorf("plan full-scans the 200-row T3 instead of index-probing:\n  %s", plan)
	}
}
