package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// TestFDB_ExistsAliasShadow pins RFC-141 R4 round-9 P1: a WHERE-EXISTS (and
// NOT-EXISTS / projected) whose subquery reuses the OUTER source TABLE, so the
// existential subquery's source alias equals the outer source alias.
//
//	SELECT id FROM t WHERE id > 1 AND EXISTS (SELECT 1 FROM t WHERE id = 1)
//
// The regression: the post-FlatMap re-architecture derived the existential
// INNER correlation from sel.GetSourceAliases()[1] — the inner subquery's
// SOURCE table name. When the subquery scans the SAME table as the outer, that
// name collides with the outer source alias, so the FlatMap bound BOTH the
// outer row and the FirstOrDefault inner under the SAME correlation (the inner
// overwrites the outer), AND a plain outer-only predicate (`id > 1`, correlated
// to the shared name) was misclassified as an INNER (join) predicate and pushed
// below the FOD — filtering the wrong rows. Result: the pass-through outer row
// was NULL / wrong / wrongly filtered.
//
// Java gives every existential quantifier its own UNIQUE correlation identity;
// the fix uses the existential quantifier's unique alias (quants[1].GetAlias())
// as the inner correlation, so outer and inner never collide and outer-vs-inner
// predicate classification stays correct.
//
// Data: t has ids 1,2,3. The inner `SELECT 1 FROM t WHERE id = 1` is NON-empty
// (row id=1 exists), so the EXISTS is TRUE for every outer row. With `id > 1`,
// the outer keeps ids 2,3. A correct plan returns {2,3}; the buggy plan returns
// the wrong set (or NULL/empty) because `id > 1` filtered the inner instead of
// the outer and the outer binding was clobbered.
func TestFDB_ExistsAliasShadow(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_existsaliasshadow")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_existsaliasshadow")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE eas_tmpl "+
		"CREATE TABLE t (id BIGINT NOT NULL, sk BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_existsaliasshadow/s WITH TEMPLATE eas_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_existsaliasshadow?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// ids 1,2,3; sk mirrors id so a correlated self-subquery can discriminate.
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)")

	queryIDs := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eqIDs := func(got, want []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// P1 core: alias-shadow self-subquery WHERE-EXISTS. The non-correlated
	// inner is non-empty (id=1 exists) ⇒ EXISTS is TRUE for all outer rows ⇒
	// `id > 1` keeps {2,3}. The buggy plan clobbers the outer binding and
	// pushes `id > 1` below the FOD ⇒ wrong/empty.
	t.Run("where_exists_alias_shadow", func(t *testing.T) {
		q := "SELECT id FROM t WHERE id > 1 AND EXISTS (SELECT 1 FROM t WHERE id = 1)"
		got := queryIDs(t, q)
		want := []int64{2, 3}
		if !eqIDs(got, want) {
			t.Errorf("WHERE EXISTS alias-shadow: got %v, want %v", got, want)
		}
	})

	// P1 NOT-EXISTS variant: the inner `id = 99` is EMPTY ⇒ NOT EXISTS is TRUE
	// for all outer rows ⇒ `id > 1` keeps {2,3}. (A non-empty inner would make
	// NOT EXISTS false everywhere; we use the empty inner so the outer-only
	// predicate is the sole filter — isolating the misclassification bug.)
	t.Run("where_not_exists_alias_shadow", func(t *testing.T) {
		q := "SELECT id FROM t WHERE id > 1 AND NOT EXISTS (SELECT 1 FROM t WHERE id = 99)"
		got := queryIDs(t, q)
		want := []int64{2, 3}
		if !eqIDs(got, want) {
			t.Errorf("WHERE NOT EXISTS alias-shadow: got %v, want %v", got, want)
		}
	})

	// P1 correlated alias-shadow: the subquery scans the same table T under a
	// DISTINCT alias and correlates to the outer row. `EXISTS (SELECT 1 FROM t
	// AS inner WHERE inner.id = t.id - 1)` is TRUE iff a predecessor row exists,
	// i.e. for ids 2,3 (1 has no predecessor 0). Combined with `id > 1` the
	// answer is still {2,3} but the correlation must resolve the OUTER id, not
	// the clobbered inner binding. The explicit inner alias keeps the source
	// names distinct, but the OUTER source alias is still `T` and the EXISTS
	// correlation references `t.id` — exercising the outer-binding survival.
	t.Run("where_exists_correlated_alias_shadow", func(t *testing.T) {
		q := "SELECT id FROM t AS o WHERE o.id > 1 AND EXISTS (SELECT 1 FROM t AS i WHERE i.id = o.id - 1)"
		got := queryIDs(t, q)
		// ids 2 and 3 have predecessors (1 and 2) ⇒ both pass id>1 and EXISTS.
		want := []int64{2, 3}
		if !eqIDs(got, want) {
			t.Errorf("WHERE EXISTS correlated alias-shadow: got %v, want %v", got, want)
		}
	})

	// P1 projected-EXISTS alias-shadow: the EXISTS is in the SELECT list and the
	// subquery scans the same table T. The projected boolean must read the inner
	// FOD binding while the outer `id` projects the outer row — the two bindings
	// must not collide.
	t.Run("projected_exists_alias_shadow", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t WHERE id = 1) AS e FROM t WHERE id > 1"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		type idBool struct {
			id int64
			e  bool
		}
		var out []idBool
		for rows.Next() {
			var r idBool
			if err := rows.Scan(&r.id, &r.e); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		want := []idBool{{2, true}, {3, true}}
		if len(out) != len(want) {
			t.Fatalf("projected EXISTS alias-shadow: got %v, want %v", out, want)
		}
		for i := range out {
			if out[i] != want[i] {
				t.Errorf("projected EXISTS alias-shadow row %d: got %v, want %v", i, out[i], want[i])
			}
		}
	})
}
