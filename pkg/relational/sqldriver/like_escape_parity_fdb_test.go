package sqldriver_test

// LIKE ... ESCAPE trailing-escape parity, on BOTH evaluator paths.
//
// The pattern `'Z' ESCAPE 'Z'` ends with the escape character and
// nothing follows it. Java's answer is recorded in its own corpus,
// fdb-record-layer/yaml-tests/src/test/resources/like.yamsql:92:
//
//	- query: select * from B WHERE B2 NOT LIKE 'Z' ESCAPE 'Z'
//	  # This should error; see .../fdb-record-layer/issues/3216
//	- result: [{1, 'Y'}, {3, 'A'}, {5, 'B'}]
//
// B holds (2,'Z') and (4,'Z'); both are excluded by NOT LIKE, so
// Java evaluates `'Z' LIKE 'Z' ESCAPE 'Z'` as TRUE — a dangling
// escape is matched as a literal. That falls out of
// PatternForLikeValue's regex translation: the escape+`_` and
// escape+`%` entries cannot fire on a dangling escape, so the
// generic `\` → `\\` REPLACE_MAP entry emits the escape rune as a
// literal. Java's own comment concedes the SQL standard would have
// it raise 22025, but the pinned 4.12.11.0 behaviour is the literal
// match, and "doesn't work in Java → doesn't work in Go, in the same
// architectural way" cuts both directions.
//
// Two Go paths must agree with that and with each other:
//   - the ENGINE path (Cascades → predicates.likeMatch → values.LikeMatch)
//   - the MAP path (INFORMATION_SCHEMA WHERE → filterSysRows →
//     evalPredicateOnMapTri)
//
// Both are pinned here because the defect being guarded is precisely
// that the two paths used different matchers; a unit test on either
// helper cannot express it.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_LikeTrailingEscape_EnginePath pins the Cascades WHERE
// evaluator against Java's like.yamsql:92 answer.
func TestFDB_LikeTrailingEscape_EnginePath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_like_esc_engine")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_like_esc_engine")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE like_esc_engine_tmpl "+
		"CREATE TABLE B (B1 BIGINT NOT NULL, B2 STRING, PRIMARY KEY (B1))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_like_esc_engine/s WITH TEMPLATE like_esc_engine_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_like_esc_engine?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mustExec(t, db, ctx, "INSERT INTO B VALUES (1, 'Y'), (2, 'Z'), (3, 'A'), (4, 'Z'), (5, 'B')")

	// Java: `'Z' LIKE 'Z' ESCAPE 'Z'` is TRUE — the dangling escape
	// matches the escape rune literally.
	got := queryInt64s(t, ctx, db, "SELECT B1 FROM B WHERE B2 LIKE 'Z' ESCAPE 'Z' ORDER BY B1")
	if want := []int64{2, 4}; !equalInt64s(got, want) {
		t.Fatalf("LIKE 'Z' ESCAPE 'Z' returned %v, want %v "+
			"(Java like.yamsql:92 — a dangling escape matches literally)", got, want)
	}

	// The NOT form is the shape Java's corpus actually records.
	got = queryInt64s(t, ctx, db, "SELECT B1 FROM B WHERE B2 NOT LIKE 'Z' ESCAPE 'Z' ORDER BY B1")
	if want := []int64{1, 3, 5}; !equalInt64s(got, want) {
		t.Fatalf("NOT LIKE 'Z' ESCAPE 'Z' returned %v, want %v (Java like.yamsql:92)", got, want)
	}

	// A dangling escape mid-string still requires the literal rune to
	// be present: pattern `A_Z` with escape `Z` is `A`, any-char, then
	// a literal `Z`. Guards against "trailing escape" being special-
	// cased into a blanket accept.
	mustExec(t, db, ctx, "INSERT INTO B VALUES (6, 'AxZ'), (7, 'Ax')")
	got = queryInt64s(t, ctx, db, "SELECT B1 FROM B WHERE B2 LIKE 'A_Z' ESCAPE 'Z' ORDER BY B1")
	if want := []int64{6}; !equalInt64s(got, want) {
		t.Fatalf("LIKE 'A_Z' ESCAPE 'Z' returned %v, want %v", got, want)
	}
}

// TestFDB_LikeTrailingEscape_MapPath pins the INFORMATION_SCHEMA
// WHERE evaluator (the map path) on the same input. Before the
// shadow-evaluator retirement this path ran a second, independent
// LikeMatch; the two disagreed on exactly this pattern.
func TestFDB_LikeTrailingEscape_MapPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_like_esc_map")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_like_esc_map")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE like_esc_map_tmpl "+
		"CREATE TABLE Z (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
		"CREATE TABLE ZQ (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_like_esc_map/s WITH TEMPLATE like_esc_map_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_like_esc_map?cluster_file=%s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// INFORMATION_SCHEMA.TABLES spans the whole cluster, which other
	// parallel tests also write to — scope every probe to this test's
	// own catalog so the row set is deterministic.
	const scope = `TABLE_CATALOG = '/testdb_like_esc_map' AND `

	// TABLE_NAME 'Z' vs pattern 'Z' ESCAPE 'Z': the dangling escape is
	// a literal `Z`, so 'Z' matches and 'ZQ' does not.
	got := queryStrings(t, ctx, db,
		`SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES" `+
			`WHERE `+scope+`TABLE_NAME LIKE 'Z' ESCAPE 'Z' ORDER BY TABLE_NAME`)
	if want := []string{"Z"}; !likeEscEqualStrings(got, want) {
		t.Fatalf("INFORMATION_SCHEMA LIKE 'Z' ESCAPE 'Z' returned %v, want %v "+
			"(must agree with the engine path and with Java like.yamsql:92)", got, want)
	}

	// NOT form, same row set complemented.
	got = queryStrings(t, ctx, db,
		`SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES" `+
			`WHERE `+scope+`TABLE_NAME NOT LIKE 'Z' ESCAPE 'Z' ORDER BY TABLE_NAME`)
	if want := []string{"ZQ"}; !likeEscEqualStrings(got, want) {
		t.Fatalf("INFORMATION_SCHEMA NOT LIKE 'Z' ESCAPE 'Z' returned %v, want %v", got, want)
	}

	// Escaped wildcard on the map path: `Z_` with escape `Z` is a
	// literal underscore, matching neither table. Pins that routing
	// the map path through the engine matcher kept ordinary escape
	// handling intact.
	got = queryStrings(t, ctx, db,
		`SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES" `+
			`WHERE `+scope+`TABLE_NAME LIKE 'Z_' ESCAPE 'Z' ORDER BY TABLE_NAME`)
	if len(got) != 0 {
		t.Fatalf("INFORMATION_SCHEMA LIKE 'Z_' ESCAPE 'Z' returned %v, want no rows "+
			"(escaped `_` is a literal underscore)", got)
	}
}

func queryInt64s(t *testing.T, ctx context.Context, db *sql.DB, q string) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func queryStrings(t *testing.T, ctx context.Context, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func likeEscEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
