package plandiff

// SeedRunCorpus is the runSql parallel of SeedCorpus: a small set of
// (schema, setup-DMLs, SELECT) cases for the result-set diff harness.
// The conformance test runs each entry through BOTH engines (Java
// fdb-relational + Go embedded) and asserts byte-equivalent column
// metadata + row values.
//
// Each entry's SetupSqls must produce deterministic state — SELECTs
// without ORDER BY can't be added until we trust both engines'
// row-order semantics match. Today every entry orders by primary key.
//
// Adding a new entry: just append {Name, SchemaTemplate, SetupSqls,
// Query}. No baseline RowSet to capture or paste — the cross-engine
// equivalence check is the assertion.
type RunQuery struct {
	Name           string
	SetupSqls      []string
	Query          string
	SchemaTemplate string
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
			Name:           "update_then_select",
			SchemaTemplate: "CREATE TABLE T_UPD (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UPD VALUES (1, 100)",
				"INSERT INTO T_UPD VALUES (2, 200)",
				"INSERT INTO T_UPD VALUES (3, 300)",
				"UPDATE T_UPD SET val = val + 1 WHERE id = 2",
			},
			Query: "SELECT id, val FROM T_UPD ORDER BY id",
		},
		{
			Name:           "delete_then_select",
			SchemaTemplate: "CREATE TABLE T_DEL (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DEL VALUES (1, 10)",
				"INSERT INTO T_DEL VALUES (2, 20)",
				"INSERT INTO T_DEL VALUES (3, 30)",
				"DELETE FROM T_DEL WHERE id = 2",
			},
			Query: "SELECT id, val FROM T_DEL ORDER BY id",
		},
		// INFORMATION_SCHEMA.TABLES probe deferred: fdb-relational's
		// SELECT parser doesn't recognize the schema-qualified
		// reference (syntax error on TABLES). Track A4 needs a
		// different probe shape — investigate the catalog access
		// path in a follow-up shift.
		// LEFT JOIN deferred: fdb-relational 4.11.1.0 returns
		// `RelationalException: Attempting to query non existing
		// column CUSTOMERS.CID` — the planner's column resolution
		// for JOIN ON clauses doesn't see PK columns at the
		// ON-clause level. Inner join via `FROM A, B WHERE` works
		// (see inner_join entry above); explicit JOIN ON syntax
		// has a planner gap. Re-add when the planner ports the
		// JOIN-ON resolution rule.
		{
			Name:           "null_in_equality",
			SchemaTemplate: "CREATE TABLE T_NEQ (id BIGINT, x BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NEQ VALUES (1, 5)",
				"INSERT INTO T_NEQ VALUES (2, NULL)",
				"INSERT INTO T_NEQ VALUES (3, 5)",
			},
			// SQL Kleene 3VL: x = 5 is UNKNOWN when x IS NULL —
			// UNKNOWN is filtered out (treated as false) by WHERE.
			// Only id=1 and id=3 (where x is genuinely 5) match.
			Query: "SELECT id FROM T_NEQ WHERE x = 5 ORDER BY id",
		},
		{
			Name:           "null_arithmetic",
			SchemaTemplate: "CREATE TABLE T_NA (id BIGINT, x BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NA VALUES (1, 5)",
				"INSERT INTO T_NA VALUES (2, NULL)",
			},
			Query: "SELECT id, x + 10 FROM T_NA ORDER BY id",
		},
		{
			Name:           "math_in_where",
			SchemaTemplate: "CREATE TABLE T_MATH (id BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_MATH VALUES (1, 5, 5)", // 5+5=10 > 9 ✓
				"INSERT INTO T_MATH VALUES (2, 3, 4)", // 3+4=7  > 9 ✗
				"INSERT INTO T_MATH VALUES (3, 4, 6)", // 4+6=10 > 9 ✓
			},
			Query: "SELECT id FROM T_MATH WHERE x + y > 9 ORDER BY id",
		},
		{
			Name:           "subquery_in_from",
			SchemaTemplate: "CREATE TABLE T_SUB (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_SUB VALUES (1, 100)",
				"INSERT INTO T_SUB VALUES (2, 200)",
				"INSERT INTO T_SUB VALUES (3, 300)",
			},
			Query: "SELECT t.id FROM (SELECT id, val FROM T_SUB WHERE val > 100) AS t WHERE t.val < 300 ORDER BY t.id",
		},
		{
			Name:           "sum_min_max",
			SchemaTemplate: "CREATE TABLE T_AGG (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_AGG VALUES (1, 10)",
				"INSERT INTO T_AGG VALUES (2, 20)",
				"INSERT INTO T_AGG VALUES (3, 30)",
			},
			Query: "SELECT sum(val), min(val), max(val) FROM T_AGG",
		},
		{
			Name:           "uuid_round_trip",
			SchemaTemplate: "CREATE TABLE T_UUID (id BIGINT, key UUID, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UUID VALUES (1, CAST('00000000-0000-0000-0000-000000000042' AS UUID))",
			},
			Query: "SELECT id, key FROM T_UUID ORDER BY id",
		},
		{
			Name:           "case_expression",
			SchemaTemplate: "CREATE TABLE T_CASE (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CASE VALUES (1, 5)",
				"INSERT INTO T_CASE VALUES (2, 15)",
				"INSERT INTO T_CASE VALUES (3, 25)",
			},
			Query: "SELECT id, CASE WHEN val < 10 THEN 'low' WHEN val < 20 THEN 'mid' ELSE 'high' END FROM T_CASE ORDER BY id",
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
		},
		{
			Name:           "math_in_projection",
			SchemaTemplate: "CREATE TABLE T22 (id BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T22 VALUES (1, 3, 5)",
				"INSERT INTO T22 VALUES (2, 7, 2)",
			},
			Query: "SELECT id, x + y, x * y FROM T22 ORDER BY id",
		},
		{
			// Negative numbers — pins big-negative + small-negative
			// preservation through the proto INT64 round-trip + JSON
			// coercion. fdb-relational supports unary minus on
			// INSERT but the parser routes it through arithmetic
			// rather than as a literal sign, so the value flows
			// through the cast path.
			Name:           "negative_numbers",
			SchemaTemplate: "CREATE TABLE T_NEG (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NEG VALUES (1, -1)",
				"INSERT INTO T_NEG VALUES (2, -9223372036854775807)",
				"INSERT INTO T_NEG VALUES (3, 0)",
			},
			Query: "SELECT id, val FROM T_NEG ORDER BY id",
		},
		{
			// Double-precision arithmetic — pins DOUBLE preservation
			// (and rules out the INT64 ↔ FLOAT64 collapse the runner
			// used to do before ColumnTypeDatabaseTypeName landed).
			Name:           "double_arithmetic",
			SchemaTemplate: "CREATE TABLE T_DARITH (id BIGINT, x DOUBLE, y DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DARITH VALUES (1, 1.5, 2.5)",
				"INSERT INTO T_DARITH VALUES (2, 0.1, 0.2)",
			},
			Query: "SELECT id, x + y FROM T_DARITH ORDER BY id",
		},
		{
			// UUID equality predicate — pins WHERE on a UUID column.
			// Forces the planner to compare UUIDs at the proto-message
			// level (not as raw bytes), and tests that CAST in WHERE
			// reaches the same canonicalization as CAST in INSERT.
			Name: "uuid_where_equality",
			SchemaTemplate: "CREATE TABLE T_UUW (id BIGINT, key UUID, " +
				"PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UUW VALUES (1, CAST('11111111-1111-1111-1111-111111111111' AS UUID))",
				"INSERT INTO T_UUW VALUES (2, CAST('22222222-2222-2222-2222-222222222222' AS UUID))",
			},
			Query: "SELECT id FROM T_UUW WHERE key = CAST('22222222-2222-2222-2222-222222222222' AS UUID)",
		},
		{
			// COUNT distinct (no DISTINCT keyword) of a string col.
			Name:           "count_with_filter",
			SchemaTemplate: "CREATE TABLE T_CF (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CF VALUES (1, 'a')",
				"INSERT INTO T_CF VALUES (2, 'b')",
				"INSERT INTO T_CF VALUES (3, 'a')",
			},
			Query: "SELECT count(*) FROM T_CF WHERE name = 'a'",
		},
		{
			// Multi-condition AND with negation — pins the WHERE
			// composition through three predicates plus NOT.
			Name:           "where_not_and",
			SchemaTemplate: "CREATE TABLE T_NA (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NA VALUES (1, 100)",
				"INSERT INTO T_NA VALUES (2, 200)",
				"INSERT INTO T_NA VALUES (3, 300)",
				"INSERT INTO T_NA VALUES (4, 400)",
			},
			Query: "SELECT id FROM T_NA WHERE val > 100 AND NOT val = 300 ORDER BY id",
		},
		{
			// INTEGER (32-bit) round-trip — exercises INSERT range-check
			// + storage as proto INT32 + read-back narrowing. Bare ints
			// are typed BIGINT in fdb-relational so explicit CAST is
			// required for INSERT into INTEGER columns.
			Name:           "integer_column",
			SchemaTemplate: "CREATE TABLE T_INT (id BIGINT, val INTEGER, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_INT VALUES (1, CAST(2147483647 AS INTEGER))",
				"INSERT INTO T_INT VALUES (2, CAST(-2147483648 AS INTEGER))",
				"INSERT INTO T_INT VALUES (3, CAST(0 AS INTEGER))",
			},
			Query: "SELECT id, val FROM T_INT ORDER BY id",
		},
		{
			// FLOAT (32-bit) round-trip — DOUBLE-typed literals narrowed
			// via CAST into FLOAT storage, read back as float32 promoted
			// to float64 on the wire.
			Name:           "float_column",
			SchemaTemplate: "CREATE TABLE T_FLT (id BIGINT, val FLOAT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_FLT VALUES (1, CAST(1.5 AS FLOAT))",
				"INSERT INTO T_FLT VALUES (2, CAST(0.0 AS FLOAT))",
			},
			Query: "SELECT id, val FROM T_FLT ORDER BY id",
		},
		{
			// BYTES round-trip with X'...' hex literal — pins the byte
			// → base64 wire encoding both engines emit. "hi" → 0x6869
			// → base64 "aGk=".
			Name:           "bytes_round_trip",
			SchemaTemplate: "CREATE TABLE T_BYTES (id BIGINT, payload BYTES, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BYTES VALUES (1, X'6869')",
				"INSERT INTO T_BYTES VALUES (2, X'')",
			},
			Query: "SELECT id, payload FROM T_BYTES ORDER BY id",
		},
		{
			// String comparison: ORDER-preserving WHERE on STRING.
			// Tests that lexicographic byte-order matches across both
			// engines.
			Name:           "string_comparison",
			SchemaTemplate: "CREATE TABLE T_SCMP (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_SCMP VALUES (1, 'apple')",
				"INSERT INTO T_SCMP VALUES (2, 'banana')",
				"INSERT INTO T_SCMP VALUES (3, 'cherry')",
				"INSERT INTO T_SCMP VALUES (4, 'date')",
			},
			Query: "SELECT id, name FROM T_SCMP WHERE name > 'b' AND name < 'd' ORDER BY id",
		},
		{
			// SUM over DOUBLE — pins DOUBLE-aggregate result type +
			// floating-point accumulation order across engines.
			Name:           "sum_double",
			SchemaTemplate: "CREATE TABLE T_SD (id BIGINT, val DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_SD VALUES (1, 1.5)",
				"INSERT INTO T_SD VALUES (2, 2.25)",
				"INSERT INTO T_SD VALUES (3, -0.75)",
			},
			Query: "SELECT sum(val) FROM T_SD",
		},
		// Multi-column ORDER BY (any direction combination) hits an
		// UnableToPlanException in fdb-relational 4.11.1.0's Cascades
		// planner — single-column ORDER BY only. Documented in
		// CLAUDE.md "Java↔Go conformance gotchas". Re-add when the
		// planner supports it.
		{
			// CASE with multiple WHEN branches + ELSE — pins branch
			// evaluation order and result-type lattice over multi-WHEN.
			Name:           "case_multi_when",
			SchemaTemplate: "CREATE TABLE T_CMW (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CMW VALUES (1, 5)",
				"INSERT INTO T_CMW VALUES (2, 15)",
				"INSERT INTO T_CMW VALUES (3, 25)",
				"INSERT INTO T_CMW VALUES (4, 35)",
			},
			Query: "SELECT id, CASE WHEN val < 10 THEN 'tiny' WHEN val < 20 THEN 'small' WHEN val < 30 THEN 'medium' ELSE 'large' END FROM T_CMW ORDER BY id",
		},
		{
			// LIKE with _ single-char wildcard — pins the per-char
			// matching, distinct from % multi-char.
			Name:           "like_underscore",
			SchemaTemplate: "CREATE TABLE T_LU (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_LU VALUES (1, 'cat')",
				"INSERT INTO T_LU VALUES (2, 'cot')",
				"INSERT INTO T_LU VALUES (3, 'cart')",
				"INSERT INTO T_LU VALUES (4, 'cup')",
			},
			Query: "SELECT id, name FROM T_LU WHERE name LIKE 'c_t' ORDER BY id",
		},
		{
			// NULL three-valued logic — `WHERE val = NULL` returns no
			// rows (NULL = NULL is UNKNOWN, not TRUE). Pins Kleene
			// propagation across engines.
			Name:           "null_eq_yields_empty",
			SchemaTemplate: "CREATE TABLE T_NE (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NE VALUES (1, 10)",
				"INSERT INTO T_NE VALUES (2, NULL)",
				"INSERT INTO T_NE VALUES (3, 30)",
			},
			Query: "SELECT id FROM T_NE WHERE val = NULL ORDER BY id",
		},
		{
			// Nested arithmetic — pins associativity / operator
			// precedence across engines.
			Name:           "nested_arithmetic",
			SchemaTemplate: "CREATE TABLE T_NEST (id BIGINT, x BIGINT, y BIGINT, z BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NEST VALUES (1, 2, 3, 4)",
				"INSERT INTO T_NEST VALUES (2, 5, 6, 7)",
			},
			Query: "SELECT id, (x + y) * z, x + y * z FROM T_NEST ORDER BY id",
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
