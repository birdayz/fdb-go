package plandiff

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
		// cascades.QueryPredicate trees in Explain output. Without the
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
	}
}
