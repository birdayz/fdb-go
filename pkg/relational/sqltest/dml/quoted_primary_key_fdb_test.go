package sqltest

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// A quoted lowercase primary-key column is scanned by key and answers the
// target's rows (WS-F oracle w13_quoted_pk_*).
func TestFDB_QuotedPrimaryKeyColumnScan(t *testing.T) {
	t.Parallel()
	db := testkit.SetupPlanShapeDB(t, "qpk", `CREATE TABLE "footab" ("id" BIGINT, v BIGINT, PRIMARY KEY ("id"))`)
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

// A quoted lowercase vector column, partition column and key are served by the
// vector index and answer the nearest rows.
func TestFDB_QuotedVectorColumnsScan(t *testing.T) {
	t.Parallel()
	db := testkit.SetupPlanShapeDB(t, "qvec", `CREATE TABLE "vt" ("zone" BIGINT, "id" BIGINT, "emb" VECTOR(3, FLOAT), PRIMARY KEY ("zone", "id")) `+
		`CREATE VECTOR INDEX vi USING HNSW ON "vt" ("emb") PARTITION BY ("zone")`)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO "vt" VALUES (1, 1, CAST([1.0, 0.0, 0.0] AS VECTOR(3, FLOAT))), (1, 2, CAST([0.0, 1.0, 0.0] AS VECTOR(3, FLOAT))), `+
		`(1, 3, CAST([0.0, 0.0, 1.0] AS VECTOR(3, FLOAT))), (2, 4, CAST([1.0, 0.0, 0.0] AS VECTOR(3, FLOAT)))`); err != nil {
		t.Fatal(err)
	}
	const q = `SELECT "id" FROM "vt" WHERE "zone" = 1 QUALIFY ROW_NUMBER() OVER (PARTITION BY "zone" ORDER BY euclidean_distance("emb", [0.9, 0.1, 0.0])) <= 2`
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "VectorIndexScan") {
		t.Fatalf("plan %s, want a vector index scan", plan)
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("rows %v, want [1 2]", got)
	}
}
