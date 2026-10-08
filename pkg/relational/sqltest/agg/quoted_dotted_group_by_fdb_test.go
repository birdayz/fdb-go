package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// A quoted column name holding dots and starting with its table's name is
// one column, not a qualified path, through GROUP BY's existence check and its
// output-slot binding (Java valid-identifiers.yamsql).
func TestFDB_QuotedDottedColumnGroupBy(t *testing.T) {
	t.Parallel()
	db := testkit.SetupPlanShapeDB(t, "qdgb", `create table "foo.tableA"("foo.tableA.A1" bigint, "foo.tableA.A2" bigint, "foo.tableA.A3" bigint, primary key("foo.tableA.A1")) `+
		`create index "foo.tableA.idx2" as select sum("foo.tableA.A1") FROM "foo.tableA" group by "foo.tableA.A2"`)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `insert into "foo.tableA" values (1, 10, 1), (2, 10, 2), (3, 20, 2)`); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		`select "foo.tableA.A2", sum("foo.tableA.A1") from "foo.tableA" group by "foo.tableA.A2"`: "[[10 3] [20 3]]",
		`select "foo.tableA.A3", count(*) from "foo.tableA" group by "foo.tableA.A3"`:             "[[1 1] [2 2]]",
		`select x, sum("foo.tableA.A1") from "foo.tableA" group by "foo.tableA.A2" as x`:          "[[10 3] [20 3]]",
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		var got [][2]int64
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			got = append(got, [2]int64{a, b})
		}
		_ = rows.Close()
		if fmt.Sprint(got) != want {
			t.Errorf("%s: %v, want %s", q, got, want)
		}
	}
}
