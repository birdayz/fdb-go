package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
		// VALUES rows: the same two shapes minted by the inline-values retag,
		// which once named every row by the kind RECORD, at top level and
		// through a derived table.
		`SELECT [a.w], [b.v] FROM VALUES ((3, 4)) AS a(w(x, y)), VALUES ((5)) AS b(v(z))`,
		`SELECT [x.w], [x.v] FROM (SELECT a.w, b.v FROM VALUES ((3, 4)) AS a(w(x, y)), VALUES ((5)) AS b(v(z))) x`,
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

// TestFDB_ADeclaredRecordNameSurvivesTheBridge pins the other half of the same
// rule: a struct literal DECLARED with a name — even the name RECORD, which is
// also the SQL kind — keeps that name through a derived table, because the
// bridge carries a record's name unconditionally; treating the name RECORD as
// "anonymous" handed the literal back under a synthetic __type__ name after
// the bridge while the same literal at top level kept RECORD.
func TestFDB_ADeclaredRecordNameSurvivesTheBridge(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_namedrec")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_namedrec")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE namedrec_tpl
		CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_namedrec/s1 WITH TEMPLATE namedrec_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_namedrec?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1, 1)")

	for _, tc := range []struct {
		query      string
		wantName   string
		wantFields []string
	}{
		{`SELECT [x.s] FROM (SELECT STRUCT RECORD (1 AS lat, 2 AS lon) AS s FROM t) x`, "RECORD", []string{"LAT", "LON"}},
		{`WITH x AS (SELECT STRUCT RECORD (1 AS lat, 2 AS lon) AS s FROM t) SELECT [x.s] FROM x`, "RECORD", []string{"LAT", "LON"}},
		// The top-level control, never bridged.
		{`SELECT [STRUCT RECORD (1 AS lat, 2 AS lon)] FROM t`, "RECORD", []string{"LAT", "LON"}},
		// A named literal under a VALUES nested column definition: the
		// definition renames the fields and keeps the name (Java's
		// TypeUtils.setFieldNames); the retag once refused a named source.
		{`SELECT [a.w] FROM VALUES (STRUCT RECORD (3 AS p, 4 AS q)) AS a(w(x, y))`, "RECORD", []string{"X", "Y"}},
		{`SELECT [a.w] FROM VALUES (STRUCT foo (3 AS p, 4 AS q)) AS a(w(x, y))`, "FOO", []string{"X", "Y"}},
		{`SELECT [x.w] FROM (SELECT a.w FROM VALUES (STRUCT foo (3 AS p, 4 AS q)) AS a(w(x, y))) x`, "FOO", []string{"X", "Y"}},
		// An ARRAY of named records under the definition takes the retag's
		// shared array arm; measured, not inferred from the record arm.
		{`SELECT a.w FROM VALUES ([STRUCT foo (3 AS p, 4 AS q)]) AS a(w(x, y))`, "FOO", []string{"X", "Y"}},
		{`SELECT x.w FROM (SELECT a.w FROM VALUES ([STRUCT foo (3 AS p, 4 AS q)]) AS a(w(x, y))) x`, "FOO", []string{"X", "Y"}},
	} {
		query := tc.query
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		var col any
		if !rows.Next() {
			rows.Close()
			t.Fatalf("%s: no row: %v", query, rows.Err())
		}
		if err := rows.Scan(&col); err != nil {
			rows.Close()
			t.Fatalf("%s: scan: %v", query, err)
		}
		rows.Close()
		elems, ok := col.([]any)
		if !ok || len(elems) != 1 {
			t.Fatalf("%s: column = %T %v, want a one-element array", query, col, col)
		}
		s, isStruct := elems[0].(api.Struct)
		if !isStruct {
			t.Fatalf("%s: element = %T %v, want an api.Struct", query, elems[0], elems[0])
		}
		if name := s.MetaData().TypeName(); name != tc.wantName {
			t.Fatalf("%s: struct type name = %q, want the declared name %s — a declared record name was dropped", query, name, tc.wantName)
		}
		for i, want := range tc.wantFields {
			if got, err := s.MetaData().AttributeName(i + 1); err != nil || got != want {
				t.Fatalf("%s: attribute %d = %q (%v), want %q", query, i+1, got, err, want)
			}
		}
	}
}

