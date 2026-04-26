package plandiff

// SeedRunCorpus is the runSql parallel of SeedCorpus: a small set of
// (schema, setup-DMLs, SELECT) cases for the result-set diff harness.
// Today only the Java side runs (Go is gated on Track C2). Once the
// Go runner is wired, RunCorpus + this corpus produce real
// cross-engine result-set diffs.
//
// Each entry's SetupSqls must produce deterministic state — SELECTs
// without ORDER BY can't be added until we trust both engines'
// row-order semantics match. Today every entry orders by primary key.
type RunQuery struct {
	Name           string
	SetupSqls      []string
	Query          string
	SchemaTemplate string
}

// SeedRunCorpus returns the baseline RunQuery set. Add entries that
// exercise distinct primitive types or row-shape edge cases (NULL
// handling, multi-row, empty, single-column, multi-column).
func SeedRunCorpus() []RunQuery {
	return []RunQuery{
		{
			Name:           "single_row_bigint",
			SchemaTemplate: "CREATE TABLE T1 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{"INSERT INTO T1 VALUES (42)"},
			Query:          "SELECT id FROM T1 ORDER BY id",
		},
		{
			Name:           "multi_row_string",
			SchemaTemplate: "CREATE TABLE T2 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T2 VALUES (1, 'alice')",
				"INSERT INTO T2 VALUES (2, 'bob')",
				"INSERT INTO T2 VALUES (3, 'carol')",
			},
			Query: "SELECT id, name FROM T2 ORDER BY id",
		},
		{
			Name:           "null_string",
			SchemaTemplate: "CREATE TABLE T3 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T3 VALUES (1, 'alice')",
				"INSERT INTO T3 VALUES (2, NULL)",
			},
			Query: "SELECT id, name FROM T3 ORDER BY id",
		},
		{
			Name:           "empty_table",
			SchemaTemplate: "CREATE TABLE T4 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      nil,
			Query:          "SELECT id FROM T4",
		},
		{
			Name:           "boolean_column",
			SchemaTemplate: "CREATE TABLE T5 (id BIGINT, flag BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T5 VALUES (1, TRUE)",
				"INSERT INTO T5 VALUES (2, FALSE)",
				"INSERT INTO T5 VALUES (3, NULL)",
			},
			Query: "SELECT id, flag FROM T5 ORDER BY id",
		},
		{
			Name:           "double_column",
			SchemaTemplate: "CREATE TABLE T6 (id BIGINT, val DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T6 VALUES (1, 1.5)",
				"INSERT INTO T6 VALUES (2, -7.25)",
			},
			Query: "SELECT id, val FROM T6 ORDER BY id",
		},
		{
			Name:           "order_by_desc",
			SchemaTemplate: "CREATE TABLE T7 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T7 VALUES (1, 'a')",
				"INSERT INTO T7 VALUES (2, 'b')",
				"INSERT INTO T7 VALUES (3, 'c')",
			},
			Query: "SELECT id FROM T7 ORDER BY id DESC",
		},
		{
			Name:           "where_filter",
			SchemaTemplate: "CREATE TABLE T8 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T8 VALUES (1, 100)",
				"INSERT INTO T8 VALUES (2, 200)",
				"INSERT INTO T8 VALUES (3, 300)",
			},
			Query: "SELECT id, val FROM T8 WHERE val > 150 ORDER BY id",
		},
		{
			Name:           "multi_column_pk",
			SchemaTemplate: "CREATE TABLE T9 (region STRING, id BIGINT, name STRING, PRIMARY KEY (region, id))",
			SetupSqls: []string{
				"INSERT INTO T9 VALUES ('us', 1, 'alice')",
				"INSERT INTO T9 VALUES ('us', 2, 'bob')",
				"INSERT INTO T9 VALUES ('eu', 1, 'carol')",
			},
			Query: "SELECT region, id, name FROM T9 ORDER BY region, id",
		},
		{
			Name:           "select_constant_expr",
			SchemaTemplate: "CREATE TABLE T10 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{"INSERT INTO T10 VALUES (5)"},
			Query:          "SELECT id, id + 10 FROM T10",
		},
		{
			Name:           "string_filter",
			SchemaTemplate: "CREATE TABLE T11 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T11 VALUES (1, 'apple')",
				"INSERT INTO T11 VALUES (2, 'banana')",
				"INSERT INTO T11 VALUES (3, 'cherry')",
			},
			Query: "SELECT id, name FROM T11 WHERE name = 'banana'",
		},
	}
}

