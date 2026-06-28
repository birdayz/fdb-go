package sqldriver_test

// Probes cross/comma joins (cartesian product) and expression projections in the
// SELECT list (boolean comparison, arithmetic). Cross-join cardinality and
// projected computed values are distinct from the ON-join + bare-column paths.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_CrossJoinProjectionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_crossproj")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_crossproj")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE crossproj "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_crossproj/s WITH TEMPLATE crossproj")
	dsn := fmt.Sprintf("fdbsql:///testdb_crossproj?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id) VALUES (10), (20), (30)")

	count := func(q string) int {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n
	}

	t.Run("comma_join_cartesian", func(t *testing.T) {
		if got := count("SELECT a.id, b.id FROM a, b"); got != 6 {
			t.Errorf("comma join = %d rows, want 6 (2x3)", got)
		}
	})
	t.Run("cross_join_keyword_rejected", func(t *testing.T) {
		// Go rejects explicit CROSS JOIN (no ON) with a helpful 0A000 pointing at
		// comma-join. Java does not functionally support no-ON CROSS JOIN either
		// (its visitInnerJoin unconditionally reads the ON expression). comma-join
		// is the working cartesian form (above).
		_, err := db.QueryContext(ctx, "SELECT a.id, b.id FROM a CROSS JOIN b")
		if err == nil {
			t.Fatal("CROSS JOIN unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), "0A000") {
			t.Errorf("CROSS JOIN error = %v, want 0A000", err)
		}
	})
	t.Run("comma_join_with_filter", func(t *testing.T) {
		if got := count("SELECT a.id, b.id FROM a, b WHERE a.id = 1"); got != 3 {
			t.Errorf("comma join WHERE a.id=1 = %d rows, want 3", got)
		}
	})
	t.Run("comma_join_cross_filter", func(t *testing.T) {
		// emulate an equi-join via WHERE across the cartesian product.
		if got := count("SELECT a.id, b.id FROM a, b WHERE b.id = a.id * 10"); got != 2 {
			t.Errorf("comma join b.id=a.id*10 = %d rows, want 2 (1->10, 2->20)", got)
		}
	})

	// Expression projections.
	t.Run("boolean_projection", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id, id > 1 FROM a ORDER BY id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id int64
			var flag bool
			if err := rows.Scan(&id, &flag); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, fmt.Sprintf("%d:%v", id, flag))
		}
		want := []string{"1:false", "2:true"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("boolean projection = %v, want %v", got, want)
		}
	})
	t.Run("arithmetic_projection", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id * 100 + 7 FROM a")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			_ = rows.Scan(&v)
			got = append(got, v)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if len(got) != 2 || got[0] != 107 || got[1] != 207 {
			t.Errorf("arithmetic projection = %v, want [107 207]", got)
		}
	})
}
