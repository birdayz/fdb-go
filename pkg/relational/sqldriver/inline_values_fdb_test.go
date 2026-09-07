package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func inlineValuesDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	const dbPath = "/testdb_inline_values"
	setup := openTestDB(t, dbPath)
	for _, statement := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE inline_values_tmpl " +
			"CREATE TABLE anchor (id BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA " + dbPath + "/main WITH TEMPLATE inline_values_tmpl",
	} {
		if _, err := setup.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open inline VALUES schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, ctx
}

func inlineValuesExplain(t *testing.T, db *sql.DB, ctx context.Context, statement string) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+statement).Scan(&plan); err != nil {
		return "<explain failed: " + err.Error() + ">"
	}
	return plan
}

func inlineValuesIntRows(t *testing.T, db *sql.DB, ctx context.Context, statement string, width int) [][]int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		t.Fatalf("query %q: %v\nplan: %s", statement, err, inlineValuesExplain(t, db, ctx, statement))
	}
	defer rows.Close()
	result := make([][]int64, 0)
	for rows.Next() {
		row := make([]int64, width)
		dest := make([]any, width)
		for i := range row {
			dest[i] = &row[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan %q: %v", statement, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %q: %v", statement, err)
	}
	return result
}

func TestFDB_InlineValuesExactExecution(t *testing.T) {
	t.Parallel()
	db, ctx := inlineValuesDB(t)

	t.Run("projectionless_star", func(t *testing.T) {
		const statement = `SELECT * FROM VALUES (42)`
		got := inlineValuesIntRows(t, db, ctx, statement, 1)
		want := [][]int64{{42}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v\nplan: %s", got, want, inlineValuesExplain(t, db, ctx, statement))
		}
	})

	t.Run("standalone", func(t *testing.T) {
		const statement = `SELECT "id" FROM VALUES (1), (2) AS "values" ("id") ORDER BY "id"`
		got := inlineValuesIntRows(t, db, ctx, statement, 1)
		want := [][]int64{{1}, {2}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v\nplan: %s", got, want, inlineValuesExplain(t, db, ctx, statement))
		}
	})

	t.Run("nested_definition_common_type", func(t *testing.T) {
		const statement = `SELECT B, C, W.X, W.Y, W.Z ` +
			`FROM VALUES (1, 2.0, (3, 4, 'foo')), (10, 90.2, (5, 6.0, 'bar')) ` +
			`AS A(B, C, W(X, Y, Z))`
		rows, err := db.QueryContext(ctx, statement)
		if err != nil {
			t.Fatalf("query nested inline VALUES: %v\nplan: %s", err, inlineValuesExplain(t, db, ctx, statement))
		}
		defer rows.Close()
		var got [][]any
		for rows.Next() {
			var b, x int64
			var c, y float64
			var z string
			if err := rows.Scan(&b, &c, &x, &y, &z); err != nil {
				t.Fatalf("scan nested inline VALUES: %v", err)
			}
			got = append(got, []any{b, c, x, y, z})
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate nested inline VALUES: %v", err)
		}
		want := [][]any{
			{int64(1), float64(2), int64(3), float64(4), "foo"},
			{int64(10), float64(90.2), int64(5), float64(6), "bar"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("nested inline rows = %#v, want %#v\nplan: %s", got, want, inlineValuesExplain(t, db, ctx, statement))
		}
	})

	t.Run("quoted_nested_definition", func(t *testing.T) {
		const statement = `SELECT "a"."w"."x", "a"."w"."y", "a"."w"."z" ` +
			`FROM VALUES ((3, 4, 'foo')), ((5, 6.0, 'bar')) AS "a" ("w"("x", "y", "z"))`
		rows, err := db.QueryContext(ctx, statement)
		if err != nil {
			t.Fatalf("query quoted nested inline VALUES: %v\nplan: %s", err, inlineValuesExplain(t, db, ctx, statement))
		}
		defer rows.Close()
		var got [][]any
		for rows.Next() {
			var x int64
			var y float64
			var z string
			if err := rows.Scan(&x, &y, &z); err != nil {
				t.Fatalf("scan quoted nested inline VALUES: %v", err)
			}
			got = append(got, []any{x, y, z})
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate quoted nested inline VALUES: %v", err)
		}
		want := [][]any{
			{int64(3), float64(4), "foo"},
			{int64(5), float64(6), "bar"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("quoted nested inline rows = %#v, want %#v\nplan: %s", got, want, inlineValuesExplain(t, db, ctx, statement))
		}
	})

	t.Run("array_of_nested_records", func(t *testing.T) {
		const statement = `SELECT W FROM VALUES ([('a', 'b', [1, 2, 3])]) AS A(W(X, Y, Z))`
		var raw any
		if err := db.QueryRowContext(ctx, statement).Scan(&raw); err != nil {
			t.Fatalf("query array-of-record inline VALUES: %v\nplan: %s", err, inlineValuesExplain(t, db, ctx, statement))
		}
		array, ok := raw.([]any)
		if !ok || len(array) != 1 {
			t.Fatalf("array-of-record value = %T %#v, want one element", raw, raw)
		}
		record, ok := array[0].(api.Struct)
		if !ok {
			t.Fatalf("array element = %T, want api.Struct", array[0])
		}
		// An anonymous record's public name is the synthesized one — Java's
		// ProtoUtils.uniqueTypeName spelling, as a record constructor's row
		// reports it. The inline-values retag once minted the SQL kind RECORD as
		// the name instead, and every VALUES record then shared one descriptor
		// name (two shapes in one row could not compile into a result
		// descriptor and came back as raw maps).
		if name := record.MetaData().TypeName(); !strings.HasPrefix(name, "__0type__") {
			t.Fatalf("array element type name = %q, want the synthesized anonymous name", name)
		}
		for ordinal, wantName := range []string{"X", "Y", "Z"} {
			if gotName, err := record.MetaData().AttributeName(ordinal + 1); err != nil || gotName != wantName {
				t.Fatalf("array element field %d = %q (%v), want %q", ordinal, gotName, err, wantName)
			}
		}
		if got, err := record.AttributeByName("X"); err != nil || got != "a" {
			t.Fatalf("array element X = %#v (%v), want a", got, err)
		}
		if got, err := record.AttributeByName("Z"); err != nil || !reflect.DeepEqual(got, []any{int64(1), int64(2), int64(3)}) {
			t.Fatalf("array element Z = %#v (%v), want [1 2 3]", got, err)
		}
	})

	t.Run("comma_lateral_as_at", func(t *testing.T) {
		const statement = `SELECT "values"."id", "val", "at" ` +
			`FROM VALUES (1, [101]), (2, [201, 202, 203]) AS "values" ("id", "arr"), ` +
			`"values"."arr" AS "val" AT "at"`
		got := inlineValuesIntRows(t, db, ctx, statement, 3)
		want := [][]int64{{1, 101, 1}, {2, 201, 1}, {2, 202, 2}, {2, 203, 3}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v\nplan: %s", got, want, inlineValuesExplain(t, db, ctx, statement))
		}
	})

	for name, expression := range map[string]string{
		"null_array":  `CAST(NULL AS INTEGER ARRAY)`,
		"empty_array": `CAST([] AS INTEGER ARRAY)`,
	} {
		t.Run(name, func(t *testing.T) {
			statement := fmt.Sprintf(`SELECT "id", "val", "at" `+
				`FROM VALUES (1, %s) AS "values" ("id", "arr"), `+
				`"values"."arr" AS "val" AT "at"`, expression)
			if got := inlineValuesIntRows(t, db, ctx, statement, 3); len(got) != 0 {
				t.Fatalf("rows = %v, want none\nplan: %s", got, inlineValuesExplain(t, db, ctx, statement))
			}
		})
	}

	t.Run("scalar_owner_is_not_an_array", func(t *testing.T) {
		assertErrorCode(t, db,
			`SELECT "val" FROM VALUES (1) AS "values" ("id"), `+
				`"values"."id" AS "val" AT "at"`,
			api.ErrCodeInvalidColumnReference)
	})
	t.Run("duplicate_owner_alias_is_attribute_ambiguous", func(t *testing.T) {
		// An inline VALUES source is an ordinary FROM relation for duplicate-
		// alias purposes. As with two same-aliased base/derived relations, the
		// sources receive distinct private bindings and a column carried by both
		// is rejected per attribute as 42702. The stricter 42712 declaration gate
		// is reserved for a lateral unnest's shadowing AS/AT aliases.
		assertErrorCode(t, db,
			`SELECT "values"."id" FROM VALUES (1) AS "values" ("id"), `+
				`VALUES (2) AS "values" ("id")`,
			api.ErrCodeAmbiguousColumn)
	})
	t.Run("column_alias_width", func(t *testing.T) {
		assertErrorCode(t, db,
			`SELECT "id" FROM VALUES (1, 2) AS "values" ("id")`,
			api.ErrCodeSyntaxError)
	})
}
