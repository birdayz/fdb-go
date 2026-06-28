package sqldriver_test

// Probes BYTES columns: binary values with embedded 0x00 / 0xFF round-trip
// exactly (tuple byte-string encoding escapes 0x00), equality via a []byte
// param finds the row, and an indexed BYTES column orders/probes correctly.
// Wire-critical (binary tuple encoding).

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_BytesColumnProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_bytescol")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_bytescol")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE bytescol "+
			"CREATE TABLE t (id BIGINT NOT NULL, data BYTES, PRIMARY KEY (id)) "+
			"CREATE INDEX t_data ON t (data)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_bytescol/s WITH TEMPLATE bytescol")
	dsn := fmt.Sprintf("fdbsql:///testdb_bytescol?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	v1 := []byte{0x00, 0x01, 0x00, 0xFF, 0x00} // embedded null bytes
	v2 := []byte{0xFF, 0xFE, 0xFD}
	v3 := []byte{0x00} // single null byte
	exec := func(q string, args ...any) {
		if _, e := db.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("exec %q: %v", q, e)
		}
	}
	exec("INSERT INTO t (id, data) VALUES (1, ?)", v1)
	exec("INSERT INTO t (id, data) VALUES (2, ?)", v2)
	exec("INSERT INTO t (id, data) VALUES (3, ?)", v3)

	t.Run("roundtrip_embedded_nulls", func(t *testing.T) {
		var got []byte
		if err := db.QueryRowContext(ctx, "SELECT data FROM t WHERE id = 1").Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !bytes.Equal(got, v1) {
			t.Errorf("bytes round-trip = %v, want %v", got, v1)
		}
	})
	t.Run("equality_via_param", func(t *testing.T) {
		var id int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE data = ?", v2).Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if id != 2 {
			t.Errorf("data=v2 → id=%d, want 2", id)
		}
	})
	t.Run("equality_embedded_null_via_param", func(t *testing.T) {
		var id int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE data = ?", v1).Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if id != 1 {
			t.Errorf("data=v1 (embedded nulls) → id=%d, want 1 (0x00 escaping)", id)
		}
	})
	t.Run("order_by_bytes", func(t *testing.T) {
		// byte order: v3 {00} < v1 {00 01 ...} < v2 {FF ...}. ids: 3,1,2.
		rows, err := db.QueryContext(ctx, "SELECT id FROM t ORDER BY data ASC")
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
		if len(got) != 3 || got[0] != 3 || got[1] != 1 || got[2] != 2 {
			t.Errorf("ORDER BY data = %v, want [3 1 2]", got)
		}
	})
}
