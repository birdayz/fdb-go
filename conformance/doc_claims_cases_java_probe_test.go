//go:build bazelrunfiles

package conformance_test

// docClaimCase is one claim about Java in DIVERGENCES.md ("Go-Only Extensions")
// or SQL_CONFORMANCE.md, named by its row. setup runs after the schema's own.
type docClaimCase struct {
	name, schema string
	setup        []string
	sql          string
}

// nullV gives t a NULL v, for the cases that measure where NULLs sort.
const nullV = "INSERT INTO t VALUES (4, NULL, 'd', 0.5, true, 4)"

var docClaimCases = []docClaimCase{
	// DML.
	{name: "select", schema: "main", sql: "SELECT id, v FROM t WHERE id = 1"},
	{name: "insert_select", schema: "main", setup: []string{"INSERT INTO u SELECT id + 10, id, v FROM t WHERE id = 2"}, sql: "SELECT id, t_id, w FROM u WHERE id = 12"},
	{name: "insert_select_column_list", schema: "main", setup: []string{"INSERT INTO u (id, t_id, w) SELECT id + 10, id, v FROM t WHERE id = 2"}, sql: "SELECT id FROM u WHERE id = 12"},
	{name: "update", schema: "main", setup: []string{"UPDATE t SET v = v + 1 WHERE id = 1"}, sql: "SELECT v FROM t WHERE id = 1"},
	{name: "delete", schema: "main", setup: []string{"DELETE FROM t WHERE id = 1"}, sql: "SELECT id FROM t"},
	// Expressions.
	{name: "arithmetic", schema: "main", sql: "SELECT v + 1 AS a, v - 1 AS b, v * 2 AS c, v / 3 AS e, v % 3 AS f FROM t WHERE id = 2"},
	{name: "searched_case", schema: "main", sql: "SELECT CASE WHEN v > 15 THEN 'hi' ELSE 'lo' END AS c FROM t WHERE id = 2"},
	{name: "simple_case", schema: "main", sql: "SELECT CASE v WHEN 10 THEN 'ten' ELSE 'other' END AS c FROM t WHERE id = 1"},
	{name: "simple_case_no_else", schema: "main", sql: "SELECT CASE v WHEN 10 THEN 'ten' WHEN 20 THEN 'twenty' END AS c FROM t WHERE id = 1"},
	{name: "cast", schema: "main", sql: "SELECT CAST(v AS STRING) AS c FROM t WHERE id = 1"},
	{name: "cast_overflow", schema: "main", sql: "SELECT CAST(9223372036854775807 AS INTEGER) AS c FROM t WHERE id = 1"},
	{name: "coalesce", schema: "main", sql: "SELECT COALESCE(b, true) AS c FROM t WHERE id = 3"},
	{name: "nullif", schema: "main", sql: "SELECT NULLIF(v, 10) AS c FROM t WHERE id = 1"},
	{name: "greatest_least", schema: "main", sql: "SELECT GREATEST(v, 15) AS g, LEAST(v, 15) AS l FROM t WHERE id = 1"},
	{name: "cardinality", schema: "main", sql: "SELECT CARDINALITY(xs) AS n FROM arr WHERE id = 1"},
	{name: "upper", schema: "main", sql: "SELECT UPPER(s) AS c FROM t WHERE id = 1"},
	{name: "position_in", schema: "main", sql: "SELECT POSITION('zz' IN s) AS p FROM t WHERE id = 1"},
	{name: "abs", schema: "main", sql: "SELECT ABS(d) AS c FROM t WHERE id = 2"},
	{name: "current_timestamp", schema: "main", sql: "SELECT CURRENT_TIMESTAMP IS NOT NULL AS c FROM t WHERE id = 1"},
	{name: "current_date", schema: "main", sql: "SELECT CURRENT_DATE IS NOT NULL AS c FROM t WHERE id = 1"},
	{name: "date_part", schema: "main", sql: "SELECT YEAR(CURRENT_DATE) > 2000 AS c FROM t WHERE id = 1"},
	{name: "date_part_of_integer", schema: "main", sql: "SELECT YEAR(v) AS y FROM t WHERE id = 1"},
	{name: "extract_year", schema: "main", sql: "SELECT EXTRACT(YEAR FROM v) AS y FROM t WHERE id = 1"},
	{name: "cast_date", schema: "main", sql: "SELECT CAST('2024-03-15' AS DATE) AS c FROM t WHERE id = 1"},
	{name: "cast_timestamp", schema: "main", sql: "SELECT CAST('2024-03-15 10:00:00' AS TIMESTAMP) AS c FROM t WHERE id = 1"},
	{name: "symbolic_and", schema: "main", sql: "SELECT id FROM t WHERE v = 10 && id = 1"},
	{name: "symbolic_or", schema: "main", sql: "SELECT id FROM t WHERE v = 20 || id = 1"},
	{name: "xor", schema: "main", sql: "SELECT id FROM t WHERE (v = 10) XOR (id = 1)"},
	// Predicates.
	{name: "and_or_not", schema: "main", sql: "SELECT id FROM t WHERE NOT (v = 10 AND id = 1) OR id = 1"},
	{name: "between", schema: "main", sql: "SELECT id FROM t WHERE v BETWEEN 5 AND 15"},
	{name: "in_list", schema: "main", sql: "SELECT id FROM t WHERE v IN (10, 30)"},
	{name: "in_subquery", schema: "main", sql: "SELECT id FROM t WHERE id IN (SELECT t_id FROM u)"},
	{name: "like", schema: "main", sql: "SELECT id FROM t WHERE s LIKE 'a%'"},
	{name: "is_null", schema: "main", sql: "SELECT id FROM t WHERE b IS NULL"},
	{name: "is_distinct_from", schema: "main", sql: "SELECT id FROM t WHERE b IS DISTINCT FROM true"},
	{name: "byte_literal", schema: "main", sql: "SELECT id FROM bt WHERE bs = x'cafe'"},
	// Aggregation.
	{name: "group_by_ordered", schema: "main", sql: "SELECT v, COUNT(*) AS n FROM t GROUP BY v"},
	{name: "group_by_primary_key", schema: "main", sql: "SELECT id, COUNT(*) AS n FROM t GROUP BY id"},
	{name: "group_by_unordered", schema: "main", sql: "SELECT s, COUNT(*) AS n FROM t GROUP BY s"},
	{name: "group_by_expression", schema: "main", sql: "SELECT v + 1 AS k, COUNT(*) AS n FROM t GROUP BY v + 1"},
	{name: "aggregates", schema: "main", sql: "SELECT COUNT(*) AS c, SUM(v) AS s, AVG(v) AS a, MIN(v) AS mn, MAX(v) AS mx FROM t"},
	{name: "having", schema: "main", sql: "SELECT v, COUNT(*) AS n FROM t GROUP BY v HAVING COUNT(*) > 1"},
	{name: "count_distinct", schema: "main", sql: "SELECT COUNT(DISTINCT v) AS n FROM t"},
	{name: "empty_aggregates", schema: "main", sql: "SELECT COUNT(*) AS c, SUM(v) AS s FROM t WHERE id > 100"},
	// Set operations.
	{name: "union_all", schema: "main", sql: "SELECT id FROM t UNION ALL SELECT id FROM u"},
	{name: "union_distinct", schema: "main", sql: "SELECT id FROM t UNION SELECT id FROM u"},
	{name: "intersect", schema: "main", sql: "SELECT id FROM t INTERSECT SELECT id FROM u"},
	{name: "except", schema: "main", sql: "SELECT id FROM t EXCEPT SELECT id FROM u"},
	{name: "union_order_by_position", schema: "main", sql: "SELECT id FROM t UNION ALL SELECT id FROM u ORDER BY 1"},
	// Joins.
	{name: "inner_join", schema: "main", sql: "SELECT t.id, u.id FROM t JOIN u ON t.id = u.t_id"},
	{name: "comma_join", schema: "main", sql: "SELECT t.id, u.id FROM t, u WHERE t.id = u.t_id"},
	{name: "cross_join", schema: "main", sql: "SELECT t.id, u.id FROM t CROSS JOIN u"},
	{name: "left_join", schema: "main", sql: "SELECT t.id, u.id FROM t LEFT JOIN u ON t.id = u.t_id"},
	{name: "right_join", schema: "main", sql: "SELECT t.id, u.id FROM t RIGHT JOIN u ON t.id = u.t_id"},
	{name: "full_join", schema: "main", sql: "SELECT t.id, u.id FROM t FULL OUTER JOIN u ON t.id = u.t_id"},
	{name: "full_join_without_outer", schema: "main", sql: "SELECT u.id FROM t FULL JOIN u ON u.t_id = 1"},
	{name: "self_join", schema: "main", sql: "SELECT a.id, b.id FROM t a, t b WHERE a.v = b.v AND a.id < b.id"},
	{name: "three_way_join", schema: "main", sql: "SELECT a.id, b.id, c.id FROM t a, u b, t c WHERE a.id = b.t_id AND c.id = a.id"},
	// Subqueries.
	{name: "scalar_subquery", schema: "main", sql: "SELECT id, (SELECT MAX(v) FROM t) AS m FROM t"},
	{name: "correlated_scalar_subquery", schema: "main", sql: "SELECT id, (SELECT MAX(w) FROM u WHERE u.t_id = t.id) AS m FROM t"},
	{name: "exists", schema: "main", sql: "SELECT id FROM t WHERE EXISTS (SELECT id FROM u WHERE u.t_id = t.id)"},
	{name: "not_exists", schema: "main", sql: "SELECT id FROM t WHERE NOT EXISTS (SELECT id FROM u WHERE u.t_id = t.id)"},
	{name: "exists_projection", schema: "main", sql: "SELECT id, EXISTS (SELECT id FROM u WHERE u.t_id = t.id) AS e FROM t"},
	{name: "derived_table", schema: "main", sql: "SELECT x.k FROM (SELECT id AS k FROM t) AS x WHERE x.k > 1"},
	{name: "unnest", schema: "main", sql: "SELECT arr.id, e FROM arr, arr.xs AS e"},
	{name: "unnest_ordinality", schema: "main", sql: "SELECT arr.id, e, at FROM arr, arr.xs AS e AT at"},
	// CTEs.
	{name: "with", schema: "main", sql: "WITH c AS (SELECT id FROM t) SELECT id FROM c"},
	{name: "with_column_rename", schema: "main", sql: "WITH c(k) AS (SELECT id FROM t) SELECT k FROM c"},
	{name: "with_chained", schema: "main", sql: "WITH c1 AS (SELECT id FROM t), c2 AS (SELECT id FROM c1 WHERE id > 1) SELECT id FROM c2"},
	{name: "recursive_level_order", schema: "tree", sql: "WITH RECURSIVE c(id, parent) AS (SELECT id, parent FROM t1 WHERE id = 1 UNION ALL SELECT b.id, b.parent FROM t1 AS b, c WHERE b.parent = c.id) TRAVERSAL ORDER LEVEL_ORDER SELECT id FROM c"},
	{name: "recursive_pre_order", schema: "tree", sql: "WITH RECURSIVE c(id, parent) AS (SELECT id, parent FROM t1 WHERE id = 1 UNION ALL SELECT b.id, b.parent FROM t1 AS b, c WHERE b.parent = c.id) TRAVERSAL ORDER PRE_ORDER SELECT id FROM c"},
	{name: "recursive_union_distinct", schema: "tree", sql: "WITH RECURSIVE c(id, parent) AS (SELECT id, parent FROM t1 WHERE id = 1 UNION SELECT b.id, b.parent FROM t1 AS b, c WHERE b.parent = c.id) SELECT id FROM c"},
	{name: "recursive_union_distinct_diamond", schema: "diamond", sql: "WITH RECURSIVE c AS (SELECT id FROM e WHERE id = 1 UNION SELECT e.id FROM e, c WHERE e.parent = c.id) SELECT id FROM c"},
	// Ordering, limiting, distinct.
	{name: "order_by_index", schema: "main", sql: "SELECT v FROM t ORDER BY v"},
	{name: "order_by_no_index", schema: "main", sql: "SELECT id, s FROM t ORDER BY s"},
	{name: "order_by_aggregate", schema: "main", sql: "SELECT v, COUNT(*) AS n FROM t GROUP BY v ORDER BY COUNT(*)"},
	{name: "order_by_nulls_first", schema: "main", setup: []string{nullV}, sql: "SELECT v FROM t ORDER BY v NULLS FIRST"},
	{name: "order_by_nulls_last", schema: "main", setup: []string{nullV}, sql: "SELECT v FROM t ORDER BY v NULLS LAST"},
	{name: "limit", schema: "main", sql: "SELECT id FROM t ORDER BY id LIMIT 2"},
	{name: "limit_offset", schema: "main", sql: "SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 1"},
	{name: "limit_in_subquery", schema: "main", sql: "SELECT x.k FROM (SELECT id AS k FROM t LIMIT 1) AS x"},
	{name: "distinct_ordered", schema: "main", sql: "SELECT DISTINCT v FROM t"},
	{name: "distinct_unordered", schema: "main", sql: "SELECT DISTINCT k FROM g"},
	{name: "distinct_order_by", schema: "main", sql: "SELECT DISTINCT v FROM t ORDER BY v"},
	// Schema and types.
	{name: "core_types", schema: "main", sql: "SELECT id, v, s, d, b, i FROM t WHERE id = 1"},
	{name: "bytes", schema: "main", sql: "SELECT bs FROM bt WHERE id = 1"},
	{name: "array_column", schema: "main", sql: "SELECT xs FROM arr WHERE id = 1"},
	{name: "date_column", schema: "date_column", sql: "SELECT id FROM dt"},
	{name: "timestamp_column", schema: "timestamp_column", sql: "SELECT id FROM ts"},
	{name: "scalar_not_null", schema: "scalar_not_null", sql: "SELECT id FROM nn"},
	{name: "information_schema_tables", schema: "main", sql: "SELECT TABLE_NAME FROM INFORMATION_SCHEMA.\"TABLES\" WHERE TABLE_NAME = 'T'"},
	{name: "information_schema_columns", schema: "main", sql: "SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = 'T' AND COLUMN_NAME = 'V'"},
	// Error codes.
	{name: "unknown_table", schema: "main", sql: "SELECT id FROM nosuch"},
	{name: "unknown_column", schema: "main", sql: "SELECT nosuch FROM t"},
	{name: "ambiguous_column", schema: "main", sql: "SELECT id FROM t, u"},
	{name: "unknown_qualifier", schema: "main", sql: "SELECT x.id FROM t"},
	{name: "comparison_type_mismatch", schema: "main", sql: "SELECT id FROM t WHERE s = 1"},
	{name: "between_type_mismatch", schema: "main", sql: "SELECT id FROM t WHERE s BETWEEN 1 AND 2"},
	{name: "in_list_type_mismatch", schema: "main", sql: "SELECT id FROM t WHERE s IN (1, 2)"},
	{name: "group_by_violation", schema: "main", sql: "SELECT id, COUNT(*) AS n FROM t GROUP BY v"},
	{name: "duplicate_order_by", schema: "main", sql: "SELECT v FROM t ORDER BY v, v"},
	{name: "union_column_mismatch", schema: "main", sql: "SELECT id FROM t UNION ALL SELECT id, w FROM u"},
	{name: "union_type_mismatch", schema: "main", sql: "SELECT id FROM t UNION ALL SELECT s FROM t"},
	{name: "not_null_violation", schema: "array_not_null", setup: []string{"INSERT INTO na VALUES (1, NULL)"}, sql: "SELECT id FROM na"},
	{name: "not_null_omitted", schema: "array_not_null", setup: []string{"INSERT INTO na (id) VALUES (1)"}, sql: "SELECT id FROM na"},
	{name: "primary_key_violation", schema: "main", setup: []string{"INSERT INTO t VALUES (1, 1, 'x', 0.0, true, 1)"}, sql: "SELECT id FROM t"},
	{name: "unique_index_violation", schema: "main", setup: []string{"INSERT INTO u VALUES (9, 1, 5)"}, sql: "SELECT id FROM u"},
	{name: "integer_overflow", schema: "main", sql: "SELECT v * 9223372036854775807 AS x FROM t WHERE id = 1"},
	{name: "int32_overflow", schema: "main", setup: []string{"INSERT INTO t VALUES (9, 1, 'z', 0.0, true, 3000000000)"}, sql: "SELECT i FROM t WHERE id = 9"},
	{name: "unknown_function", schema: "main", sql: "SELECT nosuchfn(v) FROM t"},
	{name: "unknown_table_function", schema: "main", sql: "SELECT * FROM nosuchtvf(1)"},
}
