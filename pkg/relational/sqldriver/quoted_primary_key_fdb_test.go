package sqldriver_test

import (
	"context"
	"strings"
	"testing"
)

// A quoted lowercase primary-key column is scanned by key and answers the
// target's rows (WS-F oracle w13_quoted_pk_*).
func TestFDB_QuotedPrimaryKeyColumnScan(t *testing.T) {
	t.Parallel()
	db := setupPlanShapeDB(t, "qpk", `CREATE TABLE "footab" ("id" BIGINT, v BIGINT, PRIMARY KEY ("id"))`)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO "footab" VALUES (1, 10), (2, 20), (3, 30)`); err != nil {
		t.Fatal(err)
	}
	var plan string
	if err := db.QueryRowContext(ctx, `EXPLAIN SELECT v FROM "footab" WHERE "id" = 2`).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "Scan(footab, [=])") {
		t.Fatalf("plan %s, want a primary-key equality scan", plan)
	}
	for _, c := range []struct {
		sql  string
		want []int64
	}{
		{`SELECT v FROM "footab" WHERE "id" = 2`, []int64{20}},
		{`SELECT v FROM "footab" WHERE "id" > 1 ORDER BY "id"`, []int64{20, 30}},
		{`SELECT v FROM "footab" WHERE "id" = 4`, nil},
	} {
		rows, err := db.QueryContext(ctx, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		var got []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			got = append(got, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if len(got) != len(c.want) {
			t.Fatalf("%s: rows %v, want %v", c.sql, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: rows %v, want %v", c.sql, got, c.want)
			}
		}
	}
}
