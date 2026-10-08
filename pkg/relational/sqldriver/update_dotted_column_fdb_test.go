package sqldriver_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_DMLOnDottedNames runs DML naming quoted identifiers that hold a dot,
// the shapes Java's valid-identifiers.yamsql uses.
//
//   - A dotted TABLE is one name. INSERT, UPDATE and DELETE resolved their
//     target by splitting the flattened name on '.', so INSERT INTO
//     "foo.tableA" was refused as the schema `foo` (42F00 "Unknown database
//     foo"); the targets now resolve from their parse-time segments, as a scan
//     does.
//   - A dotted COLUMN is one name, stored escaped (`"b.2"` as b__22). UPDATE
//     SET looked the column up by its raw text among the storage names, so it
//     was refused (42703) at the catalog check, and the planning check split it
//     on '.'; all three UPDATE lookups now match the user identifier.
//   - A dotted table's DML resolved its SET values and WHERE in a scope built
//     from the flattened name, split again on '.', so every UPDATE and every
//     DELETE with a WHERE failed (0AF00); the DML scopes now take the target's
//     single resolved segment. The sequence is Java's valid-identifiers.yamsql
//     update-delete-statements block (:509-525) without its RETURNING clauses,
//     with Java's counts.
func TestFDB_DMLOnDottedNames(t *testing.T) {
	t.Parallel()
	db := testkit.SetupErrorDB(t, "/TEST/DML_DOTTED", "S",
		`create table "foo.tableA"("foo.tableA.A1" bigint, "foo.tableA.A2" bigint, "foo.tableA.A3" bigint, primary key("foo.tableA.A1")) `+
			`create table tb(b1 bigint, "b.2" bigint, primary key(b1))`)
	ctx := context.Background()
	outcome := func(q string) string {
		res, err := db.ExecContext(ctx, q)
		if err != nil {
			var ae *api.Error
			if errors.As(err, &ae) {
				return "ERROR " + string(ae.Code) + " " + ae.Message
			}
			return "ERROR " + err.Error()
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("OK %d", n)
	}
	for _, c := range []struct{ q, want string }{
		{`INSERT INTO "foo.tableA" VALUES (1, 10, 1), (2, 20, 2), (3, 30, 3)`, "OK 3"},
		{`INSERT INTO tb VALUES (1, 10), (2, 20)`, "OK 2"},
		{`UPDATE tb SET "b.2" = 5 WHERE b1 = 1`, "OK 1"},
		{`UPDATE tb SET b1 = b1 WHERE "b.2" = 5`, "OK 1"},
		{`DELETE FROM tb WHERE "b.2" = 20`, "OK 1"},
		{`UPDATE "foo.tableA" SET "foo.tableA.A2" = 100 WHERE "foo.tableA.A1" = 1`, "OK 1"},
		{`UPDATE "foo.tableA" SET "foo.tableA.A2" = 100 WHERE "foo.tableA.A1" > 1`, "OK 2"},
	} {
		if got := outcome(c.q); got != c.want {
			t.Errorf("%s: %s, pinned %s", c.q, got, c.want)
		}
	}
	var b2 int64
	if err := db.QueryRowContext(ctx, `SELECT "b.2" FROM tb WHERE b1 = 1`).Scan(&b2); err != nil || b2 != 5 {
		t.Fatalf(`"b.2" after the update: %d, %v; want 5`, b2, err)
	}
	// Java's RETURNING of the next DELETE answers 102 for this row.
	var sum int64
	if err := db.QueryRowContext(ctx, `SELECT "foo.tableA.A1" + "foo.tableA.A2" + "foo.tableA.A3" FROM "foo.tableA" WHERE "foo.tableA.A1" = 1`).Scan(&sum); err != nil || sum != 102 {
		t.Fatalf("row 1's sum %d, %v; want 102", sum, err)
	}
	for _, c := range []struct{ q, want string }{
		{`DELETE FROM "foo.tableA" WHERE "foo.tableA.A1" = 1`, "OK 1"},
		{`DELETE FROM "foo.tableA" WHERE "foo.tableA.A2" = 100`, "OK 2"},
	} {
		if got := outcome(c.q); got != c.want {
			t.Errorf("%s: %s, want %s", c.q, got, c.want)
		}
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "foo.tableA"`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf(`rows of "foo.tableA": %d, %v; want none left`, rows, err)
	}
}
