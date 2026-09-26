package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_ScalarDerivedSources pins the Go scalar-subquery extension over
// derived primary and join sources. Derived aliases must carry their bodies and
// exact scope metadata, not resolve as catalog tables. Zero inner rows are NULL,
// one yields its value, and multiple rows raise the scalar cardinality error.
func TestFDB_ScalarDerivedSources(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_scalar_derived_decline"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE sdd_tmpl "+
		"CREATE TABLE ord (order_id BIGINT, cust_id BIGINT, PRIMARY KEY (order_id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE sdd_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbPath), clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, ctx, "INSERT INTO ord VALUES (1, 10), (2, 20)")

	queryRows := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		types, err := rows.ColumnTypes()
		if err != nil || len(types) != 2 || types[1].DatabaseTypeName() != "BIGINT" {
			t.Fatalf("scalar metadata=%v, err=%v; want second column BIGINT", types, err)
		}
		if nullable, known := types[1].Nullable(); !known || !nullable {
			t.Fatalf("scalar metadata nullable=(%v,%v), want known nullable", nullable, known)
		}
		var out []string
		for rows.Next() {
			var id int64
			var value sql.NullInt64
			if err := rows.Scan(&id, &value); err != nil {
				t.Fatal(err)
			}
			if value.Valid {
				out = append(out, fmt.Sprintf("%d=%d", id, value.Int64))
			} else {
				out = append(out, fmt.Sprintf("%d=NULL", id))
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)
		return out
	}
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"derived_primary", "SELECT o.order_id, (SELECT d.cust_id FROM (SELECT order_id, cust_id FROM ord) AS d WHERE d.order_id = o.order_id) FROM ord AS o", []string{"1=10", "2=20"}},
		{"derived_primary_empty", "SELECT o.order_id, (SELECT d.cust_id FROM (SELECT order_id, cust_id FROM ord WHERE order_id = 100) AS d WHERE d.order_id = o.order_id) FROM ord AS o", []string{"1=NULL", "2=NULL"}},
		{"derived_primary_computed", "SELECT o.order_id, (SELECT d.v FROM (SELECT order_id, cust_id + 1 AS v FROM ord) AS d WHERE d.order_id = o.order_id) FROM ord AS o", []string{"1=11", "2=21"}},
		{"derived_leg_filtered", "SELECT o.order_id, (SELECT a.cust_id FROM ord a, (SELECT order_id FROM ord) AS d WHERE a.order_id = o.order_id AND d.order_id = a.order_id) FROM ord AS o", []string{"1=10", "2=20"}},
		{"derived_primary_reads_cte", "WITH c AS (SELECT order_id, cust_id + 2 AS v FROM ord) SELECT o.order_id, (SELECT d.v FROM (SELECT order_id, v FROM c) AS d WHERE d.order_id = o.order_id) FROM ord AS o", []string{"1=12", "2=22"}},
		{"derived_primary_alias_shadows_outer_leg", "SELECT o.order_id, (SELECT d.cust_id FROM (SELECT order_id, cust_id FROM ord) AS d WHERE d.order_id = o.order_id) FROM ord AS o, ord AS d", []string{"1=10", "1=10", "2=20", "2=20"}},
		{"derived_join_alias_shadows_outer_leg", "SELECT o.order_id, (SELECT a.cust_id FROM ord a JOIN (SELECT order_id FROM ord) AS d ON a.order_id = d.order_id WHERE a.order_id = o.order_id) FROM ord AS o, ord AS d", []string{"1=10", "1=10", "2=20", "2=20"}},
		{"derived_shadow_qualified_star", "SELECT o.order_id, (SELECT d.* FROM (SELECT cust_id FROM ord WHERE order_id = 1) AS d WHERE o.order_id > 0) FROM ord AS o, ord AS d", []string{"1=10", "1=10", "2=10", "2=10"}},
		{"derived_shadow_using", "SELECT o.order_id, (SELECT d.cust_id FROM (SELECT order_id, cust_id FROM ord) AS d JOIN (SELECT order_id FROM ord) AS a USING (order_id) WHERE d.order_id = o.order_id) FROM ord AS o, ord AS d", []string{"1=10", "1=10", "2=20", "2=20"}},
		{"derived_body_reads_actual_parent", "SELECT o.order_id, (SELECT d.v FROM (SELECT o.cust_id AS v FROM ord WHERE order_id = 1) AS d) FROM ord AS o, ord AS d", []string{"1=10", "1=10", "2=20", "2=20"}},
		{"derived_same_level_duplicate_aliases", "SELECT o.order_id, (SELECT x.cust_id FROM (SELECT order_id FROM ord) AS x, (SELECT cust_id FROM ord WHERE order_id = 1) AS x WHERE x.order_id = o.order_id) FROM ord AS o", []string{"1=10", "2=10"}},
		{"derived_quoted_mint_alias", `SELECT o.order_id, (SELECT "Q$DERIVED0".cust_id FROM (SELECT order_id, cust_id FROM ord) AS "Q$DERIVED0" WHERE "Q$DERIVED0".order_id = o.order_id) FROM ord AS o, ord AS "Q$DERIVED0"`, []string{"1=10", "1=10", "2=20", "2=20"}},
		{"derived_body_own_with", "SELECT o.order_id, (SELECT d.v FROM (WITH c AS (SELECT order_id, cust_id + 3 AS v FROM ord) SELECT order_id, v FROM c) AS d WHERE d.order_id = o.order_id) FROM ord AS o", []string{"1=13", "2=23"}},
		{"derived_body_hidden_table_homonym", "SELECT ord.order_id, (SELECT d.cust_id FROM (SELECT order_id, cust_id FROM ord) AS d WHERE d.order_id = ord.order_id) FROM ord, ord AS other", []string{"1=10", "1=10", "2=20", "2=20"}},
		{"derived_cluster_disjoint", "SELECT o.order_id, (SELECT d.cust_id FROM (SELECT order_id, cust_id FROM ord) AS d WHERE d.order_id = o.order_id) FROM ord AS o, ord AS other", []string{"1=10", "1=10", "2=20", "2=20"}},
		{"derived_leg_on", "SELECT o.order_id, (SELECT a.cust_id FROM ord a JOIN (SELECT order_id FROM ord) AS d ON a.order_id = d.order_id WHERE a.order_id = o.order_id) FROM ord AS o", []string{"1=10", "2=20"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var firstPlan string
			for i := range 5 {
				var plan string
				if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.query).Scan(&plan); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					firstPlan = plan
				} else if plan != firstPlan {
					t.Fatalf("derived private identity leaked planning history:\n%s\n%s", firstPlan, plan)
				}
			}
			got := queryRows(t, tc.query)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	// The original comma-leg query has TWO inner rows per outer row. Preserve
	// that exact query as a cardinality negative, not a silent first-row answer.
	t.Run("derived_leg_cardinality", func(t *testing.T) {
		t.Parallel()
		rows, qerr := db.QueryContext(ctx, "SELECT o.order_id, (SELECT a.cust_id FROM ord a, (SELECT order_id FROM ord) AS d WHERE a.order_id = o.order_id) FROM ord AS o")
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		requireSQLSTATE(t, qerr, api.ErrCodeCardinalityViolation)
	})
}
