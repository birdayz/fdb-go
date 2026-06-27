package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_ProjectedExistsRound12_OtherPositions extends the round-12 convergence
// backstop to EXISTS in non-WHERE-term / non-top-level-SELECT positions found by
// adversarial probing:
//
//   - a JOIN ON clause (`t1 JOIN t2 ON EXISTS(...)`),
//   - an ORDER BY key (`ORDER BY EXISTS(...)`, `ORDER BY CASE WHEN EXISTS(...) …`),
//   - an EXISTS buried in a SCALAR expression in the WHERE (`WHERE CASE WHEN
//     EXISTS(...) THEN 1 ELSE 0 END = 1`, `WHERE (EXISTS(...)) = true`).
//
// All resolve through paths where the EXISTS would be silently DROPPED or folded
// to a constant false (JOIN ON: the ON condition vanishes → every joined row
// passes; ORDER BY: the key never evaluates → wrong/no ordering; WHERE scalar:
// the existential lowers to a constant → every row dropped). Each is now detected
// structurally (expr.ContainsExistsAtom / expr.WhereExistsInScalarPosition) and
// rejected cleanly. Controls prove the directly-handled shapes still work.
func TestFDB_ProjectedExistsRound12_OtherPositions(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_pexr12pos")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_pexr12pos")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pexr12pos_tmpl "+
		"CREATE TABLE t1 (id BIGINT NOT NULL, col1 BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t2 (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pexr12pos/s WITH TEMPLATE pexr12pos_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_pexr12pos?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 2)")

	assertRejected := func(t *testing.T, q, wantMsg string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			n := 0
			for rows.Next() {
				n++
			}
			rows.Close()
			t.Fatalf("query %q returned %d rows instead of a clean error — "+
				"the EXISTS in this position was silently dropped (round-12)", q, n)
		}
		if !strings.Contains(err.Error(), wantMsg) {
			t.Fatalf("query %q: expected clean rejection %q, got: %v", q, wantMsg, err)
		}
	}

	queryInts := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eq := func(got, want []int64) bool {
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

	exists := "EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id)"

	const unsupportedOrderBy = "EXISTS in an ORDER BY clause is not yet supported"
	const unsupportedWhereScalar = "EXISTS nested in a scalar expression is not yet supported"

	// --- INNER JOIN ON EXISTS → supported (RFC-154 §5, Java parity). The
	// correlated EXISTS gates which (t1,j) pairs survive: EXISTS(t2.fk=t1.id) is
	// true only for t1.id=2 (t2 has the single row fk=2), and j ranges over the
	// one t2 row, so only t1.id=2 is emitted. (Was: ON condition silently dropped,
	// every joined row passed — the cross-product bug.) ---
	t.Run("join_on_exists_supported", func(t *testing.T) {
		if got := queryInts(t, "SELECT t1.id FROM t1 JOIN t2 AS j ON "+exists); !eq(got, []int64{2}) {
			t.Fatalf("INNER JOIN ON EXISTS: got %v want [2]", got)
		}
	})

	// --- ORDER BY EXISTS → reject (was: key dropped, wrong/no ordering). ---
	t.Run("order_by_exists_rejected", func(t *testing.T) {
		assertRejected(t, "SELECT id FROM t1 ORDER BY "+exists, unsupportedOrderBy)
	})
	t.Run("order_by_case_when_exists_rejected", func(t *testing.T) {
		assertRejected(t,
			"SELECT id FROM t1 ORDER BY CASE WHEN "+exists+" THEN 0 ELSE 1 END", unsupportedOrderBy)
	})

	// --- WHERE EXISTS buried in a scalar expression → reject (was: constant-false, 0 rows). ---
	t.Run("where_case_when_exists_rejected", func(t *testing.T) {
		assertRejected(t,
			"SELECT id FROM t1 WHERE CASE WHEN "+exists+" THEN 1 ELSE 0 END = 1", unsupportedWhereScalar)
	})
	t.Run("where_paren_exists_eq_true_rejected", func(t *testing.T) {
		assertRejected(t,
			"SELECT id FROM t1 WHERE ("+exists+") = true", unsupportedWhereScalar)
	})

	// --- Controls: the directly-handled shapes still work. ---
	t.Run("control_join_on_no_exists", func(t *testing.T) {
		got := queryInts(t, "SELECT t1.id FROM t1 JOIN t2 AS j ON j.fk = t1.id")
		if !eq(got, []int64{2}) {
			t.Fatalf("JOIN ON j.fk=t1.id: got %v want [2]", got)
		}
	})
	t.Run("control_where_exists_term", func(t *testing.T) {
		// A top-level WHERE EXISTS term (and its AND conjunct, single-NOT) still works.
		if got := queryInts(t, "SELECT id FROM t1 WHERE "+exists); !eq(got, []int64{2}) {
			t.Fatalf("WHERE EXISTS: got %v want [2]", got)
		}
		if got := queryInts(t, "SELECT id FROM t1 WHERE NOT "+exists+" AND id > 0"); !eq(got, []int64{1, 3}) {
			t.Fatalf("WHERE NOT EXISTS AND id>0: got %v want [1 3]", got)
		}
		if got := queryInts(t, "SELECT id FROM t1 WHERE ("+exists+")"); !eq(got, []int64{2}) {
			t.Fatalf("WHERE (EXISTS): got %v want [2]", got)
		}
	})
	t.Run("control_order_by_exists_alias", func(t *testing.T) {
		// ORDER BY a SELECT-list EXISTS ALIAS still works (the key is the alias
		// identifier, not a raw EXISTS atom — round-3 shape).
		rows, err := db.QueryContext(ctx,
			"SELECT id, "+exists+" AS has_t2 FROM t1 ORDER BY has_t2 DESC, id ASC")
		if err != nil {
			t.Fatalf("ORDER BY exists-alias: %v", err)
		}
		defer rows.Close()
		var ids []int64
		var bs []bool
		for rows.Next() {
			var id int64
			var b bool
			if err := rows.Scan(&id, &b); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
			bs = append(bs, b)
		}
		if len(ids) != 3 || ids[0] != 2 || !bs[0] {
			t.Fatalf("ORDER BY exists-alias DESC: got ids=%v bools=%v want id 2 (true) first", ids, bs)
		}
	})
	t.Run("control_order_by_column", func(t *testing.T) {
		if got := queryInts(t, "SELECT id FROM t1 ORDER BY col1 DESC"); !eq(got, []int64{1, 2, 3}) {
			t.Fatalf("ORDER BY col1: got %v", got)
		}
	})
}
