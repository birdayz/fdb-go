package sqltest

// Probes ambiguous column resolution in a join: a column present in BOTH joined
// tables must be qualified — an unqualified reference (in SELECT or WHERE) is a clean
// 42702 ("column reference is ambiguous"). Qualifying it resolves, and columns unique
// to one table resolve unqualified.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_AmbiguousColumnProbe(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_ambp")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_ambp")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE ambp "+
		"CREATE TABLE a (id BIGINT, x BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE b (id BIGINT, y BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_ambp/s WITH TEMPLATE ambp")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_AMBP?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO a (id, x) VALUES (1,100),(2,200)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO b (id, y) VALUES (100,1),(200,2)")

	rowCount := func(q string) (int, error) {
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
	ambiguous := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := rowCount(q)
			if err == nil || !strings.Contains(err.Error(), "42702") {
				t.Errorf("%s error = %v, want 42702 (ambiguous)", q, err)
			}
		})
	}
	resolves := func(name, q string, want int) {
		t.Run(name, func(t *testing.T) {
			n, err := rowCount(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if n != want {
				t.Errorf("%s rows = %d, want %d", q, n, want)
			}
		})
	}

	ambiguous("unqualified_ambiguous_select", "SELECT id FROM a JOIN b ON a.x = b.id")
	ambiguous("unqualified_ambiguous_where", "SELECT a.id FROM a JOIN b ON a.x = b.id WHERE id > 0")
	resolves("qualified_resolves", "SELECT a.id FROM a JOIN b ON a.x = b.id", 2)
	resolves("unique_col_a", "SELECT x FROM a JOIN b ON a.x = b.id", 2)
	resolves("unique_col_b", "SELECT y FROM a JOIN b ON a.x = b.id", 2)
}
