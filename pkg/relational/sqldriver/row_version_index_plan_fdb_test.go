package sqldriver_test

// RFC-202 S4: VERSION-index plan shapes — the EXPLAIN pins mirroring
// pseudo-field-clash.yamsql's t2/t3 blocks. t3 has NO real "__ROW_VERSION"
// column, so its AS-SELECT version indexes are true VERSION indexes and the
// pseudo-field reads ride them; t2 declares a REAL "__ROW_VERSION" STRING
// column, so its same-shaped indexes are plain VALUE indexes
// (real-column-wins) and the existing covering machinery serves them.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_RowVersionIndexPlans(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rvip")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rvip")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE rvip_tpl
		CREATE TABLE t3(id BIGINT, col1 BIGINT, col2 STRING, PRIMARY KEY(id))
		CREATE INDEX t3_version AS SELECT "__ROW_VERSION" FROM t3
		CREATE INDEX t3_col1_version AS SELECT col1, "__ROW_VERSION" FROM t3 ORDER BY col1, "__ROW_VERSION"
		CREATE TABLE t2(id BIGINT, col1 BIGINT, "__ROW_VERSION" STRING, PRIMARY KEY(id))
		CREATE INDEX t2_version AS SELECT "__ROW_VERSION" FROM t2
		CREATE INDEX t2_col1_version AS SELECT col1, "__ROW_VERSION" FROM t2 ORDER BY col1, "__ROW_VERSION"
		WITH OPTIONS(store_row_versions=true)`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rvip/s1 WITH TEMPLATE rvip_tpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_rvip?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The carrier's insert order: odd ids first, even ids second — the
	// version order (insert order) differs from the id order.
	mwjoMustExec(t, db, ctx, "INSERT INTO t3 VALUES (1, 10, 'aa'), (3, 10, 'ac'), (5, 10, 'ae')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t3 VALUES (2, 20, 'ab'), (4, 20, 'ad'), (6, 20, 'af')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t2 VALUES (1, 10, 'aa'), (3, 10, 'ac'), (5, 10, 'ae')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t2 VALUES (2, 20, 'ab'), (4, 20, 'ad'), (6, 20, 'af')")

	explain := mwjoExplainer(t, db, ctx)

	// scanIDs runs the query and returns the ID column values in order.
	scanIDs := func(t *testing.T, query string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return ids
	}

	t.Run("t3_order_by_version_desc_iscan_reverse", func(t *testing.T) {
		t.Parallel()
		q := `SELECT id FROM t3 ORDER BY "__ROW_VERSION" DESC`
		plan := explain(q)
		if !strings.Contains(plan, "T3_VERSION") || !strings.Contains(strings.ToUpper(plan), "REVERSE") {
			t.Errorf("plan for %q = %s\nwant a reverse T3_VERSION index scan (carrier pin: ISCAN(T3_VERSION <,> REVERSE))", q, plan)
		}
		ids := scanIDs(t, q)
		// Reverse insert order: second tx (6,4,2) then first tx (5,3,1) —
		// within one transaction the local version increases in insert order.
		want := []int64{6, 4, 2, 5, 3, 1}
		if len(ids) != 6 {
			t.Fatalf("got %d rows, want 6 (%v)", len(ids), ids)
		}
		for i := range want {
			if ids[i] != want[i] {
				t.Fatalf("ORDER BY version DESC ids = %v, want %v", ids, want)
				break
			}
		}
	})

	t.Run("t3_where_col1_order_by_version_desc", func(t *testing.T) {
		t.Parallel()
		q := `SELECT id FROM t3 WHERE col1 = 20 ORDER BY "__ROW_VERSION" DESC`
		plan := explain(q)
		if !strings.Contains(plan, "T3_COL1_VERSION") || !strings.Contains(strings.ToUpper(plan), "REVERSE") {
			t.Errorf("plan for %q = %s\nwant a reverse T3_COL1_VERSION scan (carrier pin: ISCAN(T3_COL1_VERSION [EQUALS …] REVERSE))", q, plan)
		}
		ids := scanIDs(t, q)
		want := []int64{6, 4, 2}
		if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	})

	t.Run("t2_real_column_covering", func(t *testing.T) {
		t.Parallel()
		// T2_COL1_VERSION is a VALUE index over the REAL string column
		// (real-column-wins): the carrier's covering pin
		// COVERING(T2_COL1_VERSION [EQUALS …] REVERSE -> …) fires.
		qc := `SELECT id, col1 FROM t2 WHERE col1 = 20 ORDER BY "__ROW_VERSION" DESC`
		plan := explain(qc)
		if !strings.Contains(plan, "T2_COL1_VERSION") ||
			!strings.Contains(plan, "COVERING") ||
			!strings.Contains(strings.ToUpper(plan), "REVERSE") {
			t.Errorf("plan for %q = %s\nwant a reverse covering T2_COL1_VERSION scan (carrier pin: COVERING(T2_COL1_VERSION [EQUALS …] REVERSE -> …))", qc, plan)
		}
		crows, err := db.QueryContext(ctx, qc)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer crows.Close()
		var ids []int64
		for crows.Next() {
			var id, col1 int64
			if err := crows.Scan(&id, &col1); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if col1 != 20 {
				t.Fatalf("col1 = %d, want 20", col1)
			}
			ids = append(ids, id)
		}
		if err := crows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if len(ids) != 3 || ids[0] != 6 || ids[1] != 4 || ids[2] != 2 {
			t.Fatalf("ids = %v, want [6 4 2]", ids)
		}
		// The UNFILTERED projection (`SELECT id, "__ROW_VERSION" FROM t2`,
		// carrier explain COVERING(T2_VERSION <,> -> …)) plans as a full
		// scan in Go: the cost model's Scan-vs-covering preference for an
		// unfiltered unordered projection is a PRE-EXISTING divergence,
		// measured identical on a non-version template (index on a plain
		// STRING column: Java COVERING, Go Scan) — not armed by the
		// pseudo-field work. Results-only here.
		q := `SELECT id, "__ROW_VERSION" FROM t2`
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[int64]string{}
		for rows.Next() {
			var id int64
			var v string
			if err := rows.Scan(&id, &v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[id] = v
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		want := map[int64]string{1: "aa", 2: "ab", 3: "ac", 4: "ad", 5: "ae", 6: "af"}
		for id, v := range want {
			if got[id] != v {
				t.Fatalf("t2 rows = %v, want %v", got, want)
			}
		}
	})

	t.Run("t3_select_version_via_index_fetch", func(t *testing.T) {
		t.Parallel()
		// Reading the pseudo-field itself through whatever plan the engine
		// picks: every row must carry a non-null 12-byte version.
		rows, err := db.QueryContext(ctx, `SELECT id, "__ROW_VERSION" FROM t3`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var id int64
			var ver []byte
			if err := rows.Scan(&id, &ver); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(ver) != 12 {
				t.Fatalf("id=%d: version %x, want 12 bytes", id, ver)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n != 6 {
			t.Fatalf("got %d rows, want 6", n)
		}
	})
}
