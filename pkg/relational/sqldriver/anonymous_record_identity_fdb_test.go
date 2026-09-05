package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
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

// duplicateNameJoinQuery is the shape whose ordinal row names `ID` twice: a
// FULL OUTER JOIN over legs that both carry it. The join predicate is
// deliberately `a.id + 1 = c.id` over ids 1 and 2, so the two `ID` slots hold
// DIFFERENT values and both outer sides null-extend — with the slots equal a
// test cannot tell a preserved pair from one slot read twice.
//
// TestFinalizePlanLeavesTheDuplicateNameJoinRowUnstamped plans this same text
// and asserts the census that is this file's precondition: the two must stay
// identical, or these tests silently stop describing one plan.
const duplicateNameJoinQuery = "WITH d AS (SELECT id AS bid, EXISTS (SELECT 1 FROM b_md AS x WHERE x.id = b_md.id) AS foo FROM b_md) " +
	"SELECT a.id, c.id, d.foo FROM a_md AS a JOIN d ON a.id = d.bid FULL OUTER JOIN c_md AS c ON a.id + 1 = c.id"

// TestFDB_ADuplicateNameJoinRowLosesItsStructTypeNotItsValues pins what the
// poisoned repository costs, in both directions and with values, not shapes.
//
// A row that names one field twice cannot be given a descriptor, and the
// repository keeps the bad message, so constructors resolved after it lose
// theirs too. That is USER-VISIBLE: a COMPUTED struct selected through such a
// plan comes back as a raw `map[string]any`, where the SAME join with the
// repeated name removed returns an `api.Struct` carrying the same values. The
// control changes only the name — it keeps the join — so the raw map is
// attributable to the duplicate rather than to joining at all.
//
// What it does NOT cost is data: the emitting paths build dense positional rows
// the result set reads by ORDINAL, so the whole outer-join result arrives, both
// `ID` slots included. Both halves are asserted here because each bounds the
// other — without the first the entry reads as harmless, without the second as
// a wrong-answer bug.
//
// A STORED struct column read through the same poisoned plan is unaffected: it
// carries its own stored descriptor rather than a constructor's, so it stays an
// `api.Struct`. That bounds the blast radius to COMPUTED rows.
//
// TODO.md, "A join row that names one field twice leaves its plan's rows
// unstamped", carries the closure. When it lands the computed half reddens: the
// struct comes back an api.Struct and this test must then assert that.
func TestFDB_ADuplicateNameJoinRowLosesItsStructTypeNotItsValues(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dupjoin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dupjoin")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE dupjoin_tpl
		CREATE TYPE AS STRUCT st_s (p BIGINT)
		CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id))
		CREATE TABLE b_md (id BIGINT, v BIGINT, PRIMARY KEY (id))
		CREATE TABLE c_md (id BIGINT, PRIMARY KEY (id))
		CREATE TABLE s_md (id BIGINT, r st_s, PRIMARY KEY (id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dupjoin/s1 WITH TEMPLATE dupjoin_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_dupjoin?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a_md VALUES (1, 'x'), (2, 'y')")
	mwjoMustExec(t, db, ctx, "INSERT INTO b_md VALUES (1, 10), (2, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c_md VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO s_md VALUES (1, (7))")

	// Half one: a COMPUTED struct loses its type, keeping its values, while a
	// STORED struct column in the SAME statement keeps both. Reading them
	// together is what witnesses the poisoning for the stored assertion: if the
	// shape ever stops being poisoned, `d.r` becomes a struct and this fails
	// rather than passing as a bound on the damage. The control below differs
	// from the poisoned query only in whether a leg repeats `ID`.
	const computedThroughTheDuplicate = "WITH d AS (SELECT id AS bid, STRUCT foo (id AS x, v AS y) AS r FROM b_md) " +
		"SELECT d.r FROM a_md AS a JOIN d ON a.id = d.bid FULL OUTER JOIN c_md AS c ON a.id + 1 = c.id"
	const computedWithoutTheDuplicate = "WITH d AS (SELECT id AS bid, STRUCT foo (id AS x, v AS y) AS r FROM b_md) " +
		"SELECT d.r FROM a_md AS a JOIN d ON a.id = d.bid FULL OUTER JOIN (SELECT id AS cid FROM c_md) AS c ON a.id + 1 = c.cid"

	poisoned, stored := computedAndStoredThroughTheDuplicate(t, db, ctx)
	if _, isStruct := poisoned.(api.Struct); isStruct {
		t.Fatalf("the computed STRUCT through the duplicate-name join came back as %T — it is an "+
			"api.Struct now, so TODO.md's booking has closed: assert that here instead", poisoned)
	}
	asMap, isMap := poisoned.(map[string]any)
	if !isMap {
		t.Fatalf("the computed STRUCT through the duplicate-name join = %T %v, want the raw map "+
			"the missing descriptor forces", poisoned, poisoned)
	}
	if asMap["X"] != int64(1) || asMap["Y"] != int64(10) {
		t.Fatalf("the raw map = %#v, want {X:1 Y:10}: the type is lost, the VALUES are not — "+
			"a map with the wrong contents is a different (worse) defect", asMap)
	}

	clean := firstNonNilColumn(t, db, ctx, computedWithoutTheDuplicate)
	cleanStruct, isStruct := clean.(api.Struct)
	if !isStruct {
		t.Fatalf("the control STRUCT (same join, no repeated name) = %T %v, want an api.Struct — "+
			"without this the first half cannot attribute the raw map to the duplicate name "+
			"rather than to joining at all", clean, clean)
	}
	for name, want := range map[string]any{"X": int64(1), "Y": int64(10)} {
		got, err := cleanStruct.AttributeByName(name)
		if err != nil || got != want {
			t.Fatalf("control struct %s = %#v (%v), want %v: both representations must carry the "+
				"same values, or this is not a type-only difference", name, got, err, want)
		}
	}

	// The STORED column from that same row keeps its type.
	storedStruct, isStruct := stored.(api.Struct)
	if !isStruct {
		t.Fatalf("a STORED struct column through the poisoned join = %T %v, want an api.Struct: it "+
			"carries its own stored descriptor, so the blast radius is COMPUTED rows only — if this "+
			"fails, it is wider than TODO.md says", stored, stored)
	}
	if got, err := storedStruct.AttributeByName("P"); err != nil || got != int64(7) {
		t.Fatalf("stored struct P = %#v (%v), want 7", got, err)
	}

	// Half two: the whole outer-join result arrives, exactly.
	rows, err := db.QueryContext(ctx, duplicateNameJoinQuery)
	if err != nil {
		t.Fatalf("%s: %v", duplicateNameJoinQuery, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var aID, cID sql.NullInt64
		var foo sql.NullBool
		if err := rows.Scan(&aID, &cID, &foo); err != nil {
			t.Fatalf("scan: %v — every slot of an unstamped row must still arrive", err)
		}
		got = append(got, fmt.Sprintf("(%v,%v,%v)", nullInt(aID), nullInt(cID), nullBool(foo)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	want := []string{"(1,2,true)", "(2,NULL,true)", "(NULL,1,NULL)"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outer-join rows = %v, want %v: the two `ID` slots hold different values and both "+
			"outer sides null-extend, so a collapsed slot or a dropped row shows up here", got, want)
	}
}

func nullInt(v sql.NullInt64) string {
	if !v.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", v.Int64)
}

func nullBool(v sql.NullBool) string {
	if !v.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%v", v.Bool)
}

// firstNonNilColumn runs query and returns the first non-NULL first column it
// finds, so a null-extended outer row cannot stand in for the row under test.
func firstNonNilColumn(t *testing.T, db *sql.DB, ctx context.Context, query string) any {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		var value any
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		if value != nil {
			return value
		}
	}
	t.Fatalf("%s: no row with a non-NULL first column: %v", query, rows.Err())
	return nil
}

// computedAndStoredThroughTheDuplicate reads a COMPUTED struct and a STORED
// struct column out of ONE row of the duplicate-name join, and returns them in
// that order.
//
// One statement rather than two, because the stored assertion needs a witness
// that its own plan is poisoned: the computed value beside it is that witness.
// Read separately, "a stored column keeps its api.Struct" would stay green the
// day the shape stops being poisoned and would still read as a bound on the
// damage.
func computedAndStoredThroughTheDuplicate(t *testing.T, db *sql.DB, ctx context.Context) (computed, stored any) {
	t.Helper()
	const query = "WITH d AS (SELECT id AS bid, STRUCT foo (id AS x, v AS y) AS r FROM b_md) " +
		"SELECT d.r, s.r FROM s_md AS s JOIN d ON s.id = d.bid FULL OUTER JOIN c_md AS c ON s.id + 1 = c.id"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		var computedValue, storedValue any
		if err := rows.Scan(&computedValue, &storedValue); err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		if computedValue != nil && storedValue != nil {
			return computedValue, storedValue
		}
	}
	t.Fatalf("%s: no row carried both a computed and a stored struct: %v", query, rows.Err())
	return nil, nil
}
