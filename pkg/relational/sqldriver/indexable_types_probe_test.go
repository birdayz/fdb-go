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
	"fmt"
	"strings"
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

	t.Run("uuid_index_currently_xx000_KNOWN_GAP", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE idxty_uuid CREATE TABLE t (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id)) CREATE INDEX t_v ON t (v)")
		if err == nil {
			t.Errorf("CREATE INDEX on UUID unexpectedly succeeded — the UUID-index gap may be FIXED; " +
				"flip this sentinel + update TODO.md")
			return
		}
		// CURRENT (leaky) behavior: internal XX000 "message type". When fixed, this is
		// either no error (indexable, matching Java) or a clean user SQLSTATE.
		if !strings.Contains(err.Error(), "XX000") {
			t.Errorf("UUID index error = %v; was the leaky XX000 replaced with a clean error? "+
				"update this sentinel + TODO.md", err)
		}
	})
}
