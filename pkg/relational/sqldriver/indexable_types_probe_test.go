package sqldriver_test

// Pins which column types accept a secondary index. TIMESTAMP, DATE, FLOAT, INTEGER,
// BOOLEAN, and UUID are all indexable. UUID is a first-class indexable primitive
// (Java's DataType.Primitives.UUID) — RFC-162 taught the whole path: the validator
// accepts the tuple_fields.UUID message, the maintainer writes a byte-identical
// tuple.UUID entry, and the read side promotes a STRING comparand to UUID so
// `WHERE v = '<uuid>'` seeks the 0x30 entry and materializes back to the canonical
// string. The full round-trip lives in uuid_indexable_roundtrip_fdb_test.go.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_IndexableTypesProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openTestDB(t, "/testdb_idxty")
	mwjoMustExec(t, db, ctx, "CREATE DATABASE /testdb_idxty")

	indexable := func(name, ty string) {
		t.Run(name, func(t *testing.T) {
			tmpl := "idxty_" + name
			q := fmt.Sprintf("CREATE SCHEMA TEMPLATE %s CREATE TABLE t (id BIGINT NOT NULL, v %s, PRIMARY KEY (id)) CREATE INDEX t_v ON t (v)", tmpl, ty)
			if _, err := db.ExecContext(ctx, q); err != nil {
				t.Errorf("CREATE INDEX on %s unexpectedly failed: %v", ty, err)
			}
		})
	}
	indexable("timestamp", "TIMESTAMP")
	indexable("date", "DATE")
	indexable("float", "FLOAT")
	indexable("integer", "INTEGER")
	indexable("boolean", "BOOLEAN")

	// RFC-162 COMPLETE: UUID is a first-class indexable primitive. Write side
	// (sites 1-2: isTupleField + scalarToInterface → tuple.UUID entry,
	// byte-identical to Java — pinned by recordlayer/uuid_key_encoding_test.go)
	// AND read side (typed-UUID comparand coercion + [16]byte→string
	// materialization) both land. CREATE INDEX + INSERT + query-by-UUID all work;
	// the full round-trip (equality/IN/range/covering/PK/INL-join/MIN-MAX) is
	// pinned in uuid_indexable_roundtrip_fdb_test.go. Here we keep a minimal
	// end-to-end sentinel: index a UUID column and read the row back by UUID.
	t.Run("uuid_indexable_and_roundtrips", func(t *testing.T) {
		const u = "550e8400-e29b-41d4-a716-446655440000"
		mwjoMustExec(t, db, ctx,
			"CREATE SCHEMA TEMPLATE idxty_uuid CREATE TABLE t (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id)) CREATE INDEX t_v ON t (v)")
		mwjoMustExec(t, db, ctx, "CREATE SCHEMA /testdb_idxty/suuid WITH TEMPLATE idxty_uuid")
		udb, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_idxty?cluster_file=%s&schema=suuid", clusterFilePath))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		t.Cleanup(func() { udb.Close() })
		mwjoMustExec(t, udb, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (1, '%s')", u))
		// Query by UUID via the index and read the value back — the whole point
		// of the read side (string probe → tuple.UUID → 0x30 entry → canonical
		// string out).
		var gotID int64
		var gotV string
		if err := udb.QueryRowContext(ctx,
			fmt.Sprintf("SELECT id, v FROM t WHERE v = '%s'", u)).Scan(&gotID, &gotV); err != nil {
			t.Fatalf("SELECT ... WHERE v = <uuid>: %v", err)
		}
		if gotID != 1 || gotV != u {
			t.Fatalf("round-trip = (id=%d, v=%q), want (1, %q)", gotID, gotV, u)
		}
	})
}