// SeedCorpus is the small RFC-022 §4.-1 baseline set: 35 queries
// hand-picked to exercise the planner shapes the existing 11-branch
// pushdown chain rewrites (covered by Cascades Batch A rules — see
// TODO.md §HIGH 4.5). Each query is a single SELECT / DML / DDL whose
// plan-tree shape is stable under today's naive Go generator.
//
// Adding queries: name uniquely (it keys golden output + the per-
// query Diff in reports). SQL must be a complete, parseable statement.
// SchemaTemplate is optional; engines that don't consult metadata
// today (text-only Go) ignore it. When the catalog-aware Go path or
// real Java planning lands, queries with non-empty SchemaTemplate will
// pick up predicate-tree rendering automatically.
//
// Editing constraints: any change to a corpus entry's SQL or name
// invalidates the golden hash in plandiff_test.go. Update both
// together. Removing a corpus entry is fine — the hash recomputes —
// but think twice before removing one whose Status is currently
// AGREE; that's a regression sentinel.
func SeedCorpus() []Query {
	return []Query{
		// --- Bare scans ----------------------------------------------------
		{
			Name: "select_star_one_table",
			SQL:  "SELECT * FROM users",
		},
		{
			Name: "select_named_columns",
			SQL:  "SELECT id, name FROM users",
		},
		// --- Filter shapes -------------------------------------------------
		{
			Name: "select_with_equality_where",
			SQL:  "SELECT id FROM users WHERE active = TRUE",
		},
		{
			Name: "select_with_range_where",
			SQL:  "SELECT id FROM orders WHERE price > 100",
		},
		{
			Name: "select_with_compound_and",
			SQL:  "SELECT id FROM orders WHERE price > 100 AND status = 'open'",
		},
		{
			Name: "select_with_compound_or",
			SQL:  "SELECT id FROM orders WHERE price > 100 OR status = 'open'",
		},
		{
			Name: "select_with_not",
			SQL:  "SELECT id FROM orders WHERE NOT (status = 'closed')",
		},
		{
			Name: "select_with_in_list",
			SQL:  "SELECT id FROM orders WHERE status IN ('open', 'pending', 'shipped')",
		},
		{
			Name: "select_with_between",
			SQL:  "SELECT id FROM orders WHERE price BETWEEN 50 AND 200",
		},
		{
			Name: "select_with_like_prefix",
			SQL:  "SELECT id FROM orders WHERE customer LIKE 'A%'",
		},
		{
			Name: "select_with_is_null",
			SQL:  "SELECT id FROM orders WHERE shipped_at IS NULL",
		},
		// --- Sort + Limit --------------------------------------------------
		{
			Name: "select_order_by_asc",
			SQL:  "SELECT id, name FROM users ORDER BY name ASC",
		},
		{
			Name: "select_order_by_desc",
			SQL:  "SELECT id, name FROM users ORDER BY name DESC",
		},
		{
			Name: "select_order_by_limit",
			SQL:  "SELECT id, name FROM users ORDER BY id LIMIT 10",
		},
		{
			Name: "select_limit_offset",
			SQL:  "SELECT id FROM users LIMIT 10 OFFSET 20",
		},
		// --- Aggregation ---------------------------------------------------
		{
			Name: "select_count_star",
			SQL:  "SELECT COUNT(*) FROM users",
		},
		{
			Name: "select_group_by_count",
			SQL:  "SELECT status, COUNT(*) FROM orders GROUP BY status",
		},
		{
			Name: "select_group_by_having",
			SQL:  "SELECT status, COUNT(*) FROM orders GROUP BY status HAVING COUNT(*) > 5",
		},
		// --- Joins ---------------------------------------------------------
		{
			Name: "select_inner_join",
			SQL:  "SELECT a.id, b.name FROM a INNER JOIN b ON a.b_id = b.id",
		},
		{
			Name: "select_left_outer_join",
			SQL:  "SELECT a.id, b.name FROM a LEFT OUTER JOIN b ON a.b_id = b.id",
		},
		// --- DML / DDL -----------------------------------------------------
		{
			Name: "delete_with_filter",
			SQL:  "DELETE FROM orders WHERE status = 'cancelled'",
		},
		{
			Name: "update_simple",
			SQL:  "UPDATE orders SET status = 'shipped' WHERE id = 42",
		},
		{
			Name: "insert_values",
			SQL:  "INSERT INTO users (id, name) VALUES (1, 'alice')",
		},
		// --- Compound shapes -----------------------------------------------
		{
			Name: "select_distinct",
			SQL:  "SELECT DISTINCT status FROM orders",
		},
		{
			Name: "select_union_all",
			SQL:  "SELECT id FROM a UNION ALL SELECT id FROM b",
		},
		{
			Name: "select_union_distinct",
			SQL:  "SELECT id FROM a UNION SELECT id FROM b",
		},
		{
			Name: "select_with_cte",
			SQL:  "WITH active_users AS (SELECT id FROM users WHERE active = TRUE) SELECT id FROM active_users",
		},
		{
			Name: "select_recursive_cte",
			SQL:  "WITH RECURSIVE c AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 10) SELECT n FROM c",
		},
		// --- Joins (extra shapes) ------------------------------------------
		{
			Name: "select_right_outer_join",
			SQL:  "SELECT a.id, b.name FROM a RIGHT OUTER JOIN b ON a.b_id = b.id",
		},
		// --- Projection expressions ---------------------------------------
		{
			Name: "select_with_arithmetic_projection",
			SQL:  "SELECT id, price * qty FROM orders",
		},
		{
			Name: "select_with_function_projection",
			SQL:  "SELECT UPPER(name), LENGTH(name) FROM users",
		},
		{
			Name: "select_with_case_when",
			SQL:  "SELECT id, CASE WHEN price > 100 THEN 'high' ELSE 'low' END FROM orders",
		},
		// --- Subqueries ----------------------------------------------------
		{
			Name: "select_with_in_subquery",
			SQL:  "SELECT id FROM orders WHERE customer IN (SELECT id FROM users WHERE active = TRUE)",
		},
		{
			Name: "select_with_exists_subquery",
			SQL:  "SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM users WHERE users.id = orders.customer)",
		},
		// --- Multi-aggregate GROUP BY -------------------------------------
		{
			Name: "select_group_by_multi_agg",
			SQL:  "SELECT status, COUNT(*), SUM(price), AVG(price) FROM orders GROUP BY status",
		},
		// --- Catalog-aware shapes (RFC-022 §4.-1 Phase 3) -----------------
		// These queries carry a SchemaTemplate so the Go side routes
		// through buildLogicalPlanFor*WithCatalog and emits real
		// cascades.predicates.QueryPredicate trees in Explain output. Without the
		// SchemaTemplate the Go side falls back to text-only PredicateText
		// — these entries are the regression sentinels for the
		// catalog-aware rendering.
		{
			Name: "catalog_select_eq_filter",
			SQL:  "SELECT id FROM Item WHERE val = 5",
			SchemaTemplate: "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, " +
				"PRIMARY KEY (id))",
		},
		{
			Name: "catalog_select_arith_filter",
			SQL:  "SELECT id FROM Item WHERE val + 1 > 10",
			SchemaTemplate: "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, " +
				"PRIMARY KEY (id))",
		},
		{
			Name: "catalog_delete_filter",
			SQL:  "DELETE FROM Item WHERE val = 0",
			SchemaTemplate: "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, " +
				"PRIMARY KEY (id))",
		},
		{
			Name: "catalog_update_filter",
			SQL:  "UPDATE Item SET val = 100 WHERE id = 1",
			SchemaTemplate: "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, " +
				"PRIMARY KEY (id))",
		},
		{
			// Pins the derived-table WHERE path landed nightshift-50:
			// `(SELECT col, col FROM real) AS x WHERE x.col = ?` synth-
			// esises a virtual ScopeSource so the WHERE walks through
			// the catalog-aware path (rather than degrading to text
			// fallback as it did pre-this-shift).
			Name: "catalog_derived_table_where",
			SQL:  "SELECT id FROM (SELECT id, val FROM Item) AS x WHERE val = 5",
			SchemaTemplate: "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, " +
				"PRIMARY KEY (id))",
		},
		{
			// AND-chain WHERE — exercises the multi-leaf catalog walker
			// + simplifier composition (each leaf goes through the
			// walker, then the simplifier dedups / folds).
			Name: "catalog_and_where",
			SQL:  "SELECT id FROM Item WHERE val > 5 AND val < 100",
			SchemaTemplate: "CREATE TABLE Item (id BIGINT NOT NULL, val BIGINT, " +
				"PRIMARY KEY (id))",
		},
	}
}
