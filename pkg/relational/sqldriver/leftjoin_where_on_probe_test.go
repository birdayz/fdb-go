package sqldriver_test

// Probes the classic LEFT JOIN distinction: a predicate on the right table in
// WHERE filters out null-extended rows (effectively an inner join), while the same
// predicate in the ON clause keeps the left rows null-extended. Relates to the
// PR's JOIN-ON theme.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_LeftJoinWhereOnProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ljwo")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ljwo")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ljwo "+
		"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
		"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, x BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX b_aid ON b (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ljwo/s WITH TEMPLATE ljwo")
	dsn := fmt.Sprintf("fdbsql:///testdb_ljwo?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// a: 1, 2 ; b: (1, a_id=1, x=5). a=2 has no matching b.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id, x) VALUES (1, 1, 5)")

	// returns a.id values (with the b.x, NULL-aware)
	rowsOf := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var aid int64
			var bx sql.NullInt64
			if err := rows.Scan(&aid, &bx); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if bx.Valid {
				out = append(out, fmt.Sprintf("%d:%d", aid, bx.Int64))
			} else {
				out = append(out, fmt.Sprintf("%d:NULL", aid))
			}
		}
		sort.Strings(out)
		return out
	}
	eq := func(g, w []string) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}

	t.Run("plain_left_join_null_extends", func(t *testing.T) {
		// a2 has no b → null-extended.
		got := rowsOf("SELECT a.id, b.x FROM a LEFT JOIN b ON b.a_id = a.id ORDER BY a.id")
		if !eq(got, []string{"1:5", "2:NULL"}) {
			t.Errorf("plain LEFT JOIN = %v, want [1:5 2:NULL]", got)
		}
	})
	t.Run("where_on_right_filters_null_extended", func(t *testing.T) {
		// WHERE b.x = 5 drops a2's null row (NULL = 5 is UNKNOWN) → effectively inner.
		got := rowsOf("SELECT a.id, b.x FROM a LEFT JOIN b ON b.a_id = a.id WHERE b.x = 5 ORDER BY a.id")
		if !eq(got, []string{"1:5"}) {
			t.Errorf("LEFT JOIN ... WHERE b.x=5 = %v, want [1:5] (null-extended row filtered)", got)
		}
	})
	t.Run("and_in_on_keeps_null_extended", func(t *testing.T) {
		// same predicate in ON keeps a2 null-extended.
		got := rowsOf("SELECT a.id, b.x FROM a LEFT JOIN b ON b.a_id = a.id AND b.x = 5 ORDER BY a.id")
		if !eq(got, []string{"1:5", "2:NULL"}) {
			t.Errorf("LEFT JOIN ... ON AND b.x=5 = %v, want [1:5 2:NULL] (null-extension kept)", got)
		}
	})
	t.Run("where_is_null_finds_unmatched", func(t *testing.T) {
		// the anti-join idiom: WHERE b.id IS NULL finds left rows with no match.
		got := rowsOf("SELECT a.id, b.x FROM a LEFT JOIN b ON b.a_id = a.id WHERE b.id IS NULL ORDER BY a.id")
		if !eq(got, []string{"2:NULL"}) {
			t.Errorf("LEFT JOIN ... WHERE b.id IS NULL = %v, want [2:NULL] (anti-join)", got)
		}
	})
}
