package sqldriver_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

// TestFDB_NonCorrelatedExistsEmptySubquery pins that a non-correlated
// EXISTS / NOT EXISTS over an empty subquery evaluates correctly. The
// EXISTS semi-join must decide on inner-row *presence*; an earlier bug
// wrapped the non-correlated inner in FirstOrDefault (always one row),
// making EXISTS always true and NOT EXISTS always false (RFC-035).
func TestFDB_NonCorrelatedExistsEmptySubquery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/diag_nce")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /diag_nce"); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE diag_nce_tmpl
		CREATE TABLE Product (id BIGINT NOT NULL, price BIGINT, PRIMARY KEY (id))
		CREATE TABLE Flag (name STRING NOT NULL, PRIMARY KEY (name))`); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /diag_nce/main WITH TEMPLATE diag_nce_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql:///diag_nce?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`INSERT INTO Flag (name) VALUES ('apply_discount')`,
		`INSERT INTO Product (id, price) VALUES (1, 100)`,
		`INSERT INTO Product (id, price) VALUES (2, 200)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	query := func(sql string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, sql)
		if err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		return ids
	}

	// Empty subquery (no 'disable_delete' flag): NOT EXISTS → all rows.
	if got := query(`SELECT id FROM Product WHERE NOT EXISTS (SELECT name FROM Flag WHERE name = 'disable_delete') ORDER BY id`); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("NOT EXISTS(empty) = %v, want [1 2]", got)
	}
	// Empty subquery: EXISTS → no rows.
	if got := query(`SELECT id FROM Product WHERE EXISTS (SELECT name FROM Flag WHERE name = 'disable_delete') ORDER BY id`); len(got) != 0 {
		t.Fatalf("EXISTS(empty) = %v, want []", got)
	}
	// Non-empty subquery (apply_discount exists): EXISTS → all rows.
	if got := query(`SELECT id FROM Product WHERE EXISTS (SELECT name FROM Flag WHERE name = 'apply_discount') ORDER BY id`); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("EXISTS(non-empty) = %v, want [1 2]", got)
	}
	// Non-empty subquery: NOT EXISTS → no rows.
	if got := query(`SELECT id FROM Product WHERE NOT EXISTS (SELECT name FROM Flag WHERE name = 'apply_discount') ORDER BY id`); len(got) != 0 {
		t.Fatalf("NOT EXISTS(non-empty) = %v, want []", got)
	}
}
