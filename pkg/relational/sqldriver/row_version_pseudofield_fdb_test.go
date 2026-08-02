package sqldriver_test

// RFC-202 S4: the __ROW_VERSION pseudo-field's query-read surface over BASE
// SCANS — the runtime half of Java's PseudoField (PseudoField.java:36-100):
// a version-storing template exposes the ephemeral pseudo-column for name
// resolution, star expansion hides it, a REAL "__ROW_VERSION" column wins,
// and SELECTing the pseudo-field returns the record's 12-byte version at run
// time. The VERSION-index plan shapes (ISCAN/COVERING pins) are exercised by
// the pseudo-field-clash.yamsql corpus carrier.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_RowVersionPseudoField_BaseScan(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rvpf")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rvpf")
	// t3 mirrors pseudo-field-clash.yamsql's t3 (no real __ROW_VERSION
	// column → the pseudo-field applies); t2 declares a REAL "__ROW_VERSION"
	// string column (real-column-wins). No version index — this pins the
	// BASE-SCAN read path.
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE rvpf_tpl
		CREATE TABLE t3(id BIGINT, col1 BIGINT, col2 STRING, PRIMARY KEY(id))
		CREATE TABLE t2(id BIGINT, col1 BIGINT, "__ROW_VERSION" STRING, PRIMARY KEY(id))
		WITH OPTIONS(store_row_versions=true)`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rvpf/s1 WITH TEMPLATE rvpf_tpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_rvpf?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Two separate transactions → two distinct commit versions; the
	// increasing-bytes assertion below depends on that.
	mwjoMustExec(t, db, ctx, "INSERT INTO t3 VALUES (1, 10, 'aa'), (2, 20, 'ab')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t3 VALUES (3, 10, 'ac')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t2 VALUES (1, 10, 'ra'), (2, 20, 'rb')")

	t.Run("select_pseudo_field_returns_version_bytes", func(t *testing.T) {
		t.Parallel()
		rows, err := db.QueryContext(ctx, `SELECT id, "__ROW_VERSION" FROM t3 ORDER BY id`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		versions := map[int64][]byte{}
		for rows.Next() {
			var id int64
			var ver []byte
			if err := rows.Scan(&id, &ver); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(ver) != 12 {
				t.Fatalf("id=%d: version = %x, want 12 bytes (FDBRecordVersion.toBytes)", id, ver)
			}
			versions[id] = ver
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if len(versions) != 3 {
			t.Fatalf("got %d rows, want 3", len(versions))
		}
		// Same transaction → same global version for ids 1 and 2 (distinct
		// local versions); a later transaction → strictly greater bytes.
		if !bytes.Equal(versions[1][:10], versions[2][:10]) {
			t.Errorf("ids 1,2 inserted in one transaction: global version %x != %x", versions[1][:10], versions[2][:10])
		}
		if bytes.Compare(versions[3], versions[1]) <= 0 {
			t.Errorf("id=3 (later tx) version %x not > id=1 version %x", versions[3], versions[1])
		}
	})

	t.Run("star_expansion_hides_pseudo_field", func(t *testing.T) {
		t.Parallel()
		rows, err := db.QueryContext(ctx, "SELECT * FROM t3 ORDER BY id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		if got := strings.Join(cols, ","); got != "ID,COL1,COL2" {
			t.Errorf("SELECT * columns = %s, want ID,COL1,COL2 (the ephemeral pseudo-field must stay hidden)", got)
		}
		n := 0
		for rows.Next() {
			var id, col1 int64
			var col2 string
			if err := rows.Scan(&id, &col1, &col2); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n != 3 {
			t.Errorf("got %d rows, want 3", n)
		}
	})

	t.Run("real_column_wins", func(t *testing.T) {
		t.Parallel()
		// t2 declares a REAL "__ROW_VERSION" STRING column: the reference
		// must read the stored strings, never the record version
		// (PseudoField.fillInIfApplicable's descriptor-defined skip,
		// PseudoField.java:82-85).
		rows, err := db.QueryContext(ctx, `SELECT id, "__ROW_VERSION" FROM t2 ORDER BY id`)
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
		if got[1] != "ra" || got[2] != "rb" {
			t.Errorf("real __ROW_VERSION column values = %v, want map[1:ra 2:rb]", got)
		}
		// And the real column IS visible through the star.
		var cols string
		r, err := db.QueryContext(ctx, "SELECT * FROM t2")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer r.Close()
		cs, err := r.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		cols = strings.Join(cs, ",")
		if cols != "ID,COL1,__ROW_VERSION" {
			t.Errorf("SELECT * FROM t2 columns = %s, want ID,COL1,__ROW_VERSION (a real column is not ephemeral)", cols)
		}
	})

	t.Run("order_by_pseudo_field_desc", func(t *testing.T) {
		t.Parallel()
		// No version index exists — the ordering must still ANSWER
		// (in-memory sort fallback) with commit order reversed.
		rows, err := db.QueryContext(ctx, `SELECT id FROM t3 ORDER BY "__ROW_VERSION" DESC, id DESC`)
		if err != nil {
			t.Fatalf("query: %v", err)
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
		if len(ids) != 3 || ids[0] != 3 || ids[1] != 2 || ids[2] != 1 {
			t.Errorf("ORDER BY \"__ROW_VERSION\" DESC, id DESC → ids %v, want [3 2 1]", ids)
		}
	})
}

// TestFDB_RowVersionPseudoField_DisabledTemplate pins the negative: with
// store_row_versions=false (or absent) the pseudo-column does not exist and a
// query reference fails 42703 (Java: "Attempting to query non existing column
// __ROW_VERSION", IndexTest.java:952-960 — the same UNDEFINED_COLUMN code).
func TestFDB_RowVersionPseudoField_DisabledTemplate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rvpf_off")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rvpf_off")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE rvpf_off_tpl
		CREATE TABLE t3(id BIGINT, col1 BIGINT, PRIMARY KEY(id))
		WITH OPTIONS(store_row_versions=false)`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rvpf_off/s1 WITH TEMPLATE rvpf_off_tpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_rvpf_off?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t3 VALUES (1, 10)")

	_, qerr := db.QueryContext(ctx, `SELECT "__ROW_VERSION" FROM t3`)
	if qerr == nil || !strings.Contains(qerr.Error(), "42703") {
		t.Errorf(`SELECT "__ROW_VERSION" with store_row_versions=false: error = %v, want SQLSTATE 42703`, qerr)
	}
}
