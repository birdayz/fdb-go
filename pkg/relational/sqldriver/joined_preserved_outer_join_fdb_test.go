package sqldriver_test

// RFC-150 Phase-2b Piece-2 follow-up — FDB row-level proof that a LEFT OUTER whose
// preserved side is a JOIN, correlated to C through a BURIED preserved source alias,
// produces correct rows (matched + LEFT-OUTER null-extended) via the C index-probe
// FlatMap (codex P2 on #364). The embedded test pins the typed plan shape; this pins
// execution correctness.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_JoinedPreservedSide_LeftOuterRows seeds A⋈B and a partially-matching C, then
// runs `A JOIN B ON B.a_id=A.id LEFT JOIN C ON C.a_id=A.id`: every A⋈B row appears
// once, with C's column null-extended where C has no match. Proves the rewritten
// joined-preserved probe plan executes the LEFT-OUTER semantics correctly.
func TestFDB_JoinedPreservedSide_LeftOuterRows(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_jps")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_jps")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE jps "+
			"CREATE TABLE a (id BIGINT NOT NULL, flag BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_jps/s WITH TEMPLATE jps")
	dsn := fmt.Sprintf("fdbsql:///testdb_jps?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 0), (2, 0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1), (11, 2)") // each A has a B match
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (100, 1)")         // only A=1 has a C match

	rows, err := db.QueryContext(ctx,
		"SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.a_id = a.id ORDER BY a.id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		aID int64
		cID sql.NullInt64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.aID, &r.cID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// A=1 matches C=100; A=2 has no C → null-extended. Both A⋈B rows present.
	want := []row{
		{1, sql.NullInt64{Int64: 100, Valid: true}},
		{2, sql.NullInt64{Valid: false}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].aID != want[i].aID || got[i].cID != want[i].cID {
			t.Errorf("row %d = {a:%d c:%v}, want {a:%d c:%v}", i, got[i].aID, got[i].cID, want[i].aID, want[i].cID)
		}
	}
}
