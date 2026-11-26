package sqltest

import (
	"context"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// An ungrouped COUNT is COALESCE(count, 0) (Java's adjustCountOnEmpty), NOT
// NULL in the result metadata; a grouped COUNT keeps the nullable raw count. A
// HAVING-only aggregate query may project constants.
func TestFDB_CountOnEmpty(t *testing.T) {
	t.Parallel()
	db := testkit.SetupPlanShapeDB(t, "cnt", `CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id)) CREATE TABLE e (id BIGINT, g BIGINT, PRIMARY KEY (id))`)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO t VALUES (1, 1), (2, 1), (3, 2)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sql      string
		nullable bool
		want     []int64
	}{
		{`SELECT COUNT(*) FROM e`, false, []int64{0}},
		{`SELECT COUNT(g) FROM t`, false, []int64{3}},
		{`SELECT COUNT(*) FROM t GROUP BY g`, true, []int64{2, 1}},
		{`SELECT 7 FROM e HAVING COUNT(*) = 0`, false, []int64{7}},
		{`SELECT 7 FROM t HAVING COUNT(*) > 3`, false, nil},
	} {
		rows, err := db.QueryContext(ctx, c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		types, err := rows.ColumnTypes()
		if err != nil {
			t.Fatal(err)
		}
		if nullable, ok := types[0].Nullable(); !ok || nullable != c.nullable {
			t.Errorf("%s: nullable=%t (known %t), want %t", c.sql, nullable, ok, c.nullable)
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
