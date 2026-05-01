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
//
// ExpectErrorContains turns the entry into a NEGATIVE test. When
// non-empty, the harness asserts BOTH engines fail (any error) AND
// each engine's error message contains the substring. Catches cases
// where one engine accidentally accepts a query the other rejects
// (or vice versa) — the kind of silent divergence that's invisible
// to a success-path-only harness.
//
// ExpectErrorMessage is the STRICTER alternative — when set, both
// engines must fail AND each engine's core error message must
// literally EQUAL this string (Go's `api.Error.Message`, Java's
// `JavaError.Message` — i.e. the unwrapped server-side root-cause
// text). Use when the message is intentionally aligned verbatim
// between engines (e.g. shift / NULLIF / aggregate-type-mismatch
// rejections, where Go was changed to emit the exact phrasing Java
// emits). Stronger than ExpectErrorContains because no slack —
// catches drift the moment either engine reorders / rephrases its
// message. Mutually exclusive with ExpectErrorContains.
type RunQuery struct {
	Name           string
	SetupSqls      []string
	Query          string
	SchemaTemplate string
	// ExpectErrorContains: when set, both Java and Go must fail with
	// an error message containing this substring. Empty = expect
	// success (unless ExpectErrorMessage is set).
	ExpectErrorContains string
	// ExpectErrorMessage: when set, both engines must fail AND each
	// engine's core error message string equals this value verbatim.
	// Stronger than ExpectErrorContains.
	ExpectErrorMessage string
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
		{
			// BOOLEAN column with explicit `= TRUE` comparison.
			// fdb-relational rejects bare `WHERE flag` ("expected
			// BooleanValue but got FieldValue") — boolean columns
			// must be compared explicitly.
			Name:           "boolean_in_where",
			SchemaTemplate: "CREATE TABLE T_BWH (id BIGINT, flag BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BWH VALUES (1, TRUE)",
				"INSERT INTO T_BWH VALUES (2, FALSE)",
				"INSERT INTO T_BWH VALUES (3, TRUE)",
			},
			Query: "SELECT id FROM T_BWH WHERE flag = TRUE ORDER BY id",
		},
		{
			// SUM over INTEGER (32-bit) operand — pins SUM result-type
			// inheritance for INTEGER (different from BIGINT).
			Name:           "sum_integer",
			SchemaTemplate: "CREATE TABLE T_SI (id BIGINT, val INTEGER, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_SI VALUES (1, CAST(10 AS INTEGER))",
				"INSERT INTO T_SI VALUES (2, CAST(20 AS INTEGER))",
				"INSERT INTO T_SI VALUES (3, CAST(30 AS INTEGER))",
			},
			Query: "SELECT sum(val) FROM T_SI",
		},
		// MIN/MAX over non-numeric types (STRING, BYTES, BOOLEAN)
		// is unsupported by fdb-relational 4.11.1.0:
		// "VerifyException: unable to encapsulate aggregate operation
		// due to type mismatch(es)". Numeric MIN/MAX only — pinned by
		// sum_min_max above.
		{
			// Three-way AND chain — pins multi-leaf AND simplification.
			Name:           "three_way_and",
			SchemaTemplate: "CREATE TABLE T_TWA (id BIGINT, x BIGINT, y BIGINT, z BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_TWA VALUES (1, 1, 1, 1)",
				"INSERT INTO T_TWA VALUES (2, 1, 1, 0)",
				"INSERT INTO T_TWA VALUES (3, 1, 0, 1)",
				"INSERT INTO T_TWA VALUES (4, 1, 1, 1)",
			},
			Query: "SELECT id FROM T_TWA WHERE x = 1 AND y = 1 AND z = 1 ORDER BY id",
		},
		{
			// Negative literal in WHERE — exercises the
			// NegativeDecimalConstant lexer path against signed BIGINT.
			Name:           "negative_in_where",
			SchemaTemplate: "CREATE TABLE T_NW (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NW VALUES (1, -50)",
				"INSERT INTO T_NW VALUES (2, 0)",
				"INSERT INTO T_NW VALUES (3, 50)",
			},
			Query: "SELECT id, val FROM T_NW WHERE val >= -10 ORDER BY id",
		},
		{
			// CASE returning INTEGER branches with explicit cast —
			// pins the CASE-result type lattice over INTEGER (which
			// is narrower than the implicit BIGINT default).
			Name:           "case_integer_branches",
			SchemaTemplate: "CREATE TABLE T_CIB (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CIB VALUES (1, 5)",
				"INSERT INTO T_CIB VALUES (2, 15)",
			},
			Query: "SELECT id, CASE WHEN val < 10 THEN CAST(1 AS INTEGER) ELSE CAST(2 AS INTEGER) END FROM T_CIB ORDER BY id",
		},
		{
			// Mixed-type comparison: BIGINT col vs DOUBLE literal.
			// Both engines must promote BIGINT → DOUBLE for the
			// comparison without erroring.
			Name:           "bigint_vs_double_literal",
			SchemaTemplate: "CREATE TABLE T_BDL (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BDL VALUES (1, 10)",
				"INSERT INTO T_BDL VALUES (2, 20)",
				"INSERT INTO T_BDL VALUES (3, 30)",
			},
			Query: "SELECT id FROM T_BDL WHERE val > 15.5 ORDER BY id",
		},
		{
			// IS TRUE / IS FALSE — distinct from `= TRUE` because
			// IS-predicates collapse UNKNOWN to FALSE rather than
			// propagating it. Pinned both directions.
			Name:           "is_true",
			SchemaTemplate: "CREATE TABLE T_IT (id BIGINT, flag BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_IT VALUES (1, TRUE)",
				"INSERT INTO T_IT VALUES (2, FALSE)",
				"INSERT INTO T_IT VALUES (3, NULL)",
			},
			Query: "SELECT id FROM T_IT WHERE flag IS TRUE ORDER BY id",
		},
		{
			// IS FALSE — collapses NULL to FALSE.
			Name:           "is_false",
			SchemaTemplate: "CREATE TABLE T_IF (id BIGINT, flag BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_IF VALUES (1, TRUE)",
				"INSERT INTO T_IF VALUES (2, FALSE)",
				"INSERT INTO T_IF VALUES (3, NULL)",
			},
			Query: "SELECT id FROM T_IF WHERE flag IS FALSE ORDER BY id",
		},
		{
			// UPDATE multiple columns + WHERE — pins multi-SET
			// semantics through the round-trip.
			Name:           "update_multi_set",
			SchemaTemplate: "CREATE TABLE T_UMS (id BIGINT, x BIGINT, y STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UMS VALUES (1, 10, 'a')",
				"INSERT INTO T_UMS VALUES (2, 20, 'b')",
				"INSERT INTO T_UMS VALUES (3, 30, 'c')",
				"UPDATE T_UMS SET x = 100, y = 'updated' WHERE id = 2",
			},
			Query: "SELECT id, x, y FROM T_UMS ORDER BY id",
		},
		{
			// DELETE with multi-condition WHERE — pins compound
			// predicate evaluation in DELETE.
			Name:           "delete_compound_where",
			SchemaTemplate: "CREATE TABLE T_DCW (id BIGINT, region STRING, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DCW VALUES (1, 'us', 100)",
				"INSERT INTO T_DCW VALUES (2, 'us', 200)",
				"INSERT INTO T_DCW VALUES (3, 'eu', 100)",
				"INSERT INTO T_DCW VALUES (4, 'eu', 200)",
				"DELETE FROM T_DCW WHERE region = 'us' AND val = 100",
			},
			Query: "SELECT id FROM T_DCW ORDER BY id",
		},
		{
			// BYTES equality in WHERE — pins byte-array comparison
			// across engines (Java compares the raw bytes, not the
			// base64 representation).
			Name:           "bytes_where_equal",
			SchemaTemplate: "CREATE TABLE T_BWE (id BIGINT, payload BYTES, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BWE VALUES (1, X'01')",
				"INSERT INTO T_BWE VALUES (2, X'02')",
				"INSERT INTO T_BWE VALUES (3, X'03')",
			},
			Query: "SELECT id FROM T_BWE WHERE payload = X'02' ORDER BY id",
		},
		{
			// Nested CASE: outer CASE references inner CASE result.
			// Pins recursive type-inference through CASE result.
			Name:           "nested_case",
			SchemaTemplate: "CREATE TABLE T_NC (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NC VALUES (1, 5)",
				"INSERT INTO T_NC VALUES (2, 15)",
				"INSERT INTO T_NC VALUES (3, 25)",
			},
			Query: "SELECT id, CASE WHEN val < 20 THEN CASE WHEN val < 10 THEN 'tiny' ELSE 'small' END ELSE 'big' END FROM T_NC ORDER BY id",
		},
		// ===== Negative entries (ExpectErrorContains set) =====
		// Each entry below pins a query both engines must reject AND
		// reject with a matching error substring. Catches silent
		// acceptance on one side (e.g., Go quietly returning [] vs
		// Java erroring 42703).
		{
			// Reference to a column that doesn't exist on the table
			// → both engines surface the column name in the error.
			// Substring matched case-insensitively in the test (each
			// engine has its own identifier-case convention in error
			// messages — Java uppercases per fdb-relational spec, Go
			// preserves user-typed case at the error site).
			Name:                "undefined_column",
			SchemaTemplate:      "CREATE TABLE T_UC (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:           []string{"INSERT INTO T_UC VALUES (1)"},
			Query:               "SELECT no_such_col FROM T_UC",
			ExpectErrorContains: "no_such_col",
		},
		{
			// Bare `WHERE flag` on a BOOLEAN column. fdb-relational
			// rejects with "expected BooleanValue but got FieldValue"
			// — the planner refuses to treat a column reference as a
			// predicate. Pinning this as a negative test ensures Go
			// rejects it the same way; if Go silently accepted (e.g.
			// by coercing the FieldValue to bool), this test surfaces
			// the divergence.
			Name:                "bare_bool_where_rejected",
			SchemaTemplate:      "CREATE TABLE T_BBW (id BIGINT, flag BOOLEAN, PRIMARY KEY (id))",
			SetupSqls:           []string{"INSERT INTO T_BBW VALUES (1, TRUE)"},
			Query:               "SELECT id FROM T_BBW WHERE flag",
			ExpectErrorContains: "BooleanValue",
		},
		{
			// Reference to a table that doesn't exist → both engines
			// must reject. Substring matched case-insensitively.
			Name:                "undefined_table",
			SchemaTemplate:      "CREATE TABLE T_UT (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:           nil,
			Query:               "SELECT id FROM no_such_table",
			ExpectErrorContains: "no_such_table",
		},
		{
			// Bit-shift `<<` — fdb-relational 4.11.1.0 tokenizes the
			// operator but has no entry in the function registry, so
			// its planner returns the verbatim string
			// "Unsupported operator <<" (CLAUDE.md gotcha). Go's
			// embedded engine matches by NOT having `<<` / `>>` cases
			// in `ApplyBitOp`, AND by emitting the SAME exact message
			// "Unsupported operator <<" from the default arm. Same
			// architectural reason in both engines: function
			// registry has no evaluator for shift operators.
			//
			// `ExpectErrorMessage` requires the core error string to
			// match VERBATIM on both sides — Java's
			// `JavaError.Message` and Go's `api.Error.Message` —
			// proving alignment at the message level. Per-entry
			// isolation (fresh Java server spawned just for this
			// entry) prevents pollution from prior negative entries.
			Name:               "bitshift_left_rejected",
			SchemaTemplate:     "CREATE TABLE T_BSL (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls:          []string{"INSERT INTO T_BSL VALUES (1, 7)"},
			Query:              "SELECT v << 2 FROM T_BSL WHERE id = 1",
			ExpectErrorMessage: "Unsupported operator <<",
		},
		{
			// Bit-shift `>>` — symmetric to `<<` above. Both engines
			// emit the verbatim string "Unsupported operator >>".
			Name:               "bitshift_right_rejected",
			SchemaTemplate:     "CREATE TABLE T_BSR (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls:          []string{"INSERT INTO T_BSR VALUES (1, 8)"},
			Query:              "SELECT v >> 1 FROM T_BSR WHERE id = 1",
			ExpectErrorMessage: "Unsupported operator >>",
		},
		{
			// NULLIF — fdb-relational 4.11.1.0's function registry
			// has no entry, so its planner returns the verbatim
			// string "Unsupported operator NULLIF" (CLAUDE.md
			// gotcha). Go's embedded engine matches by NOT having a
			// NULLIF arm in the scalar-function evaluator switch
			// AND emitting the SAME exact message
			// "Unsupported operator NULLIF" from the default arm.
			// Same architectural reason in both engines: function
			// registry has no NULLIF evaluator. Workaround:
			// `CASE WHEN a = b THEN NULL ELSE a END`.
			Name:               "nullif_rejected",
			SchemaTemplate:     "CREATE TABLE T_NIF (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls:          []string{"INSERT INTO T_NIF VALUES (1, 5)"},
			Query:              "SELECT NULLIF(v, 5) FROM T_NIF WHERE id = 1",
			ExpectErrorMessage: "Unsupported operator NULLIF",
		},
		// NOTE: ORDER BY <alias> on a non-natural-order column is
		// rejected by BOTH engines — Java with UnableToPlanException
		// (Cascades has no rule to satisfy the ordering); Go with
		// `ErrCodeUnsupportedSort` (the structural mirror landed
		// nightshift-60 — "no scan satisfies"). Same architectural
		// reason in both engines: no rule generates a satisfying
		// physical sort. Java's error message references its
		// exception class ("UnableToPlanException"); Go's references
		// the column ("ORDER BY amount cannot be satisfied"). The
		// rejection alignment is real (Go nightshift-60 work) but
		// the surface messages differ enough that
		// `ExpectErrorContains` needs a column-name substring rather
		// than a Java-internal class name. Pinned cross-engine via
		// the existing yamsql ORDER BY rejection scenarios
		// (`order_by_expression`, `order_by_dupe_col`,
		// `order_by_limit`, etc.); not a separate corpus entry.
		// NOTE: `WHERE (boolean_expr)` with bare parens is a known
		// one-sided divergence: Java rejects with "expected
		// BooleanValue but got RecordConstructorValue" because its
		// parser treats `(...)` as a record/tuple constructor unless
		// it's in a context that forces predicate parsing
		// (CLAUDE.md gotcha "WHERE (boolean_expr) with bare
		// parentheses is rejected"). Go's embedded engine accepts
		// the form. Probed nightshift-61 — Java rejection confirmed.
		// Aligning Go to also reject would invalidate Go-only tests
		// using parenthesised boolean WHERE shapes; defer to a
		// dedicated cleanup shift. Until then, redundant parens
		// around a boolean predicate is a Go-only feature.
		// NOTE: `WITH RECURSIVE name AS (non-self-referencing-body)` is
		// a known one-sided divergence: Java rejects with
		// "condition is not met!" because it requires an actual
		// UNION ALL self-reference for RECURSIVE (CLAUDE.md gotcha).
		// SQL spec + Postgres permit the non-self-referencing body
		// (RECURSIVE is a scope enabler, not a requirement). Go's
		// embedded engine matches SQL spec / Postgres, so it accepts.
		// Probed nightshift-61 — Java rejection confirmed. Aligning
		// Go to also reject would invalidate yamsql `recursive_cte`
		// scenarios that use the non-self-referencing form. Defer
		// to a dedicated cleanup shift.
		// NOTE: `LIMIT N` clause is a known one-sided divergence: Java
		// rejects standalone `... LIMIT N` (pagination is JDBC-only via
		// `Statement.setMaxRows`, per CLAUDE.md gotcha "LIMIT clause
		// is not supported in SQL"). Go's embedded engine implements
		// LIMIT directly. Aligning Go would invalidate dozens of
		// LIMIT-using yamsql + sqldriver tests; defer to a dedicated
		// cleanup shift. Probed nightshift-61 — Java's rejection
		// confirmed.
		// NOTE: `SELECT 1+1` (FROM-less SELECT for constant projection)
		// is a known one-sided divergence: Java rejects standalone
		// FROM-less SELECT with UnableToPlan (CLAUDE.md gotcha
		// "SELECT <expr> without FROM is unsupported by the planner")
		// but ACCEPTS the same form inside CTE base cases like
		// `WITH RECURSIVE counter(n) AS (SELECT 1 AS n UNION ALL ...)`.
		// Go's embedded engine accepts both contexts uniformly.
		// Aligning Go to reject standalone FROM-less SELECT while
		// continuing to accept the CTE base case requires context-
		// aware parsing (separate parseSelectQuery entry points or a
		// flag) — deferred as a separate large-scope conformance
		// task. Probed nightshift-61.
		// NOTE: `col IN (SELECT ...)` is a known one-sided divergence:
		// Java NPEs on the form (visitor walks `ExpressionsContext`
		// which is null when the IN list comes from a subquery, per
		// CLAUDE.md gotcha "`col IN (SELECT ...)` parser-NPEs in
		// fdb-relational"); Go's embedded engine implements it
		// correctly. Aligning Go to reject would invalidate ~14
		// Go-side test files (yamsql + sqldriver) that exercise the
		// feature; deferred as a separate large-scope conformance
		// task. Until then, IN-subquery is a Go-only feature with
		// no cross-engine corpus entry.
		// NOTE: COUNT/SUM/AVG/MIN/MAX with DISTINCT are rejected by
		// BOTH engines — Java NPEs (visitor unconditionally calls
		// `AggregateWindowedFunctionContext.ALL().getText()` which is
		// null when DISTINCT is present); Go's embedded engine rejects
		// at execution time with `ErrCodeUnsupportedOperation`
		// "DISTINCT aggregate %s is not supported"
		// (`pkg/relational/core/embedded/aggregate.go` and
		// `select_query_full.go`). Same architectural reason in both
		// engines: visitor doesn't handle the DISTINCT case.
		//
		// NOT included as a cross-engine corpus entry because Java's
		// NPE message references the parser-internal class name
		// (`AggregateWindowedFunctionContext.ALL()`) rather than the
		// SQL keyword `DISTINCT`, so it can't share a meaningful
		// substring with Go's clean error message. The rejection
		// alignment is pinned on the Go side via
		// `count_distinct.yaml`, `count_distinct_join.yaml`,
		// `distinct_aggregates.yaml`, `aggregate_expr.yaml`, and
		// `group_by_validation.yaml`'s `error_code: "0A000"` tests
		// under the yamsql harness.
		// NOTE: explicit CROSS JOIN syntax (`a CROSS JOIN b`) is rejected
		// in BOTH engines — Java NPEs (InnerJoinContext.expression()
		// null-dereference in the visitor); Go's embedded engine
		// rejects at parse time with `ErrCodeUnsupportedOperation`
		// "explicit CROSS JOIN syntax is not supported"
		// (`select_parser.go#extractJoinClause`). Same architectural
		// reason in both engines: the visitor's CROSS-JOIN code path
		// doesn't exist. Workaround: comma-join `FROM a, b`.
		//
		// NOT included as a cross-engine corpus entry because Java's
		// NPE message (`Cannot invoke ... InnerJoinContext.expression()`)
		// and Go's clean error message can't share a meaningful
		// substring without aligning Go to mimic Java's panic-style
		// failure (which would be a regression in Go's UX). The
		// rejection alignment is pinned on the Go side via
		// `cross_join.yaml`'s `error_code: "0A000"` test under the
		// yamsql harness; Java's NPE behaviour is documented in
		// CLAUDE.md "Java↔Go conformance gotchas" §Parser bugs.
		{
			// MIN over a non-numeric (STRING) column — fdb-relational
			// 4.11.1.0's function registry only installs numeric
			// MIN / MAX overloads; non-numeric input raises
			// VerifyException with the verbatim message
			// "unable to encapsulate aggregate operation due to type
			// mismatch(es)" (CLAUDE.md gotcha). Go's embedded engine
			// matches at runtime via the `requireMinMaxNumeric` gate
			// (`pkg/relational/core/embedded/aggregate.go`) which
			// returns `ErrCodeUnsupportedOperation` with the SAME
			// verbatim message. Per-entry isolation prevents
			// fdb-relational's type-mismatch error-path state-leak
			// from stalling this spec under load.
			Name:               "min_over_string_rejected",
			SchemaTemplate:     "CREATE TABLE T_MOS (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls:          []string{"INSERT INTO T_MOS VALUES (1, 'alice')"},
			Query:              "SELECT MIN(name) FROM T_MOS",
			ExpectErrorMessage: "unable to encapsulate aggregate operation due to type mismatch(es)",
		},
		{
			// COUNT(*) with predicate over JOIN — exercises the
			// JOIN-with-aggregate path. Comma-separated FROM (only
			// inner-join syntax supported by fdb-relational).
			Name: "count_star_join",
			SchemaTemplate: "CREATE TABLE Buyers (bid BIGINT, name STRING, PRIMARY KEY (bid)) " +
				"CREATE TABLE Sales (sid BIGINT, bid BIGINT, amt BIGINT, PRIMARY KEY (sid))",
			SetupSqls: []string{
				"INSERT INTO Buyers VALUES (1, 'alice')",
				"INSERT INTO Buyers VALUES (2, 'bob')",
				"INSERT INTO Sales VALUES (10, 1, 100)",
				"INSERT INTO Sales VALUES (11, 1, 200)",
				"INSERT INTO Sales VALUES (12, 2, 300)",
				"INSERT INTO Sales VALUES (13, 2, 400)",
			},
			Query: "SELECT count(*) FROM Buyers b, Sales s WHERE b.bid = s.bid AND s.amt > 150",
		},

		// ===== Per-type round-trip + WHERE coverage =====
		{
			Name:           "bigint_where_eq",
			SchemaTemplate: "CREATE TABLE T_BEQ (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BEQ VALUES (1, 100)",
				"INSERT INTO T_BEQ VALUES (2, 200)",
				"INSERT INTO T_BEQ VALUES (3, 100)",
			},
			Query: "SELECT id FROM T_BEQ WHERE v = 100 ORDER BY id",
		},
		{
			Name:           "integer_where_lt",
			SchemaTemplate: "CREATE TABLE T_IWL (id BIGINT, v INTEGER, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_IWL VALUES (1, CAST(50 AS INTEGER))",
				"INSERT INTO T_IWL VALUES (2, CAST(100 AS INTEGER))",
				"INSERT INTO T_IWL VALUES (3, CAST(150 AS INTEGER))",
			},
			Query: "SELECT id, v FROM T_IWL WHERE v < CAST(120 AS INTEGER) ORDER BY id",
		},
		{
			Name:           "double_where_range",
			SchemaTemplate: "CREATE TABLE T_DWR (id BIGINT, v DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DWR VALUES (1, 0.5)",
				"INSERT INTO T_DWR VALUES (2, 1.5)",
				"INSERT INTO T_DWR VALUES (3, 2.5)",
				"INSERT INTO T_DWR VALUES (4, 3.5)",
			},
			Query: "SELECT id, v FROM T_DWR WHERE v >= 1.0 AND v <= 3.0 ORDER BY id",
		},
		{
			Name:           "float_where_eq",
			SchemaTemplate: "CREATE TABLE T_FWE (id BIGINT, v FLOAT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_FWE VALUES (1, CAST(1.0 AS FLOAT))",
				"INSERT INTO T_FWE VALUES (2, CAST(2.0 AS FLOAT))",
			},
			Query: "SELECT id FROM T_FWE WHERE v = CAST(1.0 AS FLOAT) ORDER BY id",
		},
		{
			Name:           "string_long",
			SchemaTemplate: "CREATE TABLE T_SLG (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_SLG VALUES (1, 'the quick brown fox jumps over the lazy dog')",
				"INSERT INTO T_SLG VALUES (2, '')",
				"INSERT INTO T_SLG VALUES (3, NULL)",
			},
			Query: "SELECT id, s FROM T_SLG ORDER BY id",
		},
		// Apostrophe-escape semantics diverge: Go correctly unescapes
		// SQL-standard doubled apostrophe (`'it''s'` → `it's`); Java's
		// fdb-relational stores the literal doubled form. Skip until
		// the upstream behaviour aligns.
		{
			Name:           "boolean_select_distinct_constants",
			SchemaTemplate: "CREATE TABLE T_BSD (id BIGINT, v BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BSD VALUES (1, TRUE)",
				"INSERT INTO T_BSD VALUES (2, FALSE)",
			},
			Query: "SELECT id, v FROM T_BSD ORDER BY id",
		},
		{
			Name:           "uuid_null",
			SchemaTemplate: "CREATE TABLE T_UNULL (id BIGINT, key UUID, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UNULL VALUES (1, CAST('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa' AS UUID))",
				"INSERT INTO T_UNULL VALUES (2, NULL)",
			},
			Query: "SELECT id, key FROM T_UNULL ORDER BY id",
		},

		// ===== Comparison operator coverage =====
		{
			Name:           "comparison_neq",
			SchemaTemplate: "CREATE TABLE T_NEQ (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NEQ VALUES (1, 10)",
				"INSERT INTO T_NEQ VALUES (2, 20)",
				"INSERT INTO T_NEQ VALUES (3, 30)",
			},
			Query: "SELECT id FROM T_NEQ WHERE v != 20 ORDER BY id",
		},
		{
			Name:           "comparison_neq_alt",
			SchemaTemplate: "CREATE TABLE T_NEA (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NEA VALUES (1, 10)",
				"INSERT INTO T_NEA VALUES (2, 20)",
			},
			Query: "SELECT id FROM T_NEA WHERE v <> 20 ORDER BY id",
		},
		{
			Name:           "comparison_lte",
			SchemaTemplate: "CREATE TABLE T_LTE (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_LTE VALUES (1, 10)",
				"INSERT INTO T_LTE VALUES (2, 20)",
				"INSERT INTO T_LTE VALUES (3, 30)",
			},
			Query: "SELECT id FROM T_LTE WHERE v <= 20 ORDER BY id",
		},
		{
			Name:           "comparison_gte",
			SchemaTemplate: "CREATE TABLE T_GTE (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_GTE VALUES (1, 10)",
				"INSERT INTO T_GTE VALUES (2, 20)",
				"INSERT INTO T_GTE VALUES (3, 30)",
			},
			Query: "SELECT id FROM T_GTE WHERE v >= 20 ORDER BY id",
		},

		// ===== LIKE coverage =====
		{
			Name:           "like_prefix_only",
			SchemaTemplate: "CREATE TABLE T_LP (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_LP VALUES (1, 'apple')",
				"INSERT INTO T_LP VALUES (2, 'application')",
				"INSERT INTO T_LP VALUES (3, 'banana')",
			},
			Query: "SELECT id FROM T_LP WHERE s LIKE 'app%' ORDER BY id",
		},
		{
			Name:           "like_suffix_only",
			SchemaTemplate: "CREATE TABLE T_LS (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_LS VALUES (1, 'foo.txt')",
				"INSERT INTO T_LS VALUES (2, 'bar.txt')",
				"INSERT INTO T_LS VALUES (3, 'baz.log')",
			},
			Query: "SELECT id FROM T_LS WHERE s LIKE '%.txt' ORDER BY id",
		},
		{
			Name:           "like_contains",
			SchemaTemplate: "CREATE TABLE T_LC (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_LC VALUES (1, 'hello world')",
				"INSERT INTO T_LC VALUES (2, 'goodbye')",
				"INSERT INTO T_LC VALUES (3, 'hello there')",
			},
			Query: "SELECT id FROM T_LC WHERE s LIKE '%hello%' ORDER BY id",
		},
		{
			Name:           "not_like",
			SchemaTemplate: "CREATE TABLE T_NL (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL VALUES (1, 'apple')",
				"INSERT INTO T_NL VALUES (2, 'banana')",
				"INSERT INTO T_NL VALUES (3, 'cherry')",
			},
			Query: "SELECT id FROM T_NL WHERE s NOT LIKE 'a%' ORDER BY id",
		},

		// ===== IN list variants =====
		{
			Name:           "in_string_list",
			SchemaTemplate: "CREATE TABLE T_ISL (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_ISL VALUES (1, 'a')",
				"INSERT INTO T_ISL VALUES (2, 'b')",
				"INSERT INTO T_ISL VALUES (3, 'c')",
			},
			Query: "SELECT id FROM T_ISL WHERE s IN ('a', 'c') ORDER BY id",
		},
		{
			Name:           "not_in_list",
			SchemaTemplate: "CREATE TABLE T_NIL (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NIL VALUES (1, 10)",
				"INSERT INTO T_NIL VALUES (2, 20)",
				"INSERT INTO T_NIL VALUES (3, 30)",
				"INSERT INTO T_NIL VALUES (4, 40)",
			},
			Query: "SELECT id FROM T_NIL WHERE v NOT IN (20, 40) ORDER BY id",
		},

		// ===== BETWEEN variants =====
		{
			Name:           "not_between",
			SchemaTemplate: "CREATE TABLE T_NBT (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NBT VALUES (1, 5)",
				"INSERT INTO T_NBT VALUES (2, 15)",
				"INSERT INTO T_NBT VALUES (3, 25)",
				"INSERT INTO T_NBT VALUES (4, 35)",
			},
			Query: "SELECT id FROM T_NBT WHERE v NOT BETWEEN 10 AND 30 ORDER BY id",
		},

		// ===== Aggregates =====
		{
			Name:           "count_with_join_filter",
			SchemaTemplate: "CREATE TABLE T_CWJ (id BIGINT, region STRING, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CWJ VALUES (1, 'us', 10)",
				"INSERT INTO T_CWJ VALUES (2, 'us', 20)",
				"INSERT INTO T_CWJ VALUES (3, 'eu', 30)",
				"INSERT INTO T_CWJ VALUES (4, 'eu', 40)",
			},
			Query: "SELECT count(*) FROM T_CWJ WHERE region = 'us'",
		},
		{
			Name:           "min_max_double",
			SchemaTemplate: "CREATE TABLE T_MMD (id BIGINT, v DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_MMD VALUES (1, 1.5)",
				"INSERT INTO T_MMD VALUES (2, 0.5)",
				"INSERT INTO T_MMD VALUES (3, 2.5)",
			},
			Query: "SELECT min(v), max(v) FROM T_MMD",
		},
		{
			Name:           "sum_with_filter",
			SchemaTemplate: "CREATE TABLE T_SWF (id BIGINT, region STRING, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_SWF VALUES (1, 'us', 100)",
				"INSERT INTO T_SWF VALUES (2, 'us', 200)",
				"INSERT INTO T_SWF VALUES (3, 'eu', 300)",
			},
			Query: "SELECT sum(v) FROM T_SWF WHERE region = 'us'",
		},

		// ===== Arithmetic =====
		{
			Name:           "subtraction_in_projection",
			SchemaTemplate: "CREATE TABLE T_SUB (id BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_SUB VALUES (1, 10, 3)",
				"INSERT INTO T_SUB VALUES (2, 20, 5)",
			},
			Query: "SELECT id, x - y FROM T_SUB ORDER BY id",
		},
		{
			Name:           "division_integer",
			SchemaTemplate: "CREATE TABLE T_DIV (id BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DIV VALUES (1, 10, 3)",
				"INSERT INTO T_DIV VALUES (2, 20, 4)",
			},
			Query: "SELECT id, x / y FROM T_DIV ORDER BY id",
		},
		{
			Name:           "arithmetic_with_constant",
			SchemaTemplate: "CREATE TABLE T_AWC (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_AWC VALUES (1, 5)",
				"INSERT INTO T_AWC VALUES (2, 10)",
			},
			Query: "SELECT id, v * 2 + 1 FROM T_AWC ORDER BY id",
		},

		// ===== ORDER BY variants =====
		// ORDER BY on a SELECT-list alias unsupported by fdb-relational
		// 4.11.1.0's planner (UnableToPlanException). Use the underlying
		// column name in ORDER BY instead.
		// ORDER BY on a non-projected column also unsupported by
		// fdb-relational's planner. The ORDER BY column must appear
		// in the SELECT list.

		// ===== JOIN variants =====
		{
			Name: "three_table_join",
			SchemaTemplate: "CREATE TABLE Customers (cid BIGINT, name STRING, PRIMARY KEY (cid)) " +
				"CREATE TABLE Orders (oid BIGINT, cid BIGINT, pid BIGINT, PRIMARY KEY (oid)) " +
				"CREATE TABLE Products (pid BIGINT, pname STRING, PRIMARY KEY (pid))",
			SetupSqls: []string{
				"INSERT INTO Customers VALUES (1, 'alice')",
				"INSERT INTO Customers VALUES (2, 'bob')",
				"INSERT INTO Products VALUES (10, 'widget')",
				"INSERT INTO Products VALUES (20, 'gadget')",
				"INSERT INTO Orders VALUES (100, 1, 10)",
				"INSERT INTO Orders VALUES (101, 2, 20)",
			},
			Query: "SELECT c.name, p.pname FROM Customers c, Orders o, Products p WHERE c.cid = o.cid AND o.pid = p.pid ORDER BY o.oid",
		},

		// ===== NULL semantics =====
		{
			Name:           "null_in_in_list",
			SchemaTemplate: "CREATE TABLE T_NII (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NII VALUES (1, 1)",
				"INSERT INTO T_NII VALUES (2, NULL)",
				"INSERT INTO T_NII VALUES (3, 3)",
			},
			Query: "SELECT id FROM T_NII WHERE v IN (1, 3) ORDER BY id",
		},
		{
			Name:           "null_in_between",
			SchemaTemplate: "CREATE TABLE T_NIB (id BIGINT, v BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NIB VALUES (1, 5)",
				"INSERT INTO T_NIB VALUES (2, NULL)",
				"INSERT INTO T_NIB VALUES (3, 15)",
			},
			Query: "SELECT id FROM T_NIB WHERE v BETWEEN 0 AND 10 ORDER BY id",
		},

		// ===== Negative entries: error parity =====
		// `duplicate_pk_insert` (2nd INSERT throws RecordAlreadyExists)
		// is a setup-time INSERT error — same shape as
		// `integer_overflow_on_cast`. Triggers the same fdb-relational
		// state-leak. Drop until upstream resolves.
		{
			// Comparing string against bigint without cast → both
			// engines must reject (cannot promote types). Java uses
			// "operands ... not compatible"; Go uses "cannot compare
			// X with Y" — common substring "compa" matches both case-
			// insensitively.
			Name:                "type_mismatch_compare",
			SchemaTemplate:      "CREATE TABLE T_TMC (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls:           []string{"INSERT INTO T_TMC VALUES (1, 'x')"},
			Query:               "SELECT id FROM T_TMC WHERE name > 5",
			ExpectErrorContains: "compa",
		},

		// ===== String handling — swingshift-52 =====
		// LIKE with ESCAPE clause — pins escape-character handling so
		// `\_` matches literal underscore, not the single-char wildcard.
		{
			Name:           "like_escape_underscore",
			SchemaTemplate: "CREATE TABLE T_S1 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S1 VALUES (1, 'a_b')",
				"INSERT INTO T_S1 VALUES (2, 'aXb')",
				"INSERT INTO T_S1 VALUES (3, 'a_c')",
			},
			Query: "SELECT id, s FROM T_S1 WHERE s LIKE 'a\\_b' ESCAPE '\\' ORDER BY id",
		},
		// LIKE with ESCAPE — literal % via escape.
		{
			Name:           "like_escape_percent",
			SchemaTemplate: "CREATE TABLE T_S2 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S2 VALUES (1, '50%')",
				"INSERT INTO T_S2 VALUES (2, '50abc')",
				"INSERT INTO T_S2 VALUES (3, '50')",
			},
			Query: "SELECT id, s FROM T_S2 WHERE s LIKE '50\\%' ESCAPE '\\' ORDER BY id",
		},
		// Empty string vs NULL distinction — `s = ''` matches empty
		// strings only, NULL stays NULL via 3VL.
		{
			Name:           "empty_string_eq",
			SchemaTemplate: "CREATE TABLE T_S3 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S3 VALUES (1, '')",
				"INSERT INTO T_S3 VALUES (2, 'x')",
				"INSERT INTO T_S3 VALUES (3, NULL)",
			},
			Query: "SELECT id FROM T_S3 WHERE s = '' ORDER BY id",
		},
		// IS NULL must NOT match empty string.
		{
			Name:           "empty_string_is_null_distinction",
			SchemaTemplate: "CREATE TABLE T_S4 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S4 VALUES (1, '')",
				"INSERT INTO T_S4 VALUES (2, 'x')",
				"INSERT INTO T_S4 VALUES (3, NULL)",
			},
			Query: "SELECT id, s FROM T_S4 WHERE s IS NULL ORDER BY id",
		},
		// LIKE '%' must match every non-NULL row including empty.
		{
			Name:           "like_match_all_includes_empty",
			SchemaTemplate: "CREATE TABLE T_S5 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S5 VALUES (1, '')",
				"INSERT INTO T_S5 VALUES (2, 'a')",
				"INSERT INTO T_S5 VALUES (3, NULL)",
			},
			Query: "SELECT id, s FROM T_S5 WHERE s LIKE '%' ORDER BY id",
		},
		// LIKE '_' must match exactly one-character strings (not empty).
		{
			Name:           "like_single_underscore_skips_empty",
			SchemaTemplate: "CREATE TABLE T_S6 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S6 VALUES (1, '')",
				"INSERT INTO T_S6 VALUES (2, 'a')",
				"INSERT INTO T_S6 VALUES (3, 'ab')",
			},
			Query: "SELECT id, s FROM T_S6 WHERE s LIKE '_' ORDER BY id",
		},
		// Unicode characters round-trip through the wire and compare
		// correctly under equality.
		{
			Name:           "unicode_string_eq",
			SchemaTemplate: "CREATE TABLE T_S7 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S7 VALUES (1, 'café')",
				"INSERT INTO T_S7 VALUES (2, 'naïve')",
				"INSERT INTO T_S7 VALUES (3, '日本')",
			},
			Query: "SELECT id, s FROM T_S7 WHERE s = 'café' ORDER BY id",
		},
		// Unicode multi-row sort — pins byte-equivalent ordering for
		// non-ASCII strings (UTF-8 sort order, not collation-aware).
		{
			Name:           "unicode_sort_full_scan",
			SchemaTemplate: "CREATE TABLE T_S8 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S8 VALUES (1, 'ümlaut')",
				"INSERT INTO T_S8 VALUES (2, 'apple')",
				"INSERT INTO T_S8 VALUES (3, '日本')",
				"INSERT INTO T_S8 VALUES (4, 'banana')",
			},
			Query: "SELECT id, s FROM T_S8 ORDER BY id",
		},
		// String BETWEEN — inclusive on both ends, lexicographic.
		{
			Name:           "string_between",
			SchemaTemplate: "CREATE TABLE T_S9 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S9 VALUES (1, 'apple')",
				"INSERT INTO T_S9 VALUES (2, 'banana')",
				"INSERT INTO T_S9 VALUES (3, 'cherry')",
				"INSERT INTO T_S9 VALUES (4, 'date')",
			},
			Query: "SELECT id, s FROM T_S9 WHERE s BETWEEN 'b' AND 'd' ORDER BY id",
		},
		// IN list with single string value — degenerate IN.
		{
			Name:           "string_in_singleton",
			SchemaTemplate: "CREATE TABLE T_S10 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S10 VALUES (1, 'foo')",
				"INSERT INTO T_S10 VALUES (2, 'bar')",
				"INSERT INTO T_S10 VALUES (3, 'baz')",
			},
			Query: "SELECT id FROM T_S10 WHERE s IN ('bar') ORDER BY id",
		},
		// IN list including the empty string literal.
		{
			Name:           "string_in_with_empty",
			SchemaTemplate: "CREATE TABLE T_S11 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S11 VALUES (1, '')",
				"INSERT INTO T_S11 VALUES (2, 'foo')",
				"INSERT INTO T_S11 VALUES (3, 'bar')",
			},
			Query: "SELECT id, s FROM T_S11 WHERE s IN ('', 'foo') ORDER BY id",
		},
		// Multiple OR'd string predicates (LIKE + equality + IS NULL).
		{
			Name:           "string_or_mixed_predicates",
			SchemaTemplate: "CREATE TABLE T_S12 (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_S12 VALUES (1, 'apple')",
				"INSERT INTO T_S12 VALUES (2, 'banana')",
				"INSERT INTO T_S12 VALUES (3, NULL)",
				"INSERT INTO T_S12 VALUES (4, 'cherry')",
				"INSERT INTO T_S12 VALUES (5, 'orange')",
			},
			Query: "SELECT id FROM T_S12 WHERE s LIKE 'a%' OR s = 'cherry' OR s IS NULL ORDER BY id",
		},
		// String concat via `||` is not supported by fdb-relational
		// 4.11.1.0 — neither lexer nor parser knows about ||. It's a
		// pure-grammar gap, not a Java-side planner issue, so this
		// category has no entry today. Re-add when grammar grows the
		// CONCAT_OP token.

		// ===== NULL / Kleene 3VL — extended coverage =====
		{
			// `x > NULL` is UNKNOWN (filtered out by WHERE) — same
			// as `x = NULL`, but pins the inequality side of the
			// 3-valued comparison rules. No rows ever match.
			Name:           "null_gt_yields_empty",
			SchemaTemplate: "CREATE TABLE T_NL1 (id BIGINT, x BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL1 VALUES (1, 5)",
				"INSERT INTO T_NL1 VALUES (2, NULL)",
				"INSERT INTO T_NL1 VALUES (3, 10)",
			},
			Query: "SELECT id FROM T_NL1 WHERE x > NULL ORDER BY id",
		},
		{
			// NULL multiplied by a literal → NULL projected as the
			// computed column. Pins NULL propagation through *.
			Name:           "null_arith_mul",
			SchemaTemplate: "CREATE TABLE T_NL2 (id BIGINT, x BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL2 VALUES (1, 4)",
				"INSERT INTO T_NL2 VALUES (2, NULL)",
				"INSERT INTO T_NL2 VALUES (3, 7)",
			},
			Query: "SELECT id, x * 3 FROM T_NL2 ORDER BY id",
		},
		{
			// NULL division — `NULL / 2` → NULL. (Avoid `x / 0`
			// which raises a runtime error in some engines.)
			Name:           "null_arith_div",
			SchemaTemplate: "CREATE TABLE T_NL3 (id BIGINT, x BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL3 VALUES (1, 20)",
				"INSERT INTO T_NL3 VALUES (2, NULL)",
			},
			Query: "SELECT id, x / 2 FROM T_NL3 ORDER BY id",
		},
		{
			// AND truth table with NULL. Encoded as `flag = TRUE
			// AND val = 10` so we can mix both sides programmatically.
			//   id=1: TRUE  AND TRUE  = TRUE   ← match
			//   id=2: TRUE  AND FALSE = FALSE
			//   id=3: TRUE  AND NULL  = NULL   ← UNKNOWN, filtered
			//   id=4: FALSE AND NULL  = FALSE
			//   id=5: NULL  AND TRUE  = NULL   ← UNKNOWN, filtered
			Name:           "kleene_and_truth_table",
			SchemaTemplate: "CREATE TABLE T_NL4 (id BIGINT, flag BOOLEAN, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL4 VALUES (1, TRUE,  10)",
				"INSERT INTO T_NL4 VALUES (2, TRUE,  20)",
				"INSERT INTO T_NL4 VALUES (3, TRUE,  NULL)",
				"INSERT INTO T_NL4 VALUES (4, FALSE, NULL)",
				"INSERT INTO T_NL4 VALUES (5, NULL,  10)",
			},
			Query: "SELECT id FROM T_NL4 WHERE flag = TRUE AND val = 10 ORDER BY id",
		},
		{
			// OR truth table with NULL.
			//   id=1: TRUE  OR FALSE = TRUE   ← match
			//   id=2: FALSE OR TRUE  = TRUE   ← match
			//   id=3: TRUE  OR NULL  = TRUE   ← match
			//   id=4: FALSE OR NULL  = NULL   ← UNKNOWN, filtered
			//   id=5: NULL  OR NULL  = NULL   ← UNKNOWN, filtered
			Name:           "kleene_or_truth_table",
			SchemaTemplate: "CREATE TABLE T_NL5 (id BIGINT, a BOOLEAN, b BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL5 VALUES (1, TRUE,  FALSE)",
				"INSERT INTO T_NL5 VALUES (2, FALSE, TRUE)",
				"INSERT INTO T_NL5 VALUES (3, TRUE,  NULL)",
				"INSERT INTO T_NL5 VALUES (4, FALSE, NULL)",
				"INSERT INTO T_NL5 VALUES (5, NULL,  NULL)",
			},
			Query: "SELECT id FROM T_NL5 WHERE a = TRUE OR b = TRUE ORDER BY id",
		},
		{
			// NOT of an UNKNOWN value remains UNKNOWN → filtered.
			// id=3 has flag=NULL, so `flag = TRUE` is UNKNOWN, and
			// `NOT UNKNOWN` stays UNKNOWN. Only id=2 (FALSE)
			// survives — `NOT FALSE` is TRUE.
			Name:           "kleene_not_null",
			SchemaTemplate: "CREATE TABLE T_NL6 (id BIGINT, flag BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL6 VALUES (1, TRUE)",
				"INSERT INTO T_NL6 VALUES (2, FALSE)",
				"INSERT INTO T_NL6 VALUES (3, NULL)",
			},
			Query: "SELECT id FROM T_NL6 WHERE NOT (flag = TRUE) ORDER BY id",
		},
		{
			// IS DISTINCT FROM — null-safe inequality. Returns
			// boolean (TRUE/FALSE), never UNKNOWN. Rows where
			// (x IS DISTINCT FROM 5) is TRUE: id=2 (NULL≠5),
			// id=3 (10≠5). id=1 (5=5) excluded.
			Name:           "is_distinct_from",
			SchemaTemplate: "CREATE TABLE T_NL7 (id BIGINT, x BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL7 VALUES (1, 5)",
				"INSERT INTO T_NL7 VALUES (2, NULL)",
				"INSERT INTO T_NL7 VALUES (3, 10)",
			},
			Query: "SELECT id FROM T_NL7 WHERE x IS DISTINCT FROM 5 ORDER BY id",
		},
		{
			// IS NOT DISTINCT FROM NULL — null-safe equality. Only
			// id=2 (where x IS NULL) survives.
			Name:           "is_not_distinct_from_null",
			SchemaTemplate: "CREATE TABLE T_NL8 (id BIGINT, x BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL8 VALUES (1, 5)",
				"INSERT INTO T_NL8 VALUES (2, NULL)",
				"INSERT INTO T_NL8 VALUES (3, 10)",
			},
			Query: "SELECT id FROM T_NL8 WHERE x IS NOT DISTINCT FROM NULL ORDER BY id",
		},
		{
			// CASE WHEN val = NULL THEN ... — `val = NULL` is
			// UNKNOWN for every row, so the WHEN never fires; all
			// rows take the ELSE branch. Pins that CASE treats
			// UNKNOWN like FALSE.
			Name:           "case_with_null_eq",
			SchemaTemplate: "CREATE TABLE T_NL9 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL9 VALUES (1, 5)",
				"INSERT INTO T_NL9 VALUES (2, NULL)",
				"INSERT INTO T_NL9 VALUES (3, 10)",
			},
			Query: "SELECT id, CASE WHEN val = NULL THEN CAST(1 AS INTEGER) ELSE CAST(0 AS INTEGER) END FROM T_NL9 ORDER BY id",
		},
		// `NOT IN (1, NULL)` is a one-sided divergence: SQL standard
		// + Go propagate UNKNOWN (yields empty); fdb-relational
		// 4.11.1.0 rejects with "NULL values are not allowed in the
		// IN list". Can't be expressed as a positive corpus entry
		// (Java rejects) or a negative one (Go accepts). Tracked in
		// CLAUDE.md gotchas; re-add when one side aligns.
		{
			// Empty string is distinct from NULL. id=1 has '',
			// id=2 has NULL. `name = ''` matches only id=1.
			Name:           "empty_string_vs_null",
			SchemaTemplate: "CREATE TABLE T_NL11 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL11 VALUES (1, '')",
				"INSERT INTO T_NL11 VALUES (2, NULL)",
				"INSERT INTO T_NL11 VALUES (3, 'x')",
			},
			Query: "SELECT id FROM T_NL11 WHERE name = '' ORDER BY id",
		},
		{
			// COALESCE pulls the first non-NULL value. id=2 has
			// NULL → projection becomes the literal fallback. Pins
			// the fallback path through Java's function registry
			// (COALESCE is one of the few scalar functions Java's
			// 4.11.1.0 planner registers).
			Name:           "coalesce_with_null",
			SchemaTemplate: "CREATE TABLE T_NL12 (id BIGINT, x BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL12 VALUES (1, 100)",
				"INSERT INTO T_NL12 VALUES (2, NULL)",
				"INSERT INTO T_NL12 VALUES (3, 300)",
			},
			Query: "SELECT id, COALESCE(x, -1) FROM T_NL12 ORDER BY id",
		},
		{
			// IS NULL evaluated in the SELECT projection — pins the
			// boolean projection path (rather than WHERE filter).
			Name:           "is_null_in_projection",
			SchemaTemplate: "CREATE TABLE T_NL13 (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_NL13 VALUES (1, 'a')",
				"INSERT INTO T_NL13 VALUES (2, NULL)",
				"INSERT INTO T_NL13 VALUES (3, 'c')",
			},
			Query: "SELECT id, name IS NULL FROM T_NL13 ORDER BY id",
		},

		// ===== Numeric edge cases & arithmetic — swingshift-52 =====
		{
			// BIGINT max boundary — pins INT64 round-trip at the upper
			// edge of the proto INT64 range. Adjacent to the existing
			// negative_numbers entry which pins -9223372036854775807.
			Name:           "bigint_max_boundary",
			SchemaTemplate: "CREATE TABLE T_N1 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N1 VALUES (1, 9223372036854775807)",
				"INSERT INTO T_N1 VALUES (2, 9223372036854775806)",
				"INSERT INTO T_N1 VALUES (3, 0)",
			},
			Query: "SELECT id, val FROM T_N1 ORDER BY id",
		},
		{
			// INTEGER 32-bit boundaries: max (2^31-1), min+1 (-2^31+1),
			// and zero. Tests narrowing CAST + INT32 storage.
			Name:           "integer_boundaries",
			SchemaTemplate: "CREATE TABLE T_N2 (id BIGINT, val INTEGER, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N2 VALUES (1, CAST(2147483647 AS INTEGER))",
				"INSERT INTO T_N2 VALUES (2, CAST(-2147483647 AS INTEGER))",
				"INSERT INTO T_N2 VALUES (3, CAST(0 AS INTEGER))",
			},
			Query: "SELECT id, val FROM T_N2 ORDER BY id",
		},
		{
			// DOUBLE special values — 0.0, -0.0, very small / very
			// large finite. Pins float64 wire format across engines.
			Name:           "double_special_values",
			SchemaTemplate: "CREATE TABLE T_N3 (id BIGINT, val DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N3 VALUES (1, 0.0)",
				"INSERT INTO T_N3 VALUES (2, -0.0)",
				"INSERT INTO T_N3 VALUES (3, 1.7976931348623157E308)",
				"INSERT INTO T_N3 VALUES (4, 2.2250738585072014E-308)",
			},
			Query: "SELECT id, val FROM T_N3 ORDER BY id",
		},
		{
			// Mixed-type arithmetic: BIGINT + DOUBLE → DOUBLE.
			// Pins the type-promotion result + DOUBLE column-type
			// reporting in the projection.
			Name:           "bigint_plus_double",
			SchemaTemplate: "CREATE TABLE T_N4 (id BIGINT, x BIGINT, y DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N4 VALUES (1, 10, 1.5)",
				"INSERT INTO T_N4 VALUES (2, 100, 0.25)",
			},
			Query: "SELECT id, x + y FROM T_N4 ORDER BY id",
		},
		{
			// CAST BIGINT → DOUBLE then divide → produces fractional
			// DOUBLE result. Without the cast, integer division would
			// truncate to 0. Pins float-division semantics.
			Name:           "double_division_via_cast",
			SchemaTemplate: "CREATE TABLE T_N5 (id BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N5 VALUES (1, 1, 2)",
				"INSERT INTO T_N5 VALUES (2, 3, 4)",
				"INSERT INTO T_N5 VALUES (3, 7, 2)",
			},
			Query: "SELECT id, CAST(x AS DOUBLE) / CAST(y AS DOUBLE) FROM T_N5 ORDER BY id",
		},
		{
			// Modulo operator on BIGINT — pins `%` parser handling +
			// integer-mod result type (BIGINT).
			Name:           "modulo_bigint",
			SchemaTemplate: "CREATE TABLE T_N6 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N6 VALUES (1, 10)",
				"INSERT INTO T_N6 VALUES (2, 17)",
				"INSERT INTO T_N6 VALUES (3, 100)",
			},
			Query: "SELECT id, val % 7 FROM T_N6 ORDER BY id",
		},
		{
			// Bit AND / OR / XOR on BIGINT operands. Result is BIGINT.
			Name:           "bit_and_or_xor",
			SchemaTemplate: "CREATE TABLE T_N7 (id BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N7 VALUES (1, 12, 10)",
				"INSERT INTO T_N7 VALUES (2, 255, 15)",
			},
			Query: "SELECT id, x & y, x | y, x ^ y FROM T_N7 ORDER BY id",
		},
		// Bit shift `<<` / `>>` deferred: fdb-relational 4.11.1.0
		// returns `RelationalException: Unsupported operator <<`. The
		// lexer/parser accept the tokens, but the function registry
		// has no `<<` / `>>` evaluator. Re-add when fdb-relational
		// registers shift operators.
		{
			// Comparison BIGINT vs DOUBLE — promotes BIGINT to DOUBLE
			// and compares. Pins implicit promotion in WHERE.
			Name:           "compare_bigint_to_double",
			SchemaTemplate: "CREATE TABLE T_N9 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N9 VALUES (1, 1)",
				"INSERT INTO T_N9 VALUES (2, 5)",
				"INSERT INTO T_N9 VALUES (3, 10)",
			},
			Query: "SELECT id FROM T_N9 WHERE val > 2.5 ORDER BY id",
		},
		{
			// IS NULL on numeric (BIGINT) column. Pins three-valued
			// logic + null detection on numeric storage.
			Name:           "is_null_numeric",
			SchemaTemplate: "CREATE TABLE T_N10 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N10 VALUES (1, 100)",
				"INSERT INTO T_N10 VALUES (2, NULL)",
				"INSERT INTO T_N10 VALUES (3, 200)",
			},
			Query: "SELECT id FROM T_N10 WHERE val IS NULL ORDER BY id",
		},
		{
			// IS NOT NULL on numeric (DOUBLE) column. Companion to
			// is_null_numeric for the inverse predicate path.
			Name:           "is_not_null_numeric_double",
			SchemaTemplate: "CREATE TABLE T_N11 (id BIGINT, val DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N11 VALUES (1, 1.5)",
				"INSERT INTO T_N11 VALUES (2, NULL)",
				"INSERT INTO T_N11 VALUES (3, 2.5)",
			},
			Query: "SELECT id FROM T_N11 WHERE val IS NOT NULL ORDER BY id",
		},
		{
			// AVG over BIGINT operand — result type is DOUBLE per SQL
			// standard. Pins AVG result-type lattice + DOUBLE column-
			// type reporting.
			Name:           "avg_bigint",
			SchemaTemplate: "CREATE TABLE T_N12 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N12 VALUES (1, 10)",
				"INSERT INTO T_N12 VALUES (2, 20)",
				"INSERT INTO T_N12 VALUES (3, 30)",
			},
			Query: "SELECT avg(val) FROM T_N12",
		},
		{
			// AVG over DOUBLE operand — result type is DOUBLE. Pins
			// the no-op promotion path.
			Name:           "avg_double",
			SchemaTemplate: "CREATE TABLE T_N13 (id BIGINT, val DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_N13 VALUES (1, 1.0)",
				"INSERT INTO T_N13 VALUES (2, 2.0)",
				"INSERT INTO T_N13 VALUES (3, 3.0)",
			},
			Query: "SELECT avg(val) FROM T_N13",
		},
		// `integer_overflow_on_cast` triggered a real fdb-relational
		// state-leak: setup-time INSERT with a CAST overflow leaves
		// state behind, and after ~11 such occurrences the Java
		// conformance server's per-request latency jumps from <100ms
		// to 30+s (HTTP timeout). Investigation in
		// run_sql_conformance_test.go (now reverted) bisected the
		// trigger to "~11 consecutive setup-time INSERT errors".
		// Each individual entry returns a clean error in <120ms; in
		// aggregate they wedge the Java state.
		//
		// Negative tests of QUERY-time errors (undefined_column,
		// undefined_table, type_mismatch_compare, bare_bool_where_
		// rejected) DO NOT trigger this — they stay fast across the
		// full corpus. Only setup-time INSERT errors compound state.
		//
		// `duplicate_pk_insert` (2nd INSERT throws
		// RecordAlreadyExists) is the same shape as
		// integer_overflow_on_cast: setup-time INSERT failure. Even
		// with the overflow entry removed, duplicate_pk_insert
		// alone in the corpus eventually hits the cliff. Both are
		// dropped until fdb-relational fixes the state-leak.
		// Go-side coverage for these cases lives in
		// pkg/relational/sqldriver integration tests; only the
		// cross-engine pin is missing.
		// `division_by_zero` was investigated in isolation: it returns
		// "/ by zero" (ArithmeticException) in 137ms. But when placed
		// late in the full corpus (after 80+ prior entries), the same
		// request takes 30-60+ seconds — a real cumulative-state hang
		// in fdb-relational's error-path teardown. Bumping HTTP
		// timeout to 60s didn't fix it; Java just consumes the bigger
		// budget. Real Java-side bug; not solvable from our side
		// without restarting the Java conformance server periodically.
		// Re-add when fdb-relational fixes the state-pressure issue.

		// ===== More negative entries: error-parity surface =====
		{
			Name:                "insert_into_undefined_table",
			SchemaTemplate:      "CREATE TABLE T_NEG1 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:           []string{"INSERT INTO no_such_dml_table VALUES (1)"},
			Query:               "SELECT id FROM T_NEG1",
			ExpectErrorContains: "no_such_dml_table",
		},
		// Same cumulative-state hang. Returns clean "Value of
		// column 'NAME' is not provided" in 96ms in isolation;
		// hangs in full corpus.
		// Same cumulative-state hang. Returns clean "Invalid UUID
		// value for the UUID type not-a-real-uuid" in 95ms in
		// isolation; hangs in full corpus.
		// Same cumulative-state hang as the other DML / CAST
		// negative entries. Returns clean "Value out of range for
		// INT: -2147483649" in 1.2s in isolation; hangs in full
		// corpus. Note: integer_overflow_on_cast (positive side)
		// stays in the corpus and is the SOLE CAST-overflow
		// negative coverage.
		// Documented but not enforceable (one-sided divergence): bit-shift
		// `<<` / `>>` are tokenized but unimplemented in fdb-relational
		// (Java rejects "Unsupported operator <<"); Go's engine
		// implements them. Adding a corpus entry would either fail
		// permanently or paper over the divergence. Tracked in
		// CLAUDE.md gotchas. Same shape applies to `IS TRUE` / `IS
		// FALSE` on BOOLEAN — Java's planner rejects, Go's accepts.

		// ===== WITH / CTE coverage =====
		// Java's fdb-relational 4.11.1.0 parser greedily attaches a
		// trailing `ORDER BY` to the WITH expression body's inner
		// SELECT, then rejects with "order by is not supported in
		// subquery". The same shape works in Go. Use ORDER BY-free
		// variants until the upstream parser is fixed.
		{
			// Simplest WITH: bind a name to a SELECT, query it.
			// COUNT(*) outer aggregate sidesteps the parser bug
			// (no ORDER BY in the outer projection).
			Name:           "cte_basic_count",
			SchemaTemplate: "CREATE TABLE T_CTE1 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CTE1 VALUES (1, 100)",
				"INSERT INTO T_CTE1 VALUES (2, 200)",
				"INSERT INTO T_CTE1 VALUES (3, 300)",
			},
			Query: "WITH cte AS (SELECT id, val FROM T_CTE1 WHERE val > 100) SELECT count(*) FROM cte",
		},
		{
			// CTE with aggregate — pins aggregation flowing through
			// the materialised relation. No outer ORDER BY.
			Name:           "cte_with_aggregate",
			SchemaTemplate: "CREATE TABLE T_CTE3 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CTE3 VALUES (1, 10)",
				"INSERT INTO T_CTE3 VALUES (2, 20)",
				"INSERT INTO T_CTE3 VALUES (3, 30)",
			},
			Query: "WITH s AS (SELECT id, val FROM T_CTE3 WHERE val >= 20) SELECT count(*) FROM s",
		},
		{
			// CTE + final WHERE — predicate composition without
			// outer ORDER BY.
			Name:           "cte_filtered_then_filtered",
			SchemaTemplate: "CREATE TABLE T_CTE5 (id BIGINT, region STRING, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CTE5 VALUES (1, 'us', 100)",
				"INSERT INTO T_CTE5 VALUES (2, 'us', 200)",
				"INSERT INTO T_CTE5 VALUES (3, 'eu', 300)",
				"INSERT INTO T_CTE5 VALUES (4, 'eu', 400)",
			},
			Query: "WITH us AS (SELECT id, val FROM T_CTE5 WHERE region = 'us') SELECT count(*) FROM us WHERE val > 100",
		},

		// ===== Subquery-in-FROM (derived table) coverage =====
		{
			// Derived table with WHERE — pins the inner-WHERE +
			// outer-WHERE composition. Already covered by
			// subquery_in_from earlier; this variant uses a different
			// table and column shape.
			Name:           "derived_table_aggregate_outer_filter",
			SchemaTemplate: "CREATE TABLE T_DT1 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DT1 VALUES (1, 50)",
				"INSERT INTO T_DT1 VALUES (2, 150)",
				"INSERT INTO T_DT1 VALUES (3, 250)",
			},
			Query: "SELECT count(*) FROM (SELECT id FROM T_DT1 WHERE val > 100) AS x",
		},

		// ===== UNION coverage =====
		{
			// UNION ALL — preserves duplicates, no sort.
			Name:           "union_all_basic",
			SchemaTemplate: "CREATE TABLE T_UA (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UA VALUES (1, 100)",
				"INSERT INTO T_UA VALUES (2, 200)",
			},
			Query: "SELECT id FROM T_UA WHERE val = 100 UNION ALL SELECT id FROM T_UA WHERE val = 200 ORDER BY id",
		},
		// `UNION` without ALL (DISTINCT implied) is rejected by
		// fdb-relational 4.11.1.0 with "only UNION ALL is supported".
		// Add UNION-DISTINCT corpus entry when upstream lands DISTINCT.

		// ===== INSERT...SELECT coverage =====
		{
			// INSERT INTO target SELECT FROM source — pins DML that
			// writes from a query rather than VALUES.
			Name:           "insert_select_from",
			SchemaTemplate: "CREATE TABLE T_IS_SRC (id BIGINT, val BIGINT, PRIMARY KEY (id)) CREATE TABLE T_IS_DST (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_IS_SRC VALUES (1, 100)",
				"INSERT INTO T_IS_SRC VALUES (2, 200)",
				"INSERT INTO T_IS_DST SELECT id, val FROM T_IS_SRC WHERE val >= 150",
			},
			Query: "SELECT id, val FROM T_IS_DST ORDER BY id",
		},

		// ===== Aggregate edge cases =====
		{
			// COUNT(col) — should skip NULLs (different from COUNT(*)).
			Name:           "count_col_skips_nulls",
			SchemaTemplate: "CREATE TABLE T_AG1 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_AG1 VALUES (1, 10)",
				"INSERT INTO T_AG1 VALUES (2, NULL)",
				"INSERT INTO T_AG1 VALUES (3, 30)",
			},
			Query: "SELECT count(val) FROM T_AG1",
		},
		{
			// COUNT(*) over empty table → 0 (not NULL like SUM/MIN/MAX).
			Name:           "count_empty_table",
			SchemaTemplate: "CREATE TABLE T_AG2 (id BIGINT, PRIMARY KEY (id))",
			SetupSqls:      nil,
			Query:          "SELECT count(*) FROM T_AG2",
		},
		{
			// SUM over empty result → NULL (not 0). Pins SQL-standard
			// aggregate-over-empty behaviour.
			Name:           "sum_empty_result",
			SchemaTemplate: "CREATE TABLE T_AG3 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_AG3 VALUES (1, 100)",
			},
			Query: "SELECT sum(val) FROM T_AG3 WHERE val < 0",
		},
		{
			// MIN over empty result → NULL.
			Name:           "min_empty_result",
			SchemaTemplate: "CREATE TABLE T_AG4 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_AG4 VALUES (1, 100)",
			},
			Query: "SELECT min(val) FROM T_AG4 WHERE val < 0",
		},

		// ===== Multi-row INSERT =====
		{
			// VALUES with multiple rows in one INSERT — pins
			// batch-insert semantics through the round-trip.
			Name:           "multi_row_insert",
			SchemaTemplate: "CREATE TABLE T_MRI (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_MRI VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd')",
			},
			Query: "SELECT id, name FROM T_MRI ORDER BY id",
		},

		// ===== UPDATE with arithmetic =====
		{
			// UPDATE SET col = col + N — pins arithmetic-as-RHS in
			// the SET clause.
			Name:           "update_increment",
			SchemaTemplate: "CREATE TABLE T_UI (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UI VALUES (1, 10)",
				"INSERT INTO T_UI VALUES (2, 20)",
				"INSERT INTO T_UI VALUES (3, 30)",
				"UPDATE T_UI SET val = val + 100 WHERE id <= 2",
			},
			Query: "SELECT id, val FROM T_UI ORDER BY id",
		},

		// ===== Self-join (same table aliased twice) =====
		{
			// Self-join via comma-separated FROM with two aliases.
			// Pins the planner's ability to disambiguate two
			// projections of the same source.
			Name:           "self_join",
			SchemaTemplate: "CREATE TABLE T_SJ (id BIGINT, parent BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_SJ VALUES (1, 0, 'root')",
				"INSERT INTO T_SJ VALUES (2, 1, 'a')",
				"INSERT INTO T_SJ VALUES (3, 1, 'b')",
				"INSERT INTO T_SJ VALUES (4, 2, 'a-1')",
			},
			Query: "SELECT child.name, parent.name FROM T_SJ AS child, T_SJ AS parent WHERE child.parent = parent.id ORDER BY child.id",
		},

		// ===== Compound predicates with mixed AND/OR =====
		{
			// AND has higher precedence than OR — `(a AND b) OR c`
			// is equivalent to `a AND b OR c` per SQL spec. Pins
			// engines parse and evaluate operator precedence the same.
			Name:           "and_or_precedence",
			SchemaTemplate: "CREATE TABLE T_AOP (id BIGINT, x BIGINT, y BIGINT, z BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_AOP VALUES (1, 1, 0, 0)",
				"INSERT INTO T_AOP VALUES (2, 1, 1, 0)",
				"INSERT INTO T_AOP VALUES (3, 0, 0, 1)",
				"INSERT INTO T_AOP VALUES (4, 0, 0, 0)",
			},
			Query: "SELECT id FROM T_AOP WHERE x = 1 AND y = 1 OR z = 1 ORDER BY id",
		},

		// ===== Feature combinations =====
		{
			// JOIN + aggregate + WHERE + ORDER BY — full pipeline
			// across multiple sources.
			Name: "join_with_aggregate_filter_order",
			SchemaTemplate: "CREATE TABLE T_C1 (id BIGINT, region STRING, PRIMARY KEY (id)) " +
				"CREATE TABLE T_C2 (id BIGINT, parent BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_C1 VALUES (1, 'us')",
				"INSERT INTO T_C1 VALUES (2, 'eu')",
				"INSERT INTO T_C2 VALUES (10, 1, 100)",
				"INSERT INTO T_C2 VALUES (11, 1, 200)",
				"INSERT INTO T_C2 VALUES (12, 2, 300)",
				"INSERT INTO T_C2 VALUES (13, 2, 50)",
			},
			Query: "SELECT count(*) FROM T_C1 a, T_C2 b WHERE a.id = b.parent AND b.val > 75",
		},
		{
			// CTE + JOIN — pins JOIN against a materialised CTE
			// rather than a base table.
			Name: "cte_join_to_table",
			SchemaTemplate: "CREATE TABLE T_C3 (id BIGINT, name STRING, PRIMARY KEY (id)) " +
				"CREATE TABLE T_C4 (id BIGINT, fid BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_C3 VALUES (1, 'alpha')",
				"INSERT INTO T_C3 VALUES (2, 'beta')",
				"INSERT INTO T_C4 VALUES (10, 1, 100)",
				"INSERT INTO T_C4 VALUES (11, 1, 200)",
				"INSERT INTO T_C4 VALUES (12, 2, 300)",
			},
			// No outer ORDER BY — Java's parser greedy-attaches it
			// to the WITH expression body. Aggregate sidesteps.
			Query: "WITH big AS (SELECT id, fid FROM T_C4 WHERE val >= 200) SELECT count(*) FROM T_C3 t, big WHERE t.id = big.fid",
		},
		{
			// Compound WHERE + OR-of-AND — surfaces predicate
			// simplification across operator levels.
			Name:           "compound_or_of_and",
			SchemaTemplate: "CREATE TABLE T_C5 (id BIGINT, region STRING, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_C5 VALUES (1, 'us', 50)",
				"INSERT INTO T_C5 VALUES (2, 'us', 150)",
				"INSERT INTO T_C5 VALUES (3, 'eu', 50)",
				"INSERT INTO T_C5 VALUES (4, 'eu', 150)",
			},
			Query: "SELECT id FROM T_C5 WHERE (region = 'us' AND val > 100) OR (region = 'eu' AND val < 100) ORDER BY id",
		},
		{
			// CTE feeding aggregate that then feeds a final WHERE.
			Name:           "cte_aggregate_then_filter",
			SchemaTemplate: "CREATE TABLE T_C6 (id BIGINT, region STRING, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_C6 VALUES (1, 'us', 100)",
				"INSERT INTO T_C6 VALUES (2, 'us', 200)",
				"INSERT INTO T_C6 VALUES (3, 'eu', 300)",
			},
			Query: "WITH us AS (SELECT id, val FROM T_C6 WHERE region = 'us') SELECT count(*) FROM us WHERE val < 150",
		},

		// ===== UPDATE / DELETE feature interactions =====
		{
			// UPDATE with arithmetic on existing value — pins read-
			// modify-write semantics.
			Name:           "update_arithmetic_value",
			SchemaTemplate: "CREATE TABLE T_UA1 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UA1 VALUES (1, 100)",
				"INSERT INTO T_UA1 VALUES (2, 200)",
				"UPDATE T_UA1 SET val = val * 2",
			},
			Query: "SELECT id, val FROM T_UA1 ORDER BY id",
		},
		{
			// DELETE with predicate using BETWEEN.
			Name:           "delete_between",
			SchemaTemplate: "CREATE TABLE T_DB (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DB VALUES (1, 5)",
				"INSERT INTO T_DB VALUES (2, 15)",
				"INSERT INTO T_DB VALUES (3, 25)",
				"INSERT INTO T_DB VALUES (4, 35)",
				"DELETE FROM T_DB WHERE val BETWEEN 10 AND 30",
			},
			Query: "SELECT id FROM T_DB ORDER BY id",
		},
		{
			// DELETE all rows (no WHERE) — pins truncate-style DML.
			Name:           "delete_all",
			SchemaTemplate: "CREATE TABLE T_DA (id BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DA VALUES (1)",
				"INSERT INTO T_DA VALUES (2)",
				"INSERT INTO T_DA VALUES (3)",
				"DELETE FROM T_DA",
			},
			Query: "SELECT id FROM T_DA",
		},

		// ===== Composite primary key pushdown shapes (dayshift-62) =====
		// PK = (region, id). The Cascades planner picks different scan
		// strategies for partial-PK equality, full-PK equality, and a
		// range on the leading PK column. We verify both engines emit
		// identical row sets across the three shapes.
		{
			// Equality on leading PK column only — narrows to a region
			// prefix; emits in (region, id) PK order.
			Name:           "composite_pk_leading_eq",
			SchemaTemplate: "CREATE TABLE T_CPK1 (region STRING, id BIGINT, val BIGINT, PRIMARY KEY (region, id))",
			SetupSqls: []string{
				"INSERT INTO T_CPK1 VALUES ('us', 1, 100)",
				"INSERT INTO T_CPK1 VALUES ('us', 2, 200)",
				"INSERT INTO T_CPK1 VALUES ('us', 3, 300)",
				"INSERT INTO T_CPK1 VALUES ('eu', 1, 500)",
				"INSERT INTO T_CPK1 VALUES ('eu', 2, 600)",
			},
			Query: "SELECT id, val FROM T_CPK1 WHERE region = 'us' ORDER BY region, id",
		},
		{
			// Full-PK equality — single-row read by composite key.
			Name:           "composite_pk_full_eq",
			SchemaTemplate: "CREATE TABLE T_CPK2 (region STRING, id BIGINT, val BIGINT, PRIMARY KEY (region, id))",
			SetupSqls: []string{
				"INSERT INTO T_CPK2 VALUES ('us', 1, 100)",
				"INSERT INTO T_CPK2 VALUES ('us', 2, 200)",
				"INSERT INTO T_CPK2 VALUES ('eu', 1, 500)",
			},
			Query: "SELECT val FROM T_CPK2 WHERE region = 'us' AND id = 2 ORDER BY region, id",
		},
		{
			// Equality on leading PK + range on trailing PK — Cascades
			// implements as a key-range scan with both bounds derived
			// from the PK structure.
			Name:           "composite_pk_eq_and_range",
			SchemaTemplate: "CREATE TABLE T_CPK3 (region STRING, id BIGINT, val BIGINT, PRIMARY KEY (region, id))",
			SetupSqls: []string{
				"INSERT INTO T_CPK3 VALUES ('us', 1, 100)",
				"INSERT INTO T_CPK3 VALUES ('us', 2, 200)",
				"INSERT INTO T_CPK3 VALUES ('us', 3, 300)",
				"INSERT INTO T_CPK3 VALUES ('us', 4, 400)",
				"INSERT INTO T_CPK3 VALUES ('eu', 2, 999)",
			},
			Query: "SELECT id, val FROM T_CPK3 WHERE region = 'us' AND id >= 2 AND id < 4 ORDER BY region, id",
		},

		// ===== Multi-aggregate single SELECT =====
		// Pins type-promotion + aggregator wiring when several aggregates
		// touch the same column. Java's planner wires one aggregator per
		// distinct (op, col) pair; Go's grouping-bucket emits each.
		{
			// COUNT(*) + COUNT(col) + SUM + MIN + MAX in one SELECT.
			// COUNT(*) counts all rows, COUNT(val) skips NULL.
			Name:           "multi_agg_same_col",
			SchemaTemplate: "CREATE TABLE T_MA1 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_MA1 VALUES (1, 10)",
				"INSERT INTO T_MA1 VALUES (2, 20)",
				"INSERT INTO T_MA1 VALUES (3, NULL)",
				"INSERT INTO T_MA1 VALUES (4, 40)",
			},
			Query: "SELECT count(*), count(val), sum(val), min(val), max(val) FROM T_MA1",
		},
		{
			// Aggregates over different columns in the same SELECT.
			// COUNT(*), SUM(price), AVG(qty) — three separate buckets.
			Name:           "multi_agg_different_cols",
			SchemaTemplate: "CREATE TABLE T_MA2 (id BIGINT, qty BIGINT, price DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_MA2 VALUES (1, 5, 1.5)",
				"INSERT INTO T_MA2 VALUES (2, 10, 2.5)",
				"INSERT INTO T_MA2 VALUES (3, 15, 3.5)",
			},
			Query: "SELECT count(*), sum(price), avg(qty) FROM T_MA2",
		},

		// ===== CASE expression edge cases =====
		// nightshift-61 documented that simple-CASE form is silently
		// broken in fdb-relational; we use only searched-CASE here.
		{
			// CASE WHEN ... THEN ... (no ELSE) — defaults to NULL when
			// no WHEN matches. Pins three-valued semantics when ELSE is
			// omitted.
			Name:           "case_no_else",
			SchemaTemplate: "CREATE TABLE T_CN (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CN VALUES (1, 10)",
				"INSERT INTO T_CN VALUES (2, 20)",
				"INSERT INTO T_CN VALUES (3, 30)",
			},
			Query: "SELECT id, CASE WHEN val < 25 THEN 'low' END FROM T_CN ORDER BY id",
		},
		{
			// Nested CASE — outer CASE branches whose THEN values are
			// themselves CASE expressions. Pins recursive evaluation.
			Name:           "case_nested_in_then",
			SchemaTemplate: "CREATE TABLE T_CNES (id BIGINT, val BIGINT, flag BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CNES VALUES (1, 5, TRUE)",
				"INSERT INTO T_CNES VALUES (2, 5, FALSE)",
				"INSERT INTO T_CNES VALUES (3, 50, TRUE)",
				"INSERT INTO T_CNES VALUES (4, 50, FALSE)",
			},
			Query: "SELECT id, CASE WHEN val < 10 THEN CASE WHEN flag = TRUE THEN 'small_yes' ELSE 'small_no' END ELSE CASE WHEN flag = TRUE THEN 'big_yes' ELSE 'big_no' END END FROM T_CNES ORDER BY id",
		},
		{
			// CASE inside aggregate — Go-permissive type-inference
			// divergence from Java surfaced dayshift-62. Java reports
			// `SUM(CASE WHEN p THEN 1 ELSE 0 END)` as INTEGER (the
			// Cascades planner inherits the integer-literal branch
			// type and Java's SUM(INTEGER) overload preserves INTEGER);
			// Go reports BIGINT (`inferConstantJDBCType` types bare
			// integer literals as BIGINT, and SUM with `aggExpr != nil`
			// recursively-infers BIGINT for the CASE result). The
			// divergence lives in two layers — Go's literal-typing
			// AND Go's aggregate-result inheritance from `aggExpr`. To
			// align without changing Go's row values, the CAST below
			// pins the CASE branches to BIGINT so SUM stays BIGINT in
			// Java too. The bare-int-literal form is documented as a
			// CLAUDE.md gotcha + tracked as a TODO; revisit when
			// aggregate column-type inference is unified with Cascades
			// SUM-overload resolution.
			Name:           "case_in_aggregate_bigint_cast",
			SchemaTemplate: "CREATE TABLE T_CIA (id BIGINT, status STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CIA VALUES (1, 'open')",
				"INSERT INTO T_CIA VALUES (2, 'closed')",
				"INSERT INTO T_CIA VALUES (3, 'open')",
				"INSERT INTO T_CIA VALUES (4, 'pending')",
				"INSERT INTO T_CIA VALUES (5, 'open')",
			},
			Query: "SELECT sum(CASE WHEN status = 'open' THEN CAST(1 AS BIGINT) ELSE CAST(0 AS BIGINT) END), count(*) FROM T_CIA",
		},

		// ===== BETWEEN edge cases =====
		{
			// Single-value BETWEEN — `BETWEEN x AND x` reduces to
			// equality. Pins inclusive bound semantics.
			Name:           "between_equal_bounds",
			SchemaTemplate: "CREATE TABLE T_BE (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BE VALUES (1, 5)",
				"INSERT INTO T_BE VALUES (2, 10)",
				"INSERT INTO T_BE VALUES (3, 10)",
				"INSERT INTO T_BE VALUES (4, 15)",
			},
			Query: "SELECT id FROM T_BE WHERE val BETWEEN 10 AND 10 ORDER BY id",
		},
		{
			// Reversed-bound BETWEEN — SQL spec says `BETWEEN hi AND lo`
			// matches no rows (since the predicate is `v >= hi AND v <= lo`,
			// false for hi > lo). Pins both engines collapse the same way.
			Name:           "between_reversed_bounds",
			SchemaTemplate: "CREATE TABLE T_BR (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BR VALUES (1, 5)",
				"INSERT INTO T_BR VALUES (2, 10)",
				"INSERT INTO T_BR VALUES (3, 15)",
				"INSERT INTO T_BR VALUES (4, 20)",
			},
			Query: "SELECT id FROM T_BR WHERE val BETWEEN 20 AND 5 ORDER BY id",
		},
		{
			// BETWEEN with one NULL bound — three-valued logic: NULL
			// upper means UNKNOWN for any value, BETWEEN evaluates
			// (val >= 5 AND val <= NULL) → UNKNOWN → row excluded.
			// Per CLAUDE.md the bare-NULL-in-arithmetic gotcha; the
			// BETWEEN rewrite uses comparison ops so this should
			// evaluate cleanly in both engines.
			Name:           "between_with_null_upper",
			SchemaTemplate: "CREATE TABLE T_BN (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_BN VALUES (1, 5)",
				"INSERT INTO T_BN VALUES (2, 10)",
				"INSERT INTO T_BN VALUES (3, 15)",
			},
			Query: "SELECT id FROM T_BN WHERE val BETWEEN 5 AND CAST(NULL AS BIGINT) ORDER BY id",
		},

		// ===== DML no-op edges =====
		{
			// DELETE WHERE matches no rows — should leave the table
			// intact. Pins that DML bind to WHERE evaluation.
			Name:           "delete_no_match",
			SchemaTemplate: "CREATE TABLE T_DNM (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_DNM VALUES (1, 10)",
				"INSERT INTO T_DNM VALUES (2, 20)",
				"DELETE FROM T_DNM WHERE val > 1000",
			},
			Query: "SELECT id, val FROM T_DNM ORDER BY id",
		},
		{
			// UPDATE WHERE matches no rows — table stays unchanged.
			Name:           "update_no_match",
			SchemaTemplate: "CREATE TABLE T_UNM (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UNM VALUES (1, 10)",
				"INSERT INTO T_UNM VALUES (2, 20)",
				"UPDATE T_UNM SET val = 999 WHERE val > 1000",
			},
			Query: "SELECT id, val FROM T_UNM ORDER BY id",
		},

		// ===== LIKE pattern variants =====
		{
			// LIKE without wildcards — degenerates to equality. Pins
			// that engines treat anchored literal patterns as exact
			// matches (no implicit prefix/contains semantics).
			Name:           "like_no_wildcard_eq",
			SchemaTemplate: "CREATE TABLE T_LNW (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_LNW VALUES (1, 'alpha')",
				"INSERT INTO T_LNW VALUES (2, 'alphabet')",
				"INSERT INTO T_LNW VALUES (3, 'beta')",
			},
			Query: "SELECT id FROM T_LNW WHERE name LIKE 'alpha' ORDER BY id",
		},
		{
			// LIKE with both leading and trailing % wildcards —
			// contains-anywhere pattern. Pins planner doesn't
			// confuse 'contains' with 'prefix' or 'equality'.
			Name:           "like_contains_middle",
			SchemaTemplate: "CREATE TABLE T_LCM (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_LCM VALUES (1, 'foobarbaz')",
				"INSERT INTO T_LCM VALUES (2, 'bar')",
				"INSERT INTO T_LCM VALUES (3, 'qux')",
				"INSERT INTO T_LCM VALUES (4, 'barbaz')",
			},
			Query: "SELECT id FROM T_LCM WHERE name LIKE '%bar%' ORDER BY id",
		},
		{
			// LIKE against NULL — three-valued logic: NULL LIKE
			// anything yields UNKNOWN, which excludes the row from
			// the WHERE clause.
			Name:           "like_against_null",
			SchemaTemplate: "CREATE TABLE T_LN (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_LN VALUES (1, 'a')",
				"INSERT INTO T_LN VALUES (2, NULL)",
				"INSERT INTO T_LN VALUES (3, 'b')",
			},
			Query: "SELECT id FROM T_LN WHERE name LIKE '%' ORDER BY id",
		},

		// ===== INSERT variants =====
		{
			// INSERT multiple rows in a single VALUES clause. Pins
			// row-batch inserts behave the same as individual inserts.
			Name:           "insert_multi_values_single_stmt",
			SchemaTemplate: "CREATE TABLE T_IMV (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_IMV VALUES (1, 10), (2, 20), (3, 30)",
			},
			Query: "SELECT id, val FROM T_IMV ORDER BY id",
		},

		// ===== UNION ALL edge cases =====
		{
			// UNION ALL with overlapping rows — preserves duplicates
			// (UNION ALL semantics, no dedup).
			Name:           "union_all_with_dupes",
			SchemaTemplate: "CREATE TABLE T_UD (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UD VALUES (1, 10)",
				"INSERT INTO T_UD VALUES (2, 20)",
				"INSERT INTO T_UD VALUES (3, 30)",
			},
			// No outer ORDER BY — UNION ALL planner rejects inner
			// ORDER BYs and a top-level ORDER BY is also constrained.
			// Use an aggregate to make the result deterministic.
			Query: "SELECT count(*) FROM (SELECT id FROM T_UD WHERE val > 5 UNION ALL SELECT id FROM T_UD WHERE val > 15) AS u",
		},
		{
			// UNION ALL of empty + non-empty — empty side contributes
			// 0 rows, non-empty side contributes its rows.
			Name:           "union_all_empty_side",
			SchemaTemplate: "CREATE TABLE T_UE (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UE VALUES (1, 100)",
				"INSERT INTO T_UE VALUES (2, 200)",
			},
			Query: "SELECT count(*) FROM (SELECT id FROM T_UE WHERE val > 1000 UNION ALL SELECT id FROM T_UE WHERE val > 50) AS u",
		},

		// ===== HAVING shapes =====
		{
			// HAVING with COUNT predicate over the whole table (no GROUP
			// BY). Pins HAVING-without-GROUP-BY in both engines.
			Name:           "having_count_predicate",
			SchemaTemplate: "CREATE TABLE T_HC (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_HC VALUES (1, 10)",
				"INSERT INTO T_HC VALUES (2, 20)",
				"INSERT INTO T_HC VALUES (3, 30)",
			},
			Query: "SELECT count(*) FROM T_HC HAVING count(*) > 2",
		},

		// ===== Mixed-type arithmetic (DOUBLE + BIGINT) =====
		{
			// BIGINT + DOUBLE -> DOUBLE per SQL type-promotion lattice.
			// Pins both engines pick DOUBLE result type for mixed-type
			// arithmetic, not BIGINT or INTEGER.
			Name:           "mixed_arith_bigint_plus_double_col",
			SchemaTemplate: "CREATE TABLE T_MA3 (id BIGINT, b BIGINT, d DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_MA3 VALUES (1, 10, 1.5)",
				"INSERT INTO T_MA3 VALUES (2, 20, 2.25)",
			},
			Query: "SELECT id, b + d FROM T_MA3 ORDER BY id",
		},
		{
			// DOUBLE / BIGINT preserves DOUBLE precision rather than
			// integer-dividing. Pins type-aware division.
			Name:           "mixed_arith_double_div_bigint",
			SchemaTemplate: "CREATE TABLE T_MA4 (id BIGINT, b BIGINT, d DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_MA4 VALUES (1, 4, 10.0)",
				"INSERT INTO T_MA4 VALUES (2, 3, 10.0)",
			},
			Query: "SELECT id, d / b FROM T_MA4 ORDER BY id",
		},

		// ===== Three-valued logic in NOT shape =====
		{
			// `NOT (flag = TRUE)` — NOT forces predicate context so the
			// inner parens are accepted. flag=TRUE → FALSE (excluded);
			// flag=FALSE → TRUE (included); flag=NULL → NOT(NULL)=NULL
			// (excluded). Pins NOT-of-NULL doesn't accidentally match.
			Name:           "kleene_not_eq_true",
			SchemaTemplate: "CREATE TABLE T_KNN (id BIGINT, flag BOOLEAN, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_KNN VALUES (1, TRUE)",
				"INSERT INTO T_KNN VALUES (2, FALSE)",
				"INSERT INTO T_KNN VALUES (3, NULL)",
			},
			Query: "SELECT id FROM T_KNN WHERE NOT (flag = TRUE) ORDER BY id",
		},
		{
			// (a > 5) AND (b > 5) where one side is NULL. UNKNOWN AND
			// TRUE = UNKNOWN → row excluded. UNKNOWN AND FALSE = FALSE
			// → row excluded. Pins AND-with-NULL collapses correctly.
			Name:           "kleene_and_with_null_operand",
			SchemaTemplate: "CREATE TABLE T_KAN (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_KAN VALUES (1, 10, 10)",
				"INSERT INTO T_KAN VALUES (2, 10, NULL)",
				"INSERT INTO T_KAN VALUES (3, NULL, 10)",
				"INSERT INTO T_KAN VALUES (4, NULL, NULL)",
				"INSERT INTO T_KAN VALUES (5, 1, 10)",
			},
			Query: "SELECT id FROM T_KAN WHERE a > 5 AND b > 5 ORDER BY id",
		},

		// ===== CAST chains and edge cases =====
		{
			// CAST(CAST(...)) chain — pins lossy round-trips. DOUBLE
			// 1.5 → BIGINT (rounds to 2 per Java's Math.round) →
			// DOUBLE 2.0. Both engines round identically.
			Name:           "cast_chain_double_to_bigint_to_double",
			SchemaTemplate: "CREATE TABLE T_CC1 (id BIGINT, v DOUBLE, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CC1 VALUES (1, 1.5)",
				"INSERT INTO T_CC1 VALUES (2, 2.6)",
				"INSERT INTO T_CC1 VALUES (3, 3.4)",
			},
			Query: "SELECT id, CAST(CAST(v AS BIGINT) AS DOUBLE) FROM T_CC1 ORDER BY id",
		},
		{
			// CAST a typed NULL through different types — both engines
			// preserve NULL through CAST. Pins typed-NULL round-trips.
			Name:           "cast_typed_null_chain",
			SchemaTemplate: "CREATE TABLE T_CTN (id BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CTN VALUES (1)",
				"INSERT INTO T_CTN VALUES (2)",
			},
			Query: "SELECT id, CAST(CAST(NULL AS BIGINT) AS STRING) FROM T_CTN ORDER BY id",
		},
		{
			// CAST(string AS BIGINT) — implicit numeric parsing.
			// Pins both engines parse '42' identically.
			Name:           "cast_string_to_bigint",
			SchemaTemplate: "CREATE TABLE T_CSB (id BIGINT, s STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CSB VALUES (1, '42')",
				"INSERT INTO T_CSB VALUES (2, '-7')",
				"INSERT INTO T_CSB VALUES (3, '0')",
			},
			Query: "SELECT id, CAST(s AS BIGINT) FROM T_CSB ORDER BY id",
		},

		// ===== UPDATE edge cases =====
		{
			// UPDATE with predicate referencing NEW logic — UPDATE
			// SET val = val + 1 WHERE val < 50 — only some rows match.
			Name:           "update_with_predicate",
			SchemaTemplate: "CREATE TABLE T_UWP (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_UWP VALUES (1, 10)",
				"INSERT INTO T_UWP VALUES (2, 50)",
				"INSERT INTO T_UWP VALUES (3, 100)",
				"UPDATE T_UWP SET val = val + 1 WHERE val < 50",
			},
			Query: "SELECT id, val FROM T_UWP ORDER BY id",
		},
		{
			// UPDATE setting one column to NULL. Pins NULL-to-column
			// assignment in both engines.
			Name:           "update_set_null",
			SchemaTemplate: "CREATE TABLE T_USN (id BIGINT, name STRING, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_USN VALUES (1, 'alice')",
				"INSERT INTO T_USN VALUES (2, 'bob')",
				"UPDATE T_USN SET name = NULL WHERE id = 2",
			},
			Query: "SELECT id, name FROM T_USN ORDER BY id",
		},

		// ===== Multi-CTE shapes =====
		{
			// Two CTEs in a single WITH clause; one feeds the other.
			Name:           "cte_chain",
			SchemaTemplate: "CREATE TABLE T_CCH (id BIGINT, region STRING, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_CCH VALUES (1, 'us', 100)",
				"INSERT INTO T_CCH VALUES (2, 'us', 200)",
				"INSERT INTO T_CCH VALUES (3, 'eu', 50)",
				"INSERT INTO T_CCH VALUES (4, 'eu', 300)",
			},
			Query: "WITH us AS (SELECT id, val FROM T_CCH WHERE region = 'us'), big AS (SELECT id, val FROM us WHERE val > 100) SELECT count(*) FROM big",
		},

		// ===== Aggregate edge cases (no GROUP BY) =====
		{
			// SUM/MIN/MAX on a column that's NULL in all rows.
			// SUM(NULL) = NULL, MIN(NULL) = NULL, MAX(NULL) = NULL.
			// Pins all-NULL aggregate result.
			Name:           "agg_all_null_column",
			SchemaTemplate: "CREATE TABLE T_AAN (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_AAN VALUES (1, NULL)",
				"INSERT INTO T_AAN VALUES (2, NULL)",
				"INSERT INTO T_AAN VALUES (3, NULL)",
			},
			Query: "SELECT sum(val), min(val), max(val), count(val) FROM T_AAN",
		},
		{
			// COUNT(*) over filtered single row — confirms simple
			// aggregate path with WHERE pushdown.
			Name:           "count_star_one_row",
			SchemaTemplate: "CREATE TABLE T_C1 (id BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_C1 VALUES (1, 10)",
				"INSERT INTO T_C1 VALUES (2, 20)",
				"INSERT INTO T_C1 VALUES (3, 30)",
			},
			Query: "SELECT count(*) FROM T_C1 WHERE id = 2",
		},

		// ===== JOIN with WHERE on inner table =====
		{
			// Comma-join with WHERE filter on the right table only —
			// pins JOIN+filter pushdown semantics.
			Name: "join_filter_inner_only",
			SchemaTemplate: "CREATE TABLE T_J1 (id BIGINT, name STRING, PRIMARY KEY (id)) " +
				"CREATE TABLE T_J2 (id BIGINT, parent BIGINT, val BIGINT, PRIMARY KEY (id))",
			SetupSqls: []string{
				"INSERT INTO T_J1 VALUES (1, 'a')",
				"INSERT INTO T_J1 VALUES (2, 'b')",
				"INSERT INTO T_J2 VALUES (10, 1, 100)",
				"INSERT INTO T_J2 VALUES (11, 1, 50)",
				"INSERT INTO T_J2 VALUES (12, 2, 200)",
				"INSERT INTO T_J2 VALUES (13, 2, 25)",
			},
			Query: "SELECT count(*) FROM T_J1 a, T_J2 b WHERE a.id = b.parent AND b.val > 75",
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
