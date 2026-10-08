package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// A GROUP BY over a nullable indexed column streams off the value index and
// still returns the NULL group: a value index stores an entry for a NULL key.
func TestFDB_NullableGroupByOverIndexKeepsTheNullGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/nullgrp_idx", "NULLGRP_IDX",
		"CREATE TABLE products (id BIGINT, category INTEGER, price INTEGER, name STRING, PRIMARY KEY (id)) CREATE INDEX idx_cat ON products (category)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO products VALUES (1, 1, 10, 'a'), (2, NULL, 5, 'b'), (3, 1, 2, 'c')")
	const q = "SELECT category, SUM(price) AS total FROM products GROUP BY category ORDER BY category"
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "IndexScan(IDX_CAT") || strings.Contains(plan, "InMemorySort") {
		t.Fatalf("plan %s: want a streaming aggregation off IDX_CAT", plan)
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var c sql.NullInt64
		var s int64
		if err := rows.Scan(&c, &s); err != nil {
			t.Fatal(err)
		}
		if c.Valid {
			got = append(got, "1:"+string(rune('0'+s/10))+string(rune('0'+s%10)))
		} else {
			got = append(got, "NULL:0"+string(rune('0'+s)))
		}
	}
	if strings.Join(got, ",") != "NULL:05,1:12" {
		t.Fatalf("rows %v, want NULL:05,1:12", got)
	}
}
