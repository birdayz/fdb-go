package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
)

// A FROM-less source contributes exactly one empty row. Projection, grouping,
// subqueries and DML consume it through the ordinary query pipeline.
func TestFDB_NoFromSelectProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	// Setup ends before parallel subtests start; their operation deadlines
	// must not include time spent waiting for the suite's parallel-test slots.
	defer cancel()
	setup := openTestDB(t, "/testdb_nofrom")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nofrom")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nofrom CREATE TABLE t (id BIGINT, PRIMARY KEY (id)) CREATE TABLE wide_t (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE TABLE pair_t (id BIGINT, PRIMARY KEY (id)) CREATE TABLE pair_s (id STRING, PRIMARY KEY (id)) CREATE TABLE sort_t (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE TYPE AS STRUCT item_type (sk BIGINT, co BIGINT) CREATE TABLE items_t (id BIGINT, items item_type ARRAY, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nofrom/s WITH TEMPLATE nofrom")
	dsn := fmt.Sprintf("fdbsql:///testdb_nofrom?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO wide_t VALUES (2, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO pair_t VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO pair_s VALUES ('a'), ('b')")
	mwjoMustExec(t, db, ctx, "INSERT INTO sort_t VALUES (1, 9), (2, 3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO items_t VALUES (1, [(9, 1), (3, 2)])")

	for _, tc := range []struct {
		name, query string
		columns     []string
		rows        [][]any
	}{
		{"union_quoted_lower", `SELECT id AS "x", v AS "X" FROM sort_t UNION ALL SELECT id AS "x", v AS "X" FROM sort_t ORDER BY "x"`, []string{"x", "X"}, [][]any{{int64(1), int64(9)}, {int64(1), int64(9)}, {int64(2), int64(3)}, {int64(2), int64(3)}}},
		{"union_quoted_upper", `SELECT id AS "x", v AS "X" FROM sort_t UNION ALL SELECT id AS "x", v AS "X" FROM sort_t ORDER BY "X"`, []string{"x", "X"}, [][]any{{int64(2), int64(3)}, {int64(2), int64(3)}, {int64(1), int64(9)}, {int64(1), int64(9)}}},
		{"union_quoted_mixed", `SELECT id AS "x", v AS "X" FROM sort_t UNION ALL SELECT id AS "x", v AS "X" FROM sort_t ORDER BY "x", "X"`, []string{"x", "X"}, [][]any{{int64(1), int64(9)}, {int64(1), int64(9)}, {int64(2), int64(3)}, {int64(2), int64(3)}}},
		{"qualified_whole_object_value", `SELECT d.whole.sk FROM (SELECT x.x AS whole FROM items_t, items_t.items AS x) d ORDER BY d.whole.sk`, []string{"SK"}, [][]any{{int64(3)}, {int64(9)}}},
		{"union_duplicate_labels_positions", `SELECT * FROM pair_t p, pair_t q UNION ALL SELECT * FROM pair_t p, pair_t q ORDER BY 1, 2`, []string{"ID", "ID"}, [][]any{{int64(1), int64(1)}, {int64(1), int64(1)}, {int64(1), int64(2)}, {int64(1), int64(2)}, {int64(2), int64(1)}, {int64(2), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(2)}}},
		{"union_star_distinct_keys", `SELECT * FROM sort_t UNION ALL SELECT * FROM sort_t ORDER BY id, v`, []string{"ID", "V"}, [][]any{{int64(1), int64(9)}, {int64(1), int64(9)}, {int64(2), int64(3)}, {int64(2), int64(3)}}},
		{"alias_before_star", `SELECT s.id AS v, s.* FROM sort_t s ORDER BY v`, []string{"V", "ID", "V"}, [][]any{{int64(1), int64(1), int64(9)}, {int64(2), int64(2), int64(3)}}},
		{"select_constant", "SELECT 1", []string{"_0"}, [][]any{{int64(1)}}},
		{"select_expression", "SELECT 1 + 1", []string{"_0"}, [][]any{{int64(2)}}},
		{"select_function", "SELECT UPPER('hi')", []string{"_0"}, [][]any{{"HI"}}},
		{"expression_from_singleton_table", "SELECT 1 + 1 FROM t WHERE id = 1", []string{"_0"}, [][]any{{int64(2)}}},
		{"empty_row", "SELECT *", []string{}, [][]any{{}}},
		{"empty_derived", "SELECT d.* FROM (SELECT *) d", []string{}, [][]any{{}}},
		{"two_empty_legs", "SELECT * FROM (SELECT *) a, (SELECT *) b", []string{}, [][]any{{}}},
		{"empty_derived_multiplicity", "SELECT d.* FROM (SELECT * UNION ALL SELECT *) d", []string{}, [][]any{{}, {}}},
		{"empty_cross_multiplicity", "SELECT * FROM (SELECT * UNION ALL SELECT *) a, (SELECT * UNION ALL SELECT *) b", []string{}, [][]any{{}, {}, {}, {}}},
		{"empty_cte", "WITH c AS (SELECT *) SELECT * FROM c", []string{}, [][]any{{}}},
		{"star_and_literal", "SELECT *, 1 AS n", []string{"N"}, [][]any{{int64(1)}}},
		{"star_and_aggregate", "SELECT *, COUNT(*) AS n", []string{"N"}, [][]any{{int64(1)}}},
		{"table_star_and_literal", "SELECT *, 8 AS x FROM t WHERE id = 2", []string{"ID", "X"}, [][]any{{int64(2), int64(8)}}},
		{"table_star_between_literals", "SELECT 8 AS x, *, 9 AS y FROM t WHERE id = 2", []string{"X", "ID", "Y"}, [][]any{{int64(8), int64(2), int64(9)}}},
		{"table_repeated_star", "SELECT *, 8 AS x, * FROM t WHERE id = 2", []string{"ID", "X", "ID"}, [][]any{{int64(2), int64(8), int64(2)}}},
		{"table_star_and_aggregate", "SELECT *, COUNT(*) AS n FROM t WHERE id = 2 GROUP BY id", []string{"ID", "N"}, [][]any{{int64(2), int64(1)}}},
		{"table_repeated_star_aggregate", "SELECT *, COUNT(*) AS n, * FROM t WHERE id = 2 GROUP BY id", []string{"ID", "N", "ID"}, [][]any{{int64(2), int64(1), int64(2)}}},
		{"table_star_between_aggregates", "SELECT COUNT(*) AS n, *, COUNT(*) AS n2 FROM t WHERE id = 2 GROUP BY id", []string{"N", "ID", "N2"}, [][]any{{int64(1), int64(2), int64(1)}}},
		{"wide_repeated_star", "SELECT 8 AS x, *, COUNT(*) AS n, *, 9 AS y FROM wide_t WHERE id = 2 GROUP BY id, v", []string{"X", "ID", "V", "N", "ID", "V", "Y"}, [][]any{{int64(8), int64(2), int64(7), int64(1), int64(2), int64(7), int64(9)}}},
		{"group_alias_bound_star", `SELECT * FROM wide_t x GROUP BY 2 AS "X.ID", 1`, []string{"ID", "V"}, [][]any{{int64(2), int64(7)}}},
		{"group_alias_bound_aggregate", `SELECT w.*, COUNT(*) AS n FROM wide_t w GROUP BY 1, 2 AS "W.ID"`, []string{"ID", "V", "N"}, [][]any{{int64(2), int64(7), int64(1)}}},
		{"group_alias_unqualified_name", `SELECT w.id, w.v FROM wide_t w GROUP BY w.id, w.v AS id`, []string{"ID", "V"}, [][]any{{int64(2), int64(7)}}},
		{"group_alias_qualified_column", `SELECT w.id, w.v FROM wide_t w GROUP BY w.id, w.v AS "W.ID"`, []string{"ID", "V"}, [][]any{{int64(2), int64(7)}}},
		{"group_alias_qualified_argument", `SELECT w.id, w.v, SUM(w.id) AS n FROM wide_t w GROUP BY w.id, w.v AS "W.ID"`, []string{"ID", "V", "N"}, [][]any{{int64(2), int64(7), int64(2)}}},
		{"group_alias_select_alias_order", `SELECT p.id AS z, q.id FROM pair_t p, pair_t q GROUP BY p.id, q.id AS "P.ID" ORDER BY z, q.id DESC`, []string{"Z", "ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_group_collision", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id AS "P.ID" ORDER BY z, q.id DESC`, []string{"Z", "P.ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_select_collision", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`, []string{"Z", "P.ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_computed", `SELECT p.id+0 AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`, []string{"Z", "P.ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_aggregate", `SELECT SUM(p.id) AS z, SUM(q.id) AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`, []string{"Z", "P.ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_derived_limit", `SELECT d.z, d."P.ID" FROM (SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id AS "P.ID" ORDER BY z, q.id DESC LIMIT 1) d`, []string{"Z", "P.ID"}, [][]any{{int64(1), int64(2)}}},
		{"alias_owner_select_before_group", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id AS z ORDER BY z, q.id DESC`, []string{"Z", "P.ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_group_rebased_collision", `SELECT p.id AS "Q.ID", q.id AS v FROM pair_t p, pair_t q GROUP BY p.id, q.id AS g ORDER BY g, p.id DESC`, []string{"Q.ID", "V"}, [][]any{{int64(2), int64(1)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(1), int64(2)}}},
		{"alias_owner_ambiguous_source", `SELECT p.id AS id, q.id AS v FROM pair_t p, pair_t q GROUP BY p.id, q.id AS id ORDER BY id, q.id DESC`, []string{"ID", "V"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_duplicate_outputs_unordered", `SELECT id AS z, v AS z FROM wide_t GROUP BY id, v`, []string{"Z", "Z"}, [][]any{{int64(2), int64(7)}}},
		{"alias_owner_duplicate_outputs_qualified", `SELECT id AS z, v AS z FROM wide_t GROUP BY id, v ORDER BY wide_t.id`, []string{"Z", "Z"}, [][]any{{int64(2), int64(7)}}},
		{"alias_owner_plain", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q ORDER BY z, q.id DESC`, []string{"Z", "P.ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_named_not_numeric", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, p.id, q.id DESC`, []string{"Z", "P.ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"alias_owner_quoted", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY "P.ID", p.id DESC`, []string{"Z", "P.ID"}, [][]any{{int64(2), int64(1)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(1), int64(2)}}},
		{"group_alias_quoted_order", `SELECT p.id, q.id FROM pair_t p, pair_t q GROUP BY p.id, q.id AS "P.ID" ORDER BY "P.ID", p.id DESC`, []string{"ID", "ID"}, [][]any{{int64(2), int64(1)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(1), int64(2)}}},
		{"group_alias_quoted_read", `SELECT "W.ID" FROM wide_t w GROUP BY w.v AS "W.ID"`, []string{"W.ID"}, [][]any{{int64(7)}}},
		{"group_alias_quoted_argument", `SELECT SUM("W.ID") AS n FROM wide_t w GROUP BY w.v AS "W.ID"`, []string{"N"}, [][]any{{int64(7)}}},
		{"group_alias_qualified_order", `SELECT p.id, q.id FROM pair_t p, pair_t q GROUP BY p.id, q.id AS "P.ID" ORDER BY p.id, q.id DESC`, []string{"ID", "ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"heterogeneous_grouped_cte", "WITH c AS (SELECT * FROM pair_t, pair_s GROUP BY 1, 2) SELECT * FROM c ORDER BY 1, 2 DESC", []string{"ID", "ID"}, [][]any{{int64(1), "b"}, {int64(1), "a"}, {int64(2), "b"}, {int64(2), "a"}}},
		{"computed_duplicate_slots", "SELECT id+0 AS x, id+0 AS y FROM pair_t ORDER BY 1, 2 DESC", []string{"X", "Y"}, [][]any{{int64(1), int64(1)}, {int64(2), int64(2)}}},
		{"two_source_star_order", "SELECT * FROM pair_t a, pair_t b ORDER BY 1, 2 DESC", []string{"ID", "ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"qualified_two_source_star_order", "SELECT a.*, b.* FROM pair_t a, pair_t b ORDER BY 1, 2 DESC", []string{"ID", "ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"star_aliased_column_order", "SELECT a.*, b.id AS id FROM pair_t a, pair_t b ORDER BY 1, 2 DESC", []string{"ID", "ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"two_source_star_group", "SELECT * FROM pair_t, pair_t GROUP BY 1, 2 ORDER BY 1, 2 DESC", []string{"ID", "ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"duplicate_alias_star_group", "SELECT * FROM pair_t p, pair_t p GROUP BY 1, 2 ORDER BY 1, 2 DESC", []string{"ID", "ID"}, [][]any{{int64(1), int64(2)}, {int64(1), int64(1)}, {int64(2), int64(2)}, {int64(2), int64(1)}}},
		{"two_source_star_aggregate", "SELECT *, COUNT(*) AS n FROM pair_t, pair_t GROUP BY 1, 2 ORDER BY 1, 2 DESC", []string{"ID", "ID", "N"}, [][]any{{int64(1), int64(2), int64(1)}, {int64(1), int64(1), int64(1)}, {int64(2), int64(2), int64(1)}, {int64(2), int64(1), int64(1)}}},
		{"duplicate_alias_star_aggregate", "SELECT *, COUNT(*) AS n FROM pair_t p, pair_t p GROUP BY 1, 2 ORDER BY 1, 2 DESC", []string{"ID", "ID", "N"}, [][]any{{int64(1), int64(2), int64(1)}, {int64(1), int64(1), int64(1)}, {int64(2), int64(2), int64(1)}, {int64(2), int64(1), int64(1)}}},
		{"star_between_aggregates_identity", "SELECT COUNT(*) AS n, *, COUNT(*) AS m FROM pair_t p, pair_t p GROUP BY 2, 3 ORDER BY 2, 3 DESC", []string{"N", "ID", "ID", "M"}, [][]any{{int64(1), int64(1), int64(2), int64(1)}, {int64(1), int64(1), int64(1), int64(1)}, {int64(1), int64(2), int64(2), int64(1)}, {int64(1), int64(2), int64(1), int64(1)}}},
		{"zero_star_positional_group", "SELECT *, 1 AS a, COUNT(*) AS n GROUP BY 1", []string{"A", "N"}, [][]any{{int64(1), int64(1)}}},
		{"zero_star_positional_order", "SELECT *, 8 AS x ORDER BY 1", []string{"X"}, [][]any{{int64(8)}}},
		{"one_star_group_no_aggregate", "SELECT *, 8 AS x FROM t WHERE id < 3 GROUP BY id ORDER BY 1", []string{"ID", "X"}, [][]any{{int64(1), int64(8)}, {int64(2), int64(8)}}},
		{"one_star_positional_order", "SELECT *, 8 AS x FROM t WHERE id < 3 ORDER BY 2, 1 DESC", []string{"ID", "X"}, [][]any{{int64(2), int64(8)}, {int64(1), int64(8)}}},
		{"wide_star_positional_order", "SELECT *, 8 AS x FROM wide_t ORDER BY 3", []string{"ID", "V", "X"}, [][]any{{int64(2), int64(7), int64(8)}}},
		{"wide_star_positional_group", "SELECT *, 8 AS x, COUNT(*) AS n FROM wide_t GROUP BY 1, 2", []string{"ID", "V", "X", "N"}, [][]any{{int64(2), int64(7), int64(8), int64(1)}}},
		{"wide_star_group_no_aggregate", "SELECT *, 8 AS x FROM wide_t GROUP BY 1, 2", []string{"ID", "V", "X"}, [][]any{{int64(2), int64(7), int64(8)}}},
		{"wide_repeated_star_positional_group", "SELECT *, 8 AS x, *, COUNT(*) AS n FROM wide_t GROUP BY 4, 5 ORDER BY 6", []string{"ID", "V", "X", "ID", "V", "N"}, [][]any{{int64(2), int64(7), int64(8), int64(2), int64(7), int64(1)}}},
		{"qualified_star_positional_group", "SELECT w.*, 8 AS x, COUNT(*) AS n FROM wide_t w GROUP BY 1, 2", []string{"ID", "V", "X", "N"}, [][]any{{int64(2), int64(7), int64(8), int64(1)}}},
		{"aliased_column_positional_group", "SELECT id AS x, COUNT(*) AS n FROM t WHERE id < 3 GROUP BY 1 ORDER BY 1", []string{"X", "N"}, [][]any{{int64(1), int64(1)}, {int64(2), int64(1)}}},
		{"sole_star_positional_group", "SELECT * FROM wide_t GROUP BY 1, 2 ORDER BY 2", []string{"ID", "V"}, [][]any{{int64(2), int64(7)}}},
		{"group_constant", "SELECT 1 AS a, COUNT(*) AS n GROUP BY 1", []string{"A", "N"}, [][]any{{int64(1), int64(1)}}},
		{"empty_group_count", "SELECT COUNT(*) AS n FROM (SELECT *) d", []string{"N"}, [][]any{{int64(1)}}},
		{"recursive", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT n FROM c", []string{"N"}, [][]any{{int64(1)}, {int64(2)}, {int64(3)}}},
		{"recursive_order", "WITH RECURSIVE counter(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM counter WHERE n < 5) SELECT n FROM counter ORDER BY n", []string{"N"}, [][]any{{int64(1)}, {int64(2)}, {int64(3)}, {int64(4)}, {int64(5)}}},
		{"recursive_filter", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT n FROM c WHERE n > 1", []string{"N"}, [][]any{{int64(2)}, {int64(3)}}},
		{"recursive_sum", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT SUM(n) AS s FROM c", []string{"S"}, [][]any{{int64(6)}}},
		{"recursive_group", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT n, COUNT(*) AS ct FROM c GROUP BY n ORDER BY n DESC", []string{"N", "CT"}, [][]any{{int64(3), int64(1)}, {int64(2), int64(1)}, {int64(1), int64(1)}}},
		{"recursive_join", "WITH RECURSIVE c(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT c.n FROM c, t WHERE c.n = t.id AND t.id = 1", []string{"N"}, [][]any{{int64(1)}}},
		{"exists", "SELECT EXISTS(SELECT 1) AS e", []string{"E"}, [][]any{{true}}},
		{"scalar", "SELECT (SELECT 4) AS n", []string{"N"}, [][]any{{int64(4)}}},
		{"zero_limit", "SELECT 1 LIMIT 0", []string{"_0"}, [][]any{}},
		{"empty_offset", "SELECT 1 LIMIT 1 OFFSET 1", []string{"_0"}, [][]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			columns, err := rows.Columns()
			if err != nil || !reflect.DeepEqual(columns, tc.columns) {
				t.Fatalf("columns=%v error=%v, want %v", columns, err, tc.columns)
			}
			got := make([][]any, 0)
			for rows.Next() {
				row := make([]any, len(columns))
				pointers := make([]any, len(row))
				for i := range row {
					pointers[i] = &row[i]
				}
				if err := rows.Scan(pointers...); err != nil {
					t.Fatal(err)
				}
				got = append(got, row)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.rows) {
				t.Fatalf("rows=%#v, want %#v", got, tc.rows)
			}
		})
	}
	for _, tc := range []struct {
		name, query string
		code        api.ErrorCode
	}{
		{"repeated_order_position", "SELECT * FROM pair_t ORDER BY 1, 1", api.ErrCodeColumnAlreadyExists},
		{"repeated_order_source", "SELECT *, * FROM pair_t ORDER BY 1, 2", api.ErrCodeColumnAlreadyExists},
		{"position_then_named_order", "SELECT id FROM pair_t ORDER BY 1, id", api.ErrCodeColumnAlreadyExists},
		{"named_then_position_order", "SELECT id FROM pair_t ORDER BY id, 1", api.ErrCodeColumnAlreadyExists},
		{"position_then_alias_order", "SELECT id AS x FROM pair_t ORDER BY 1, x", api.ErrCodeColumnAlreadyExists},
		{"repeated_group_position", "SELECT *, COUNT(*) FROM pair_t GROUP BY 1, 1", api.ErrCodeAmbiguousColumn},
		{"repeated_group_source", "SELECT *, *, COUNT(*) FROM pair_t GROUP BY 1, 2", api.ErrCodeAmbiguousColumn},
		{"alias_owner_invalid_computed_alias", `SELECT p.id+0 AS z, q.id AS "(P.ID + 0)", q.id AS "P.ID + 0" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`, api.ErrCodeInvalidName},
		{"alias_owner_invalid_aggregate_alias", `SELECT SUM(p.id) AS z, SUM(q.id) AS "SUM(P.ID)" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`, api.ErrCodeInvalidName},
		{"alias_owner_duplicate_position", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY 1, z`, api.ErrCodeColumnAlreadyExists},
		{"alias_owner_ambiguous_absent_grouped", `SELECT id AS z, v AS z FROM wide_t GROUP BY id, v ORDER BY z`, api.ErrCodeAmbiguousColumn},
		{"alias_owner_ambiguous_source_grouped", `SELECT id AS id, v AS id FROM wide_t GROUP BY id, v ORDER BY id`, api.ErrCodeAmbiguousColumn},
		{"alias_owner_ambiguity_before_duplicate_plain", `SELECT id AS z, v AS z FROM wide_t ORDER BY z, z`, api.ErrCodeAmbiguousColumn},
		{"alias_owner_ambiguity_before_duplicate_grouped", `SELECT id AS z, v AS z FROM wide_t GROUP BY id, v ORDER BY z, z`, api.ErrCodeAmbiguousColumn},
		{"alias_owner_ambiguous_absent_plain", `SELECT id AS z, v AS z FROM wide_t ORDER BY z`, api.ErrCodeAmbiguousColumn},
		{"alias_owner_ambiguous_source_plain", `SELECT id AS id, v AS id FROM wide_t ORDER BY id`, api.ErrCodeAmbiguousColumn},
		{"union_star_duplicate_name", `SELECT * FROM sort_t UNION ALL SELECT * FROM sort_t ORDER BY id, id`, api.ErrCodeColumnAlreadyExists},
		{"union_star_missing_name", `SELECT * FROM sort_t UNION ALL SELECT * FROM sort_t ORDER BY missing, missing`, api.ErrCodeUndefinedColumn},
		{"qualified_whole_object_alias_precedence", `SELECT x.x, 99 AS x FROM items_t, items_t.items AS x ORDER BY x, x`, api.ErrCodeAmbiguousColumn},
		{"union_joined_star_ambiguous", `SELECT * FROM sort_t p, sort_t q UNION ALL SELECT * FROM sort_t p, sort_t q ORDER BY id`, api.ErrCodeAmbiguousColumn},
		{"union_joined_star_ambiguity_before_duplicate", `SELECT * FROM sort_t p, sort_t q UNION ALL SELECT * FROM sort_t p, sort_t q ORDER BY id, id`, api.ErrCodeAmbiguousColumn},
		{"whole_object_alias_precedence", `SELECT x, 99 AS x FROM items_t, items_t.items AS x ORDER BY x, x`, api.ErrCodeAmbiguousColumn},
		{"union_duplicate_name", `SELECT id FROM t UNION ALL SELECT id FROM t ORDER BY id, id`, api.ErrCodeColumnAlreadyExists},
		{"union_missing_before_duplicate", `SELECT id FROM t UNION ALL SELECT id FROM t ORDER BY missing, missing`, api.ErrCodeUndefinedColumn},
		{"unnamed_source_alias", `SELECT "_0", "_1" AS "_0" FROM VALUES (9, 1), (3, 2) ORDER BY "_0"`, api.ErrCodeAmbiguousColumn},
		{"unnamed_star_alias", `SELECT *, 99 AS "_0" FROM VALUES (42) ORDER BY "_0"`, api.ErrCodeAmbiguousColumn},
		{"alias_owner_duplicate_name", `SELECT p.id AS z, q.id AS "P.ID" FROM pair_t p, pair_t q GROUP BY p.id, q.id ORDER BY z, z`, api.ErrCodeColumnAlreadyExists},
		{"group_alias_repeat_quoted_order", `SELECT p.id, q.id FROM pair_t p, pair_t q GROUP BY p.id, q.id AS "P.ID" ORDER BY "P.ID", "P.ID"`, api.ErrCodeColumnAlreadyExists},
		{"group_alias_repeat_qualified_order", `SELECT p.id, q.id FROM pair_t p, pair_t q GROUP BY p.id, q.id AS "P.ID" ORDER BY p.id, P.ID`, api.ErrCodeColumnAlreadyExists},
		{"group_alias_ungrouped_star", `SELECT * FROM wide_t x GROUP BY 2 AS "X.ID"`, api.ErrCodeGroupingError},
		{"group_alias_ungrouped_aggregate", `SELECT w.*, COUNT(*) AS n FROM wide_t w GROUP BY 2 AS "W.ID"`, api.ErrCodeGroupingError},
		{"ungrouped_bound_source", "SELECT *, COUNT(*) FROM pair_t p, pair_t p GROUP BY 1", api.ErrCodeGroupingError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			rows, err := db.QueryContext(ctx, tc.query)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
				}
				err = rows.Err()
			}
			if err == nil {
				t.Fatalf("expected SQLSTATE %s for %s", tc.code, tc.query)
			}
			requireSQLSTATE(t, err, tc.code)
		})
	}
	t.Run("explode_no_scan", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT 1").Scan(&plan); err != nil {
			t.Fatal(err)
		}
		t.Logf("singleton plan: %s", plan)
		if !strings.Contains(plan, "Explode") || strings.Contains(plan, "Scan") {
			t.Fatalf("singleton must use Explode without a table scan: %s", plan)
		}
	})
	t.Run("insert_select", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		result, err := db.ExecContext(ctx, "INSERT INTO t SELECT 7")
		if err != nil {
			t.Fatal(err)
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			t.Fatalf("affected rows=%d error=%v, want 1", count, err)
		}
		var id int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM t WHERE id = 7").Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != 7 {
			t.Fatalf("stored id=%d, want 7", id)
		}
	})
}
