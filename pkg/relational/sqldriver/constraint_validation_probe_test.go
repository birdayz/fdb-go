package sqldriver_test

// Probes data-integrity constraint enforcement at the DDL and DML surface.
// Scalar NOT NULL is unexpressible (Java parity: NOT NULL is only allowed for
// ARRAY column type, rejected at CREATE); the scalar 23502 surface is gone.
// What remains pinned here: the 0A000 CREATE-time rejection itself, INSERT
// column/value count mismatch → 42601, NULL primary key handling, and that a
// nullable column may be omitted.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ConstraintValidationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_constrp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_constrp")

	// Scalar NOT NULL is rejected at CREATE time — RecordMetaData cannot
	// represent scalar non-nullability, so the constraint would silently
	// vanish on catalog round-trip. Java rejects it up front; so do we.
	t.Run("scalar_not_null_rejected_at_create", func(t *testing.T) {
		_, err := setup.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE constrp_nn "+
				"CREATE TABLE t (id BIGINT, req BIGINT NOT NULL, PRIMARY KEY (id))")
		if err == nil {
			t.Fatal("CREATE with scalar NOT NULL unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), "0A000") ||
			!strings.Contains(err.Error(), "NOT NULL is only allowed for ARRAY column type") {
			t.Errorf("scalar NOT NULL create error = %v, want 0A000 \"NOT NULL is only allowed for ARRAY column type\"", err)
		}
	})

	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE constrp "+
			"CREATE TABLE t (id BIGINT, req BIGINT, opt BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_constrp/s WITH TEMPLATE constrp")
	dsn := fmt.Sprintf("fdbsql:///testdb_constrp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, req, opt) VALUES (1, 10, 100)")

	rejects := func(name, q, code string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, q)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded", name)
			}
			if !strings.Contains(err.Error(), code) {
				t.Errorf("%s error = %v, want SQLSTATE %s", name, err, code)
			}
		})
	}

	rejects("insert_value_count_mismatch", "INSERT INTO t (id, req) VALUES (7, 70, 700)", "42601")

	// A NULL primary key INSERT is accepted: the pk column is an ordinary
	// nullable field (scalar NOT NULL being unexpressible), Java's insert
	// visitor only rejects NULL for non-nullable field types, and the record
	// layer's tuple encoding carries a null pk element natively. The old
	// 23502 here rode on proto2 LABEL_REQUIRED, which the emitter no longer
	// produces for any scalar.
	t.Run("insert_null_pk_accepted", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO t (id, req) VALUES (NULL, 80)"); err != nil {
			t.Fatalf("NULL pk insert failed: %v", err)
		}
		var req int64
		if err := db.QueryRowContext(ctx, "SELECT req FROM t WHERE id IS NULL").Scan(&req); err != nil {
			t.Fatalf("read back NULL-pk row: %v", err)
		}
		if req != 80 {
			t.Errorf("NULL-pk row req = %d, want 80", req)
		}
	})

	t.Run("omit_nullable_ok", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO t (id, req) VALUES (4, 40)"); err != nil {
			t.Errorf("omitting nullable opt failed: %v", err)
		}
		var opt sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT opt FROM t WHERE id = 4").Scan(&opt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if opt.Valid {
			t.Errorf("omitted nullable opt = %d, want NULL", opt.Int64)
		}
	})
}
