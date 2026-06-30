package sqldriver_test

// Pins which column types accept a secondary index. TIMESTAMP, DATE, FLOAT, INTEGER,
// and BOOLEAN are all indexable. UUID is NOT — CREATE INDEX on a UUID column fails
// with a leaky XX000 ("field is a message type; use Nest()") because Go stores UUID
// as the tuple_fields.UUID proto MESSAGE and the record-layer index validation
// rejects message-typed fields.
//
// KNOWN GAP (TODO.md "UUID columns are not indexable; leaky XX000"): this is most
// likely a Go DIVERGENCE — Java treats UUID as a first-class indexable PRIMITIVE
// (DataType.Primitives.UUID / Type.uuidType(); SemanticAnalyzer.java:724,
// DataTypeUtils.java:152), so a UUID index works in Java. Go should either support it
// (teach the index maintainer to treat the tuple_fields.UUID message as an indexable
// primitive) or at minimum report a clean user error, not the internal XX000. This
// test pins the current boundary (other types indexable; UUID → XX000); flip the UUID
// case when fixed.

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

	// RFC-162 sites 1-2 DONE: the index validator accepts the tuple_fields.UUID
	// message (isTupleField) and the maintainer writes the entry as a tuple.UUID
	// (scalarToInterface → uuidMessageToTuple, byte-identical to Java — pinned by
	// recordlayer/uuid_key_encoding_test.go). So CREATE INDEX + INSERT now succeed.
	//
	// The READ side (RFC-162 sites 3-5: typed-UUID comparand coercion + the
	// materialization-boundary conversion) is the Graefe-ACK'd follow-up. Until it
	// lands, `SELECT … WHERE v = '<uuid>'` via the index would mis-match (string
	// probe vs tuple.UUID entry), so this transitional sentinel pins ONLY the
	// write/validate half and does NOT query by UUID yet. Replace with the full
	// `uuid_indexable_and_roundtrips` round-trip when the read side lands.
	t.Run("uuid_index_create_and_insert_sites1_2", func(t *testing.T) {
		mwjoMustExec(t, db, ctx,
			"CREATE SCHEMA TEMPLATE idxty_uuid CREATE TABLE t (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id)) CREATE INDEX t_v ON t (v)")
		mwjoMustExec(t, db, ctx, "CREATE SCHEMA /testdb_idxty/suuid WITH TEMPLATE idxty_uuid")
		udb, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_idxty?cluster_file=%s&schema=suuid", clusterFilePath))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		t.Cleanup(func() { udb.Close() })
		// INSERT must succeed (the maintainer writes the UUID index entry without the
		// old leaky XX000 "message type" rejection).
		mwjoMustExec(t, udb, ctx,
			"INSERT INTO t (id, v) VALUES (1, '550e8400-e29b-41d4-a716-446655440000')")
	})
}
