package plandiff

// SeedRunCorpus is the runSql parallel of SeedCorpus: a small set of
// (schema, setup-DMLs, SELECT, expected RowSet) cases for the result-
// set diff harness. Each entry carries the EXPECTED Java-side output
// inline, so a regression surfaces with a precise per-entry diff
// rather than "the corpus changed somewhere."
//
// Today only the Java side runs (Go is gated on Track C2). The same
// Expected RowSet doubles as the cross-engine reference: when the Go
// runner lands, every entry becomes a Go-vs-Java equivalence check.
//
// Each entry's SetupSqls must produce deterministic state — SELECTs
// without ORDER BY can't be added until we trust both engines'
// row-order semantics match. Today every entry orders by primary key.
//
// JSON note: Expected.Rows uses Go's `any` slice. Numbers MUST be
// `float64` (encoding/json's default for JSON numbers); `int` literals
// would compare unequal because the wire-arrived values are float64.
type RunQuery struct {
	Name           string
	SetupSqls      []string
	Query          string
	SchemaTemplate string
	// Expected is the Java-side RowSet this entry must produce. Captured
	// from real fdb-relational output and pinned here. Update
	// deliberately when the corpus query / setup / Java behaviour
	// changes intentionally.
	Expected RowSet
}

// SeedRunCorpus returns the baseline RunQuery set. Add entries that
// exercise distinct primitive types or row-shape edge cases (NULL
// handling, multi-row, empty, single-column, multi-column).
//
// fdb-relational quirks that show up in Expected:
//   - All identifiers uppercased ("ID" not "id").
//   - Type names: BIGINT / STRING / BOOLEAN / DOUBLE / BYTES.
//   - Anonymous projections (constant exprs, COUNT(*)) get synthetic
//     column names "_0", "_1", ... in declaration order.
func SeedRunCorpus() []RunQuery {
	return []RunQuery{
		{
			Name:           "single_row_bigint",
			SchemaTemplate: "CREATE TABLE T1 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{"INSERT INTO T1 VALUES (42)"},
			Query:          "SELECT id FROM T1 ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}},
				Rows:    [][]any{{float64(42)}},
			},
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
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "NAME", Type: "STRING"}},
				Rows: [][]any{
					{float64(1), "alice"},
					{float64(2), "bob"},
					{float64(3), "carol"},
				},
			},
		},
		{
			Name:           "null_string",
			SchemaTemplate: "CREATE TABLE T3 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T3 VALUES (1, 'alice')",
				"INSERT INTO T3 VALUES (2, NULL)",
			},
			Query: "SELECT id, name FROM T3 ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "NAME", Type: "STRING"}},
				Rows: [][]any{
					{float64(1), "alice"},
					{float64(2), nil},
				},
			},
		},
		{
			Name:           "empty_table",
			SchemaTemplate: "CREATE TABLE T4 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      nil,
			Query:          "SELECT id FROM T4",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}},
				Rows:    [][]any{},
			},
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
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "FLAG", Type: "BOOLEAN"}},
				Rows: [][]any{
					{float64(1), true},
					{float64(2), false},
					{float64(3), nil},
				},
			},
		},
		{
			Name:           "double_column",
			SchemaTemplate: "CREATE TABLE T6 (id BIGINT, val DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T6 VALUES (1, 1.5)",
				"INSERT INTO T6 VALUES (2, -7.25)",
			},
			Query: "SELECT id, val FROM T6 ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "VAL", Type: "DOUBLE"}},
				Rows: [][]any{
					{float64(1), 1.5},
					{float64(2), -7.25},
				},
			},
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
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}},
				Rows:    [][]any{{float64(3)}, {float64(2)}, {float64(1)}},
			},
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
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "VAL", Type: "BIGINT"}},
				Rows: [][]any{
					{float64(2), float64(200)},
					{float64(3), float64(300)},
				},
			},
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
			Expected: RowSet{
				Columns: []Column{
					{Name: "REGION", Type: "STRING"},
					{Name: "ID", Type: "BIGINT"},
					{Name: "NAME", Type: "STRING"},
				},
				Rows: [][]any{
					{"eu", float64(1), "carol"},
					{"us", float64(1), "alice"},
					{"us", float64(2), "bob"},
				},
			},
		},
		{
			Name:           "select_constant_expr",
			SchemaTemplate: "CREATE TABLE T10 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      []string{"INSERT INTO T10 VALUES (5)"},
			Query:          "SELECT id, id + 10 FROM T10",
			Expected: RowSet{
				// fdb-relational synthesises "_<n>" names for anonymous
				// projection slots in declaration order.
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "_1", Type: "BIGINT"}},
				Rows:    [][]any{{float64(5), float64(15)}},
			},
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
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "NAME", Type: "STRING"}},
				Rows:    [][]any{{float64(2), "banana"}},
			},
		},
		{
			Name: "inner_join",
			SchemaTemplate: "CREATE TABLE Users (uid BIGINT, name STRING, PRIMARY KEY (uid)) " +
				"CREATE TABLE Orders (oid BIGINT, uid BIGINT, total BIGINT, PRIMARY KEY (oid))",
			SetupSqls: []string{
				"INSERT INTO Users VALUES (1, 'alice')",
				"INSERT INTO Users VALUES (2, 'bob')",
				"INSERT INTO Orders VALUES (10, 1, 100)",
				"INSERT INTO Orders VALUES (11, 1, 200)",
				"INSERT INTO Orders VALUES (12, 2, 300)",
			},
			Query: "SELECT u.name, o.total FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid",
			Expected: RowSet{
				Columns: []Column{{Name: "NAME", Type: "STRING"}, {Name: "TOTAL", Type: "BIGINT"}},
				Rows: [][]any{
					{"alice", float64(100)},
					{"alice", float64(200)},
					{"bob", float64(300)},
				},
			},
		},
		{
			Name:           "count_aggregate",
			SchemaTemplate: "CREATE TABLE T12 (id BIGINT, region STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T12 VALUES (1, 'us')",
				"INSERT INTO T12 VALUES (2, 'us')",
				"INSERT INTO T12 VALUES (3, 'eu')",
			},
			Query: "SELECT count(*) FROM T12",
			Expected: RowSet{
				Columns: []Column{{Name: "_0", Type: "BIGINT"}},
				Rows:    [][]any{{float64(3)}},
			},
		},
		{
			Name:           "like_pattern",
			SchemaTemplate: "CREATE TABLE T13 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T13 VALUES (1, 'apple')",
				"INSERT INTO T13 VALUES (2, 'apricot')",
				"INSERT INTO T13 VALUES (3, 'banana')",
			},
			Query: "SELECT id, name FROM T13 WHERE name LIKE 'ap%' ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "NAME", Type: "STRING"}},
				Rows: [][]any{
					{float64(1), "apple"},
					{float64(2), "apricot"},
				},
			},
		},
		{
			Name:           "in_list",
			SchemaTemplate: "CREATE TABLE T14 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T14 VALUES (1, 'a')",
				"INSERT INTO T14 VALUES (2, 'b')",
				"INSERT INTO T14 VALUES (3, 'c')",
				"INSERT INTO T14 VALUES (4, 'd')",
			},
			Query: "SELECT id, name FROM T14 WHERE id IN (1, 3) ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "NAME", Type: "STRING"}},
				Rows: [][]any{
					{float64(1), "a"},
					{float64(3), "c"},
				},
			},
		},
		// GROUP BY <col> deferred: fdb-relational 4.11.1.0's Cascades
		// planner returns UnableToPlanException for "SELECT region,
		// count(*) FROM T GROUP BY region". The unaggregated count(*)
		// works (see count_aggregate above). Re-add when the planner
		// learns the GROUP BY rule.
		{
			Name:           "and_predicate",
			SchemaTemplate: "CREATE TABLE T16 (id BIGINT, region STRING, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T16 VALUES (1, 'us', 100)",
				"INSERT INTO T16 VALUES (2, 'us', 200)",
				"INSERT INTO T16 VALUES (3, 'eu', 100)",
				"INSERT INTO T16 VALUES (4, 'eu', 200)",
			},
			Query: "SELECT id FROM T16 WHERE region = 'us' AND val > 150 ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}},
				Rows:    [][]any{{float64(2)}},
			},
		},
		{
			Name:           "or_predicate",
			SchemaTemplate: "CREATE TABLE T17 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T17 VALUES (1, 10)",
				"INSERT INTO T17 VALUES (2, 20)",
				"INSERT INTO T17 VALUES (3, 30)",
				"INSERT INTO T17 VALUES (4, 40)",
			},
			Query: "SELECT id FROM T17 WHERE val = 10 OR val = 30 ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}},
				Rows:    [][]any{{float64(1)}, {float64(3)}},
			},
		},
		{
			Name:           "is_null",
			SchemaTemplate: "CREATE TABLE T18 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T18 VALUES (1, 'alice')",
				"INSERT INTO T18 VALUES (2, NULL)",
				"INSERT INTO T18 VALUES (3, 'bob')",
				"INSERT INTO T18 VALUES (4, NULL)",
			},
			Query: "SELECT id FROM T18 WHERE name IS NULL ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}},
				Rows:    [][]any{{float64(2)}, {float64(4)}},
			},
		},
		{
			Name:           "is_not_null",
			SchemaTemplate: "CREATE TABLE T19 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T19 VALUES (1, 'alice')",
				"INSERT INTO T19 VALUES (2, NULL)",
				"INSERT INTO T19 VALUES (3, 'bob')",
			},
			Query: "SELECT id, name FROM T19 WHERE name IS NOT NULL ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "NAME", Type: "STRING"}},
				Rows: [][]any{
					{float64(1), "alice"},
					{float64(3), "bob"},
				},
			},
		},
		{
			Name:           "comparison_ops",
			SchemaTemplate: "CREATE TABLE T20 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T20 VALUES (1, 10)",
				"INSERT INTO T20 VALUES (2, 20)",
				"INSERT INTO T20 VALUES (3, 30)",
			},
			Query: "SELECT id FROM T20 WHERE val >= 20 AND val < 30 ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}},
				Rows:    [][]any{{float64(2)}},
			},
		},
		// LIMIT deferred: fdb-relational 4.11.1.0 returns
		// `RelationalException: LIMIT clause is not supported.` —
		// it's a JDBC-only knob exposed via Statement.setMaxRows.
		// Re-add when the planner adds LIMIT-as-SQL support.
		// SELECT DISTINCT deferred: fdb-relational 4.11.1.0's Cascades
		// planner returns UnableToPlanException for "SELECT DISTINCT
		// region FROM T". Re-add when the planner ports the
		// distinct rule (RFC-022 §4.5).
		// Common SQL scalar string functions (lower/upper/length, etc.)
		// are NOT registered in fdb-relational 4.11.1.0:
		// `RelationalException: Unsupported operator lower`. Re-add
		// when the function registry expands.
		{
			Name:           "case_expression",
			SchemaTemplate: "CREATE TABLE T_CASE (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CASE VALUES (1, 5)",
				"INSERT INTO T_CASE VALUES (2, 15)",
				"INSERT INTO T_CASE VALUES (3, 25)",
			},
			Query: "SELECT id, CASE WHEN val < 10 THEN 'low' WHEN val < 20 THEN 'mid' ELSE 'high' END FROM T_CASE ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "_1", Type: "STRING"}},
				Rows: [][]any{
					{float64(1), "low"},
					{float64(2), "mid"},
					{float64(3), "high"},
				},
			},
		},
		{
			Name:           "between",
			SchemaTemplate: "CREATE TABLE T_BTW (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BTW VALUES (1, 5)",
				"INSERT INTO T_BTW VALUES (2, 10)",
				"INSERT INTO T_BTW VALUES (3, 15)",
				"INSERT INTO T_BTW VALUES (4, 20)",
			},
			Query: "SELECT id FROM T_BTW WHERE val BETWEEN 10 AND 15 ORDER BY id",
			Expected: RowSet{
				Columns: []Column{{Name: "ID", Type: "BIGINT"}},
				Rows:    [][]any{{float64(2)}, {float64(3)}},
			},
		},
		{
			Name:           "math_in_projection",
			SchemaTemplate: "CREATE TABLE T22 (id BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T22 VALUES (1, 3, 5)",
				"INSERT INTO T22 VALUES (2, 7, 2)",
			},
			Query: "SELECT id, x + y, x * y FROM T22 ORDER BY id",
			Expected: RowSet{
				Columns: []Column{
					{Name: "ID", Type: "BIGINT"},
					{Name: "_1", Type: "BIGINT"},
					{Name: "_2", Type: "BIGINT"},
				},
				Rows: [][]any{
					{float64(1), float64(8), float64(15)},
					{float64(2), float64(9), float64(14)},
				},
			},
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
