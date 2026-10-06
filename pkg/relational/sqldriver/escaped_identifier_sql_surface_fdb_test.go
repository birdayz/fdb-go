package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// A table or column whose DDL name needs ProtoUtils escaping ('$', '.', or
// "__") must remain addressable from SQL under the name the user wrote.
//
// Java carries BOTH names on the type: Type.Record.fromDescriptorPreservingName
// stores `ProtoUtils.toUserIdentifier(descriptor.getName())` as the name and
// the raw descriptor name as the storage name (Type.java:2591-2593), and
// Type.Record.Field does the same per column (Type.java:2874-2877). The wire
// keeps the escaped spelling; the SQL surface sees the user's.
//
// Go collapsed the two: RecordMetaData is the relational table catalog, so its
// record types are keyed by STORAGE name while SQL text spells the USER name.
// Two failures followed, and the second is only reachable once the first is
// fixed — which is why they are pinned together:
//
//  1. the table was unreachable (42F01) even though it existed on the wire;
//  2. with the table reachable, an explicit INSERT column list compared the
//     user name against the descriptor name, matched NOTHING, fell through to
//     the NULL-fill arm, and the INSERT SUCCEEDED — writing NULL over the
//     value the caller supplied. Silent data loss, no error.
func TestFDB_EscapedIdentifierSQLSurface(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/FRL/escapedident")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /FRL/escapedident"); err != nil {
		t.Fatalf("db: %v", err)
	}
	// Both escape classes on both kinds of identifier: '$' and '.' in a table
	// name, '$' in a column name. (A '.' in a column name is finding-6
	// territory for PRIMARY KEY parsing and is pinned there.)
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE esc_tmpl "+
			`CREATE TABLE "foo$table" (id BIGINT, "a$b" BIGINT, plain BIGINT, PRIMARY KEY (id)) `+
			`CREATE TABLE "dot.table" (id BIGINT, v BIGINT, PRIMARY KEY (id))`); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /FRL/escapedident/s WITH TEMPLATE esc_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/ESCAPEDIDENT?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	t.Run("escaped_table_is_addressable", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `INSERT INTO "foo$table" VALUES (1, 7, 8)`); err != nil {
			t.Fatalf(`INSERT into an escaped-name table failed: %v
The table exists on the wire (stored as FOO__1TABLE) but SQL cannot reach it:
the record-type lookup is addressing the metadata with the USER identifier and
no longer resolves it through the ProtoUtils escape.`, err)
		}
		var id, ab, plain int64
		if err := db.QueryRowContext(ctx, `SELECT id, "a$b", plain FROM "foo$table" WHERE id = 1`).
			Scan(&id, &ab, &plain); err != nil {
			t.Fatalf(`SELECT from an escaped-name table failed: %v`, err)
		}
		if id != 1 || ab != 7 || plain != 8 {
			t.Fatalf("read back (%d,%d,%d), want (1,7,8)", id, ab, plain)
		}
	})

	// A table whose name contains a QUOTED DOT is one name. It was split into
	// (qualifier, name) before any escape translation, so `"dot.table"` read as
	// database `dot`, table `table` and died 42F00; DML targets now resolve
	// from their parse-time segments, as a scan does, and the DML scopes that
	// resolve SET values and the WHERE take the one resolved segment
	// (TestFDB_DMLOnDottedNames runs Java's dotted-table DML sequence).
	t.Run("dotted_table_name_is_one_name", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `INSERT INTO "dot.table" VALUES (1, 5)`); err != nil {
			t.Fatalf("INSERT into a quoted-dot table: %v", err)
		}
		var v int64
		if err := db.QueryRowContext(ctx, `SELECT v FROM "dot.table" WHERE id = 1`).Scan(&v); err != nil || v != 5 {
			t.Fatalf("read back %d, %v; want 5", v, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE "dot.table" SET v = 6 WHERE id = 1`); err != nil {
			t.Fatalf("UPDATE a quoted-dot table: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT v FROM "dot.table" WHERE id = 1`).Scan(&v); err != nil || v != 6 {
			t.Fatalf("after the update %d, %v; want 6", v, err)
		}
		res, err := db.ExecContext(ctx, `DELETE FROM "dot.table" WHERE id = 1`)
		if err != nil {
			t.Fatalf("DELETE from a quoted-dot table: %v", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("DELETE from a quoted-dot table affected %d, %v; want 1", n, err)
		}
		var left int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "dot.table" WHERE id = 1`).Scan(&left); err != nil || left != 0 {
			t.Fatalf("after the delete %d rows, %v; want 0", left, err)
		}
	})

	// THE DATA-LOSS PIN. An explicit column list naming the escaped column
	// must store the supplied value — never silently NULL-fill it. Two rows
	// are not needed; what is needed is reading the value back, because the
	// defect's whole signature is a statement that SUCCEEDS with the wrong
	// stored value.
	t.Run("explicit_column_list_stores_the_value_not_null", func(t *testing.T) {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO "foo$table" (id, "a$b") VALUES (2, 7)`); err != nil {
			t.Fatalf(`INSERT with an explicit escaped column list failed: %v`, err)
		}
		var ab sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT "a$b" FROM "foo$table" WHERE id = 2`).Scan(&ab); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !ab.Valid {
			t.Fatal(`INSERT INTO "foo$table" (id, "a$b") VALUES (2, 7) stored NULL in "a$b".
The explicit column list is being matched against the DESCRIPTOR name (A__1B)
instead of the USER name ("a$b"), so the column matched nothing, fell through
to the NULL-fill arm, and the supplied 7 was silently discarded. This is data
loss with no error — the statement reports success.`)
		}
		if ab.Int64 != 7 {
			t.Fatalf(`"a$b" = %d, want 7`, ab.Int64)
		}
	})

	// The user-name match must not change Java's handling of a column name
	// that matches NO field: Java iterates the TARGET fields and looks each
	// up in the named list, so an unmatched NAME is silently ignored along
	// with its value (ExpressionVisitor.parseRecordFieldsUnderReorderings —
	// indexOf never finds it; the corpus's composite-aggregates.yamsql
	// inserts into T2(…, COL3) on a 3-column T2 and Java accepts). Pinned so
	// the escaped-name match cannot be "fixed" into a stricter rejection
	// that would diverge from Java on a shape the corpus exercises.
	t.Run("unmatched_column_name_ignored_as_java_does", func(t *testing.T) {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO "foo$table" (id, nosuchcol) VALUES (3, 1)`); err != nil {
			t.Fatalf(`Java accepts an explicit column list naming a non-field and ignores it; Go rejected: %v`, err)
		}
		var plain sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT plain FROM "foo$table" WHERE id = 3`).Scan(&plain); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if plain.Valid {
			t.Fatalf("plain = %d, want NULL (the ignored name supplied no value for any field)", plain.Int64)
		}
	})
}
