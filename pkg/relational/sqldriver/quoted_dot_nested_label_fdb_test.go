package sqldriver_test

// A struct member declared with a dot in its name (`"a.b" BIGINT`) reads
// correctly through a derived table — the value is the member's — and its
// result-set LABEL is `b`, over the base table and through the derived table
// alike: the label is derived by qualifierStrippedLabel, whose declared
// limit is that a nested member is not a top-level field of any descriptor,
// so the dot inside its name is read as a qualifier and stripped (RFC-238 §2,
// `qualifier_stripped_label_test.go`, "nested field declared with a dot is
// invisible"). This is a NEGATIVE pin of that residual on the shape the
// derived-table arm admits now (a nested path decided by its shape): the
// value must stay right, and when the label reads `a.b` the residual is
// closed — re-pin both spellings to it. The aliased control labels as told.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_QuotedDotNestedMemberLabel(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_qdnl")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_qdnl")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE qdnl_tpl
		CREATE TYPE AS STRUCT qs ("a.b" BIGINT)
		CREATE TABLE tq (id BIGINT, s qs, PRIMARY KEY (id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_qdnl/s1 WITH TEMPLATE qdnl_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_qdnl?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO tq VALUES (1, (9))")

	read := func(t *testing.T, query string) (string, int64) {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%q: %v", query, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil || len(cols) != 1 {
			t.Fatalf("%q: columns=%v err=%v", query, cols, err)
		}
		if !rows.Next() {
			t.Fatalf("%q: no row", query)
		}
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("%q: scan: %v", query, err)
		}
		return cols[0], v
	}

	for _, spelling := range []string{
		`SELECT tq.s."a.b" FROM tq`,
		`SELECT x."a.b" FROM (SELECT tq.s."a.b" FROM tq) x`,
	} {
		label, v := read(t, spelling)
		if v != 9 {
			t.Fatalf("%q: value = %d, want the member's 9", spelling, v)
		}
		if label == "a.b" {
			t.Fatalf("%q: the label reads the member's own name %q — RFC-238's nested-member residual is closed; re-pin this spelling to it", spelling, label)
		}
		if label != "b" {
			t.Fatalf("%q: label = %q, want the residual's `b` (or `a.b` once closed)", spelling, label)
		}
	}
	label, v := read(t, `SELECT x.q FROM (SELECT tq.s."a.b" AS q FROM tq) x`)
	if v != 9 || label != "Q" {
		t.Fatalf("aliased control: label=%q value=%d, want Q / 9", label, v)
	}
}
