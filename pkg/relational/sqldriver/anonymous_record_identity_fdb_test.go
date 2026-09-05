package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_AnonymousRecordsThroughADerivedRowKeepDistinctIdentities pins that a
// record constructor's row published through a derived table or a CTE stays an
// ANONYMOUS record on the way back into the plan. The semantic column model
// carries a record's declared name in StructTypeName and nothing for an
// anonymous one; when the bridge back substituted the SQL kind "RECORD" as the
// name, two different anonymous shapes in one row claimed one descriptor, the
// synthesized result descriptor failed to compile, and the driver handed the
// array elements back as raw maps instead of structs — while the same two
// shapes at top level, never bridged, stamped fine. Every element below is an
// api.Struct; the top-level control beside them.
func TestFDB_AnonymousRecordsThroughADerivedRowKeepDistinctIdentities(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_anonrec")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_anonrec")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE anonrec_tpl
		CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_anonrec/s1 WITH TEMPLATE anonrec_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_anonrec?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1, 1)")

	const body = `SELECT (1 AS lat, 2 AS lon) AS s, (3 AS z) AS q FROM t`
	for _, query := range []string{
		`SELECT [x.s], [x.q] FROM (` + body + `) x`,
		`WITH x AS (` + body + `) SELECT [x.s], [x.q] FROM x`,
		`SELECT [x.s], [x.q] FROM (SELECT u.s, u.q FROM (` + body + `) u) x`,
		// The top-level control: the same two shapes, never bridged.
		`SELECT [s], [q] FROM (` + body + `) x`,
		`SELECT [(1 AS lat, 2 AS lon)], [(3 AS z)] FROM t`,
	} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		var a, b any
		if !rows.Next() {
			rows.Close()
			t.Fatalf("%s: no row: %v", query, rows.Err())
		}
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			t.Fatalf("%s: scan: %v", query, err)
		}
		rows.Close()
		for i, col := range []any{a, b} {
			elems, ok := col.([]any)
			if !ok || len(elems) != 1 {
				t.Fatalf("%s: column %d = %T %v, want a one-element array", query, i, col, col)
			}
			s, isStruct := elems[0].(api.Struct)
			if !isStruct {
				t.Fatalf("%s: column %d element = %T %v, want an api.Struct — an anonymous record lost its identity on the way through the derived row", query, i, elems[0], elems[0])
			}
			if n := s.AttributeCount(); n != 2-i {
				t.Fatalf("%s: column %d struct has %d attributes, want %d", query, i, n, 2-i)
			}
		}
	}
}