// TestFDB_OneDeclaredNameOverTwoShapesIsRefused pins that two record literals
// declared under ONE name with TWO shapes in one row fail loudly — as Java's
// TypeRepository.build does on the duplicate message name — instead of coming
// back as raw maps with no error, which is what swallowing the synthesised
// descriptor's compile failure produced. Two distinct declared names beside
// them are structs.
func TestFDB_OneDeclaredNameOverTwoShapesIsRefused(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_samename")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_samename")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE samename_tpl
		CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_samename/s1 WITH TEMPLATE samename_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_samename?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1, 1)")

	for _, query := range []string{
		`SELECT [STRUCT foo (1 AS p)], [STRUCT foo (2 AS p, 3 AS q)] FROM t`,
		`SELECT [a.w], [b.v] FROM VALUES (STRUCT foo (1 AS p)) AS a(w(x)), VALUES (STRUCT foo (2 AS p, 3 AS q)) AS b(v(y, z))`,
		`SELECT [x.s], [x.q] FROM (SELECT STRUCT foo (1 AS p) AS s, STRUCT foo (2 AS p, 3 AS q) AS q FROM t) x`,
	} {
		rows, err := db.QueryContext(ctx, query)
		if err == nil {
			rows.Close()
			t.Fatalf("%s: planned; one declared name over two shapes must be refused, never handed back as raw maps", query)
		}
		if !strings.Contains(err.Error(), "XX000") || !strings.Contains(err.Error(), "result descriptor") {
			t.Fatalf("%s: failed for another reason than the descriptor compile: %v", query, err)
		}
	}

	// The control: two distinct declared names are two structs.
	const control = `SELECT [STRUCT foo (1 AS p)], [STRUCT bar (2 AS p, 3 AS q)] FROM t`
	rows, err := db.QueryContext(ctx, control)
	if err != nil {
		t.Fatalf("%s: %v", control, err)
	}
	var a, b any
	if !rows.Next() {
		rows.Close()
		t.Fatalf("%s: no row: %v", control, rows.Err())
	}
	if err := rows.Scan(&a, &b); err != nil {
		rows.Close()
		t.Fatalf("%s: scan: %v", control, err)
	}
	rows.Close()
	for i, col := range []any{a, b} {
		elems, ok := col.([]any)
		if !ok || len(elems) != 1 {
			t.Fatalf("%s: column %d = %T %v, want a one-element array", control, i, col, col)
		}
		s, isStruct := elems[0].(api.Struct)
		if !isStruct {
			t.Fatalf("%s: column %d element = %T, want an api.Struct", control, i, elems[0])
		}
		if want := []string{"FOO", "BAR"}[i]; s.MetaData().TypeName() != want {
			t.Fatalf("%s: column %d struct name = %q, want %s", control, i, s.MetaData().TypeName(), want)
		}
	}
}

// TestFDB_ADuplicateNameJoinRowStillReturnsEveryField pins the NEGATIVE the
// unstamped-rows booking rests on: a FULL OUTER JOIN over legs that both carry
// `ID` leaves its plan's computed rows without descriptors (the duplicate name
// makes the synthesised file unvalidatable, and the repository keeps the bad
// message so later types fail too), and that costs descriptor IDENTITY only.
// The emitting paths build dense positional rows and the result set reads them
// by ORDINAL, so both `ID` values still arrive.
//
// Without this, TODO.md's "A join row that names one field twice leaves its
// plan's rows unstamped" would read as a wrong-answer bug; it is not one. If a
// row here ever comes back short, the descriptor gap has become data loss and
// that entry is no longer latent.
func TestFDB_ADuplicateNameJoinRowStillReturnsEveryField(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dupjoin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dupjoin")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE dupjoin_tpl
		CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id))
		CREATE TABLE b_md (id BIGINT, v BIGINT, PRIMARY KEY (id))
		CREATE TABLE c_md (id BIGINT, PRIMARY KEY (id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dupjoin/s1 WITH TEMPLATE dupjoin_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_dupjoin?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a_md VALUES (1, 'x')")
	mwjoMustExec(t, db, ctx, "INSERT INTO b_md VALUES (1, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c_md VALUES (1)")

	const query = "WITH d AS (SELECT id AS bid, EXISTS (SELECT 1 FROM b_md AS x WHERE x.id = b_md.id) AS foo FROM b_md) " +
		"SELECT a.id, c.id, d.foo FROM a_md AS a JOIN d ON a.id = d.bid FULL OUTER JOIN c_md AS c ON a.id = c.id"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	var aID, cID int64
	var foo bool
	if err := rows.Scan(&aID, &cID, &foo); err != nil {
		t.Fatalf("scan: %v — both ID slots must arrive; the row is positional, and a short row "+
			"would mean the descriptor gap has become data loss", err)
	}
	if aID != 1 || cID != 1 || !foo {
		t.Fatalf("row = (%d, %d, %v), want (1, 1, true): every field of an unstamped row must still arrive", aID, cID, foo)
	}
}
