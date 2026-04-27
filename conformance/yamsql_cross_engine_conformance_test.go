package conformance_test

// Track A3 — yamsql semantic equivalence on the cross-engine plandiff
// harness. dayshift-55: drives hand-picked yamsql scenarios through BOTH
// the Java fdb-relational executor and the Go embedded engine (via
// plandiff's runners), asserting that
//
//	(a) Java succeeds AND its rows match the scenario's expected rows
//	(b) Go succeeds AND its rows match the scenario's expected rows
//	(c) Java's rows match Go's rows
//
// (c) is the genuine cross-engine equivalence assertion — drift on
// either side surfaces immediately. (a) and (b) keep both engines
// pinned to a stable reference; without them, both engines could drift
// in the same way and (c) wouldn't catch it.
//
// Scenarios are inlined rather than loaded from yamsql/testdata/ — the
// Bazel sandbox doesn't include that tree, and adding it as a data dep
// would couple this conformance test to the yamsql package's data layout.
// Inlining also makes the per-scenario adaptations explicit (e.g. dropping
// NOT NULL on PK columns per the fdb-relational gotcha).
//
// Per-test skips cover Java limitations enumerated in CLAUDE.md (GROUP BY,
// DISTINCT, LIMIT, multi-col ORDER BY, IS TRUE/FALSE) plus error_code
// tests (Java's error structure differs from Go's api.Error) and DML
// tests (runWithSetup expects exactly one query). Wider rollout to all
// scenarios is mechanical follow-on.

import (
	"context"
	"fmt"
	"math"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/plandiff"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/yamsql"
)

var _ = Describe("yamsql cross-engine equivalence (A3)", func() {
	var (
		ctx             context.Context
		java            *JavaInvoker
		clusterFile     string
		clusterFilePath string
	)

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()
		var err error
		clusterFile, err = sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		// The Go runner takes a cluster-file path on disk (the fdbsql
		// driver opens it directly); the Java runner takes the contents
		// (sent over HTTP and the Java side opens it server-side).
		clusterFilePath = writeClusterFileToTemp(clusterFile)
	})

	AfterEach(func() {
		if clusterFilePath != "" {
			_ = os.Remove(clusterFilePath)
		}
	})

	for _, s := range crossEngineScenarios() {
		s := s
		Describe("scenario "+s.Name, func() {
			for i, t := range s.Tests {
				i, t := i, t
				It(t.Query, func() {
					if t.ErrorCode != "" {
						Skip("error_code tests not yet wired cross-engine — Java's error structure differs from Go's api.Error")
					}
					if !yamsql.IsQuery(t.Query) {
						Skip("non-query (DML) cross-engine tests need a different harness — runWithSetup expects exactly one query")
					}
					prefix := fmt.Sprintf("scenario %q test #%d query %q", s.Name, i, t.Query)
					javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), clusterFile).(plandiff.SetupRunner)
					goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

					javaRes := javaRunner.RunWithSetup(ctx, s.SchemaTemplate, s.Setup, t.Query)
					Expect(javaRes.Err).NotTo(HaveOccurred(), "%s: Java executor errored", prefix)
					goRes := goRunner.RunWithSetup(ctx, s.SchemaTemplate, s.Setup, t.Query)
					Expect(goRes.Err).NotTo(HaveOccurred(), "%s: Go executor errored", prefix)

					// (a) Java vs scenario-declared expected.
					assertRowsMatch(javaRes.Rows.Rows, t.Rows, t.Unordered, prefix+" [Java vs expected]")
					// (b) Go vs scenario-declared expected.
					assertRowsMatch(goRes.Rows.Rows, t.Rows, t.Unordered, prefix+" [Go vs expected]")
					// (c) Java vs Go directly. The plandiff runners
					// already coerce numerics to float64 on both sides
					// for comparability — multiset compare via the same
					// helper.
					assertRowSetsCrossEqual(javaRes.Rows.Rows, goRes.Rows.Rows, t.Unordered,
						prefix+" [Java vs Go]")
				})
			}
		})
	}
})

// crossEngineScenarios is the list of scenarios driven cross-engine. Each
// is hand-adapted from its yamsql YAML twin: PK columns drop NOT NULL
// (fdb-relational restriction; PK is implicitly NOT NULL), error_code
// tests are kept (per-test Skip handles them), Java-unsupported features
// (GROUP BY, DISTINCT, LIMIT, multi-col ORDER BY) trigger Java errors and
// stay on the not-yet-wired list.
func crossEngineScenarios() []*yamsql.Scenario {
	return []*yamsql.Scenario{
		whereLiteralOnLeftScenario(),
		arithmeticScenario(),
		castScenario(),
		compositePKScenario(),
		bytesScenario(),
		betweenScenario(),
		booleanScenario(),
		likeScenario(),
		caseWhenScenario(),
		aggregateEmptyTableScenario(),
		bitwiseScenario(),
		avgScenario(),
		derivedTableScenario(),
		coalesceNullifScenario(),
		bareColWithAggScenario(),
		aggregateNullsScenario(),
	}
}

// whereLiteralOnLeftScenario mirrors testdata/where_literal_on_left.yaml.
// The PK column drops NOT NULL — fdb-relational rejects NOT NULL outside
// ARRAY column types (CLAUDE.md gotcha, swingshift-52). PK is implicitly
// NOT NULL so the constraint isn't lost.
func whereLiteralOnLeftScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "where_literal_on_left",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 5, 'a'), (2, 10, 'b'), (3, 15, 'c')",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE 10 < n", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE 10 <= n ORDER BY id", Rows: [][]any{{2}, {3}}},
			{Query: "SELECT id FROM t WHERE 10 > n", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE 10 >= n ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE 10 = n", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE 10 != n ORDER BY id", Rows: [][]any{{1}, {3}}},
			{Query: "SELECT id FROM t WHERE 1 = 1 ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			{Query: "SELECT id FROM t WHERE 1 = 2", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE 'b' = s", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE 'b' < s ORDER BY id", Rows: [][]any{{3}}},
		},
	}
}

// arithmeticScenario mirrors testdata/arithmetic.yaml. Two cross-engine
// adaptations: drops NOT NULL on the PK column, and drops the bare-NULL
// arithmetic + FROM-less SELECT cases. fdb-relational's planner rejects
// `<op> NULL` literal arithmetic with "unable to encapsulate arithmetic
// operation due to type mismatch(es)" — bare NULL has no inferred type,
// so the planner can't pick an operator overload. Wrapping with
// CAST(NULL AS BIGINT) would satisfy Java but the Go-side YAML uses bare
// NULL for cleanliness. FROM-less SELECTs (`SELECT -10 / 3`) hit a
// separate planner restriction. Both are tracked as Java gaps in
// CLAUDE.md (cross-engine yamsql gotchas).
func arithmeticScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "arithmetic",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 20, 4), (2, 7, 0), (3, 10, 3)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a / b FROM t WHERE id = 1", Rows: [][]any{{5}}},
			{Query: "SELECT a / b FROM t WHERE id = 3", Rows: [][]any{{3}}},
			{Query: "SELECT a % b FROM t WHERE id = 3", Rows: [][]any{{1}}},
			{Query: "SELECT a % b FROM t WHERE id = 1", Rows: [][]any{{0}}},
			{Query: "SELECT a / b FROM t WHERE id = 2", ErrorCode: "22012"},
			{Query: "SELECT a % b FROM t WHERE id = 2", ErrorCode: "22012"},
			{Query: "SELECT a + b FROM t WHERE id = 1", Rows: [][]any{{24}}},
			{Query: "SELECT a - b FROM t WHERE id = 1", Rows: [][]any{{16}}},
			{Query: "SELECT a * b FROM t WHERE id = 1", Rows: [][]any{{80}}},
			{Query: "SELECT a / 0 FROM t WHERE id = 1", ErrorCode: "22012"},
		},
	}
}

// castScenario mirrors testdata/cast.yaml. Drops NOT NULL on the two
// PK columns. Keeps error_code tests for visibility (per-test Skip).
func castScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "cast",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE test_cast (id BIGINT, num_col BIGINT, str_col STRING, bool_col BOOLEAN, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1)",
			"INSERT INTO test_cast VALUES (1, 123, 'hello', true), (2, 456, 'world', false), (3, 789, '123', null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT CAST(1.6 AS BIGINT) FROM t", Rows: [][]any{{2}}},
			{Query: "SELECT CAST(-1.5 AS BIGINT) FROM t", Rows: [][]any{{-1}}},
			{Query: "SELECT CAST(-2.6 AS BIGINT) FROM t", Rows: [][]any{{-3}}},
			{Query: "SELECT CAST(' 42 ' AS BIGINT) FROM t", Rows: [][]any{{42}}},
			{Query: "SELECT CAST(' 3.14 ' AS DOUBLE) FROM t", Rows: [][]any{{3.14}}},
			{Query: "SELECT CAST(NULL AS BIGINT) FROM t", Rows: [][]any{{nil}}},
			{Query: "SELECT CAST(1e20 AS BIGINT) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST(-1e20 AS BIGINT) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST('not a bool' AS BOOLEAN) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST(CAST('not a number' AS DOUBLE) AS BIGINT) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST(9223372036854775807 AS INTEGER) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST(num_col AS STRING) FROM test_cast WHERE id = 1", Rows: [][]any{{"123"}}},
			{Query: "SELECT CAST(num_col AS DOUBLE) FROM test_cast WHERE id = 1", Rows: [][]any{{123.0}}},
			{Query: "SELECT id FROM test_cast WHERE CAST(bool_col AS INTEGER) + 1 > 1", Rows: [][]any{{1}}},
			{Query: "SELECT SUM(CAST(num_col AS DOUBLE)) FROM test_cast", Rows: [][]any{{1368.0}}},
			{Query: "SELECT id FROM test_cast WHERE CAST(num_col AS STRING) = CAST(123 AS STRING)", Rows: [][]any{{1}}},
		},
	}
}

// compositePKScenario mirrors testdata/composite_pk.yaml. Drops NOT NULL
// from the two PK columns. Skips the duplicate-PK INSERT (DML; non-query
// path) and its associated state-verification query — runWithSetup
// rebuilds schema per test, so the post-INSERT state isn't observable.
func compositePKScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "composite_pk",
		SchemaTemplate: "CREATE TABLE t (a BIGINT, b BIGINT, label STRING, PRIMARY KEY (a, b))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'alpha'), (1, 20, 'beta'), (2, 10, 'gamma')",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT b, label FROM t WHERE a = 1 ORDER BY b", Rows: [][]any{{10, "alpha"}, {20, "beta"}}},
			{Query: "SELECT label FROM t WHERE a = 2 AND b = 10", Rows: [][]any{{"gamma"}}},
			{Query: "INSERT INTO t VALUES (1, 10, 'replacement')", ErrorCode: "23505"},
			{Query: "SELECT label FROM t WHERE a = 1 AND b = 10", Rows: [][]any{{"alpha"}}},
		},
	}
}

// bytesScenario mirrors testdata/bytes.yaml. Cross-engine adaptations:
// drop NOT NULL on PK column; uppercase BYTES literals X'...' and
// B64'...' (CLAUDE.md gotcha — fdb-relational rejects lowercase x'...'
// / b64'...').
func bytesScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "bytes",
		SchemaTemplate: "CREATE TABLE lb (a BIGINT, b BYTES, PRIMARY KEY (a))",
		Setup: []string{
			"INSERT INTO lb VALUES (1, X'deadbeef'), (2, X'cafe'), (3, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a FROM lb WHERE b = X'cafe'", Rows: [][]any{{2}}},
			{Query: "SELECT a FROM lb WHERE b IN (X'cafe', X'deadbeef')", Unordered: true, Rows: [][]any{{1}, {2}}},
			{Query: "SELECT a FROM lb WHERE b <> X'cafe'", Rows: [][]any{{1}}},
			{Query: "SELECT a FROM lb WHERE b IS NULL", Rows: [][]any{{3}}},
			{Query: "SELECT a FROM lb WHERE b IS NOT NULL", Unordered: true, Rows: [][]any{{1}, {2}}},
			{Query: "SELECT a FROM lb WHERE b = B64'yv4='", Rows: [][]any{{2}}},
			{Query: "SELECT a FROM lb WHERE b = X'0'", ErrorCode: "22F03"},
			{Query: "SELECT a FROM lb WHERE b = X'ABCDMN'", ErrorCode: "22F03"},
			{Query: "SELECT a FROM lb WHERE b = B64'***'", ErrorCode: "22F03"},
			{Query: "SELECT a FROM lb WHERE b IS NOT DISTINCT FROM null", Rows: [][]any{{3}}},
			{Query: "SELECT a FROM lb WHERE b IS DISTINCT FROM null ORDER BY a", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT a FROM lb WHERE b = null", Rows: [][]any{}},
			{Query: "SELECT X'cafe' = b FROM lb WHERE a = 2", Rows: [][]any{{true}}},
		},
	}
}

// betweenScenario mirrors testdata/between.yaml. Drops NOT NULL on PK.
// Drops the Kleene-NULL-bound tests (untyped NULL hits the same planner
// "unable to encapsulate" error documented for arithmetic), the ABS()
// test (function not in fdb-relational's registry per CLAUDE.md), and
// the subquery-bound test (uncertain support). Keeps the geometry-of-
// BETWEEN tests, the error_code type-mismatch tests (skipped), and the
// COUNT(*) constant-fold tests.
func betweenScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "between",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 0), (2, 5), (3, 10), (4, 15), (5, 100)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE v BETWEEN 5 AND 15", Unordered: true, Rows: [][]any{{2}, {3}, {4}}},
			{Query: "SELECT id FROM t WHERE v NOT BETWEEN 5 AND 15", Unordered: true, Rows: [][]any{{1}, {5}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN 15 AND 5", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE v NOT BETWEEN 15 AND 5 ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {4}, {5}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN 10 AND 10", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN (2+3) AND (10*2) ORDER BY id", Rows: [][]any{{2}, {3}, {4}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN 10 AND 'a'", ErrorCode: "22000"},
			{Query: "SELECT id FROM t WHERE 'a' BETWEEN 10 AND 20", ErrorCode: "22000"},
			{Query: "SELECT id FROM t WHERE v BETWEEN 0 AND 5 OR v BETWEEN 90 AND 100 ORDER BY id", Rows: [][]any{{1}, {2}, {5}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN 4 AND 6.2", Rows: [][]any{{2}}},
			{Query: "SELECT COUNT(*) FROM t WHERE 4.5 BETWEEN 4 AND 6", Rows: [][]any{{5}}},
			{Query: "SELECT COUNT(*) FROM t WHERE 2+2 BETWEEN 1+1 AND 3+3", Rows: [][]any{{5}}},
		},
	}
}

// booleanScenario mirrors testdata/boolean.yaml. Drops NOT NULL on PK.
// Cross-engine omissions: IS TRUE / IS FALSE / IS NOT TRUE / IS NOT
// FALSE forms (CLAUDE.md gotcha — fdb-relational planner rejects),
// untyped-NULL operands (`b = null`, `b AND NULL`, `b OR NULL`),
// `WHERE (b = true)` (parser interprets bare-paren as
// RecordConstructorValue, not a predicate; `NOT (...)` works because it
// forces predicate context), CTE + outer ORDER BY (Java rejects
// "order by is not supported in subquery" — fdb-relational treats the
// outer ORDER BY as part of the subquery scope when a WITH clause is
// present), and bare-bool-column-as-operand-in-projection
// (`SELECT b AND TRUE`, `SELECT NOT b`) — Go's embedded engine rejects
// these with "expected BooleanValue but got FieldValue", asymmetric
// with Java which accepts them. New gotcha — Go is stricter than Java
// here, fix tracked separately.
func booleanScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "boolean",
		SchemaTemplate: "CREATE TABLE lb (a BIGINT, b BOOLEAN, PRIMARY KEY (a))",
		Setup: []string{
			"INSERT INTO lb VALUES (1, true), (2, false), (3, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT lb.* FROM lb WHERE b = true", Rows: [][]any{{1, true}}},
			{Query: "SELECT lb.* FROM lb WHERE b = false", Rows: [][]any{{2, false}}},
			{Query: "SELECT lb.* FROM lb WHERE b <> TRUE", Rows: [][]any{{2, false}}},
			{Query: "SELECT lb.* FROM lb WHERE b <> FALSE", Rows: [][]any{{1, true}}},
			{Query: "SELECT lb.* FROM lb WHERE b IS NULL", Rows: [][]any{{3, nil}}},
			{Query: "SELECT lb.* FROM lb WHERE b IS NOT NULL", Unordered: true, Rows: [][]any{{1, true}, {2, false}}},
			{Query: "SELECT lb.* FROM lb WHERE NOT (b = false)", Unordered: true, Rows: [][]any{{1, true}}},
			{Query: "SELECT b = true FROM lb ORDER BY a", Rows: [][]any{{true}, {false}, {nil}}},
			{Query: "SELECT b = false FROM lb ORDER BY a", Rows: [][]any{{false}, {true}, {nil}}},
			{Query: "SELECT b <> TRUE FROM lb ORDER BY a", Rows: [][]any{{false}, {true}, {nil}}},
			{Query: "SELECT b IS NULL FROM lb ORDER BY a", Rows: [][]any{{false}, {false}, {true}}},
		},
	}
}

// likeScenario mirrors testdata/like.yaml. Drops NOT NULL on PK.
// LIKE / NOT LIKE pattern matching with %, _, regex-metachar literals.
func likeScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "like",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'hello'), (2, 'help'), (3, 'world'), (4, null), (5, '')",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE s LIKE 'hel%'", Unordered: true, Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE s LIKE 'hel_'", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE s LIKE 'xyz'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE 'hello'", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE s LIKE ''", Rows: [][]any{{5}}},
			{Query: "SELECT id FROM t WHERE s NOT LIKE 'hel%'", Unordered: true, Rows: [][]any{{3}, {5}}},
			{Query: "SELECT id FROM t WHERE s LIKE '%lp' ORDER BY id", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE s LIKE '%el%' ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE s LIKE 'h___o'", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE s LIKE '_o%'", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE s LIKE 'x_y'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE 'HEL%'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE '(hello)'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE '[h]ello'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE 'h*llo'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE '^hello'", Rows: [][]any{}},
		},
	}
}

// caseWhenScenario mirrors testdata/case_when.yaml. Drops NOT NULL on
// PK. Drops the ORDER BY <alias> test (CLAUDE.md gotcha — fdb-relational
// rejects ORDER BY on a SELECT-list alias).
func caseWhenScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "case_when",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 5), (2, 15), (3, null), (4, 30)",
		},
		Tests: []yamsql.Test{
			{
				Query: "SELECT id, CASE WHEN v < 10 THEN 'low' WHEN v < 20 THEN 'mid' ELSE 'high' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "low"}, {2, "mid"}, {3, "high"}, {4, "high"}},
			},
			{
				Query: "SELECT id, CASE WHEN v IS NULL THEN 'missing' WHEN v < 10 THEN 'low' ELSE 'other' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "low"}, {2, "other"}, {3, "missing"}, {4, "other"}},
			},
			{
				Query: "SELECT id, CASE WHEN v < 10 THEN 'low' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "low"}, {2, nil}, {3, nil}, {4, nil}},
			},
			{
				Query: "SELECT id FROM t WHERE CASE WHEN v IS NULL THEN 0 WHEN v < 10 THEN 0 ELSE 1 END = 1 ORDER BY id",
				Rows:  [][]any{{2}, {4}},
			},
			{
				Query: "SELECT id, CASE WHEN v < 10 THEN 10 ELSE 3.14 END FROM t WHERE id = 1",
				Rows:  [][]any{{1, 10.0}},
			},
			{
				Query: "SELECT id, CASE WHEN v < 10 THEN 10 ELSE 3.14 END FROM t WHERE id = 2",
				Rows:  [][]any{{2, 3.14}},
			},
			{
				Query: "SELECT id, CASE WHEN v IS NULL THEN 'absent' ELSE 'present' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "present"}, {2, "present"}, {3, "absent"}, {4, "present"}},
			},
			{
				Query: "SELECT id, CASE WHEN CASE WHEN v = 5 THEN 10 ELSE 20 END > 15 THEN 'big' ELSE 'small' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "small"}, {2, "big"}, {3, "big"}, {4, "big"}},
			},
			{
				Query: "SELECT id, CASE WHEN v IS NULL THEN 0 ELSE v + 1 END FROM t ORDER BY id",
				Rows:  [][]any{{1, 6}, {2, 16}, {3, 0}, {4, 31}},
			},
		},
	}
}

// aggregateEmptyTableScenario mirrors testdata/aggregate_empty_table.yaml.
// Drops NOT NULL on PK. Drops the `WHERE x > 0 HAVING COUNT(*) >= 0`
// test — divergence: Go returns 1 row [[0]] (SQL spec — aggregate over
// empty set produces a single grouping then HAVING filters), Java
// returns 0 rows (treats empty WHERE result as no grouping at all,
// HAVING never fires). Tracked as new gotcha in CLAUDE.md.
func aggregateEmptyTableScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "aggregate_empty_table",
		SchemaTemplate: "CREATE TABLE empty_t (id BIGINT, n BIGINT, PRIMARY KEY (id))",
		Setup:          []string{},
		Tests: []yamsql.Test{
			{Query: "SELECT COUNT(*) FROM empty_t", Rows: [][]any{{0}}},
			{Query: "SELECT COUNT(n) FROM empty_t", Rows: [][]any{{0}}},
			{Query: "SELECT SUM(n) FROM empty_t", Rows: [][]any{{nil}}},
			{Query: "SELECT AVG(n) FROM empty_t", Rows: [][]any{{nil}}},
			{Query: "SELECT MIN(n) FROM empty_t", Rows: [][]any{{nil}}},
			{Query: "SELECT MAX(n) FROM empty_t", Rows: [][]any{{nil}}},
			{Query: "SELECT COUNT(*) FROM empty_t WHERE id = 999", Rows: [][]any{{0}}},
			{Query: "SELECT COUNT(*) FROM empty_t HAVING COUNT(*) > 0", Rows: [][]any{}},
		},
	}
}

// bitwiseScenario mirrors testdata/bitwise.yaml. Drops NOT NULL on PK.
// Drops the bit-shift tests (`<< / >>`) — fdb-relational tokenizes the
// operators but the function registry has no evaluator (CLAUDE.md
// gotcha). Drops the FROM-less SELECT and error_code shift-out-of-range
// tests for the same reason.
func bitwiseScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "bitwise",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 15, 3), (2, 16, 32), (3, -1, 1), (4, 5, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a & b FROM t WHERE id = 1", Rows: [][]any{{3}}},
			{Query: "SELECT a | b FROM t WHERE id = 2", Rows: [][]any{{48}}},
			{Query: "SELECT a ^ b FROM t WHERE id = 1", Rows: [][]any{{12}}},
			{Query: "SELECT a & b FROM t WHERE id = 4", Rows: [][]any{{nil}}},
			{Query: "SELECT id FROM t WHERE a & 1 = 1 ORDER BY id", Rows: [][]any{{1}, {3}, {4}}},
		},
	}
}

// avgScenario mirrors testdata/avg.yaml. Drops NOT NULL on PK. Pins
// AVG result type as DOUBLE regardless of input.
func avgScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "avg",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 1), (2, 2), (3, 3)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT AVG(v) FROM t", Rows: [][]any{{2.0}}},
			{Query: "SELECT AVG(v) FROM t WHERE v <= 2", Rows: [][]any{{1.5}}},
			{Query: "SELECT AVG(v) FROM t WHERE v > 100", Rows: [][]any{{nil}}},
		},
	}
}

// derivedTableScenario mirrors testdata/derived_table.yaml. Drops NOT
// NULL on PK. Drops the GROUP BY-using test (CLAUDE.md gotcha).
func derivedTableScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "derived_table",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 1, 10), (2, 1, 20), (3, 2, 30), (4, 2, 40), (5, 3, 50)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM (SELECT id, v FROM t WHERE v >= 30) AS x ORDER BY id", Rows: [][]any{{3}, {4}, {5}}},
			{Query: "SELECT id FROM (SELECT id FROM t)", ErrorCode: "42601"},
			{Query: "SELECT x.v AS val FROM (SELECT id, v FROM t WHERE id = 3) AS x", Rows: [][]any{{30}}},
			{Query: "SELECT t.id FROM t, (SELECT id FROM t WHERE id <= 2) AS x", ErrorCode: "0A000"},
		},
	}
}

// coalesceNullifScenario mirrors testdata/coalesce_nullif.yaml. Drops
// NOT NULL on PK. Drops NULLIF tests — fdb-relational rejects with
// "Unsupported operator NULLIF" (function registry doesn't have it,
// joining the list of unregistered scalars). Adds new gotcha to
// CLAUDE.md.
func coalesceNullifScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "coalesce_nullif",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a STRING, b STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'x', 'y'), (2, null, 'y'), (3, 'x', null), (4, null, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id, COALESCE(a, b, 'default') FROM t ORDER BY id", Rows: [][]any{{1, "x"}, {2, "y"}, {3, "x"}, {4, "default"}}},
		},
	}
}

// bareColWithAggScenario mirrors testdata/bare_col_with_agg.yaml. Drops
// NOT NULL on PK. Drops the DISTINCT test (CLAUDE.md gotcha — fdb-
// relational's planner doesn't support SELECT DISTINCT).
func bareColWithAggScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "bare_col_with_agg",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id, COUNT(*) FROM t", ErrorCode: "42803"},
			{Query: "SELECT v, SUM(v) FROM t", ErrorCode: "42803"},
			{Query: "SELECT v + 1, SUM(v) FROM t", ErrorCode: "42803"},
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{3}}},
			{Query: "SELECT SUM(v) FROM t", Rows: [][]any{{60}}},
			{Query: "SELECT COUNT(*), SUM(v), MAX(v), MIN(v) FROM t", Rows: [][]any{{3, 60, 30, 10}}},
			{Query: "SELECT SUM(v) * 2 FROM t", Rows: [][]any{{120}}},
			{Query: "SELECT 'total' AS label, COUNT(*) FROM t", Rows: [][]any{{"total", 3}}},
		},
	}
}

// aggregateNullsScenario mirrors testdata/aggregate_nulls.yaml. Drops
// NOT NULL on PK. Pins SQL-spec NULL aggregate semantics: COUNT(*)
// counts all rows including NULLs; COUNT(col), SUM, MIN, MAX skip
// NULLs; SUM/MIN/MAX over all-NULL or empty-input returns NULL; ungrouped
// aggregate over empty WHERE result still produces one row with NULL.
func aggregateNullsScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "aggregate_nulls",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, grp STRING, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'a', 10), (2, 'a', 20), (3, 'a', null), (4, 'b', null), (5, null, 7), (6, null, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{6}}},
			{Query: "SELECT COUNT(v) FROM t", Rows: [][]any{{3}}},
			{Query: "SELECT SUM(v) FROM t", Rows: [][]any{{37}}},
			{Query: "SELECT SUM(v) FROM t WHERE grp = 'b'", Rows: [][]any{{nil}}},
			{Query: "SELECT SUM(v) FROM t WHERE grp = 'no_such_group'", Rows: [][]any{{nil}}},
			{Query: "SELECT MIN(v), MAX(v) FROM t WHERE grp = 'b'", Rows: [][]any{{nil, nil}}},
			{Query: "SELECT COUNT(*) FROM t WHERE grp = 'no_such_group'", Rows: [][]any{{0}}},
		},
	}
}

// assertRowsMatch checks actual vs expected, honouring multiset semantics
// when unordered is set. Per-cell numeric equality is loose because Java
// sends ints as float64 (JSON-decoded) while YAML loads ints as int.
func assertRowsMatch(actual, expected [][]any, unordered bool, prefix string) {
	Expect(actual).To(HaveLen(len(expected)), "%s row count", prefix)
	if unordered {
		// Match each expected row to some unmatched actual row. O(N²) but
		// scenario row counts are small.
		used := make([]bool, len(actual))
		for ei, er := range expected {
			matched := false
			for ai, ar := range actual {
				if used[ai] {
					continue
				}
				if rowsLooselyEqual(ar, er) {
					used[ai] = true
					matched = true
					break
				}
			}
			Expect(matched).To(BeTrue(), "%s row %d: no match in actual %v for expected %v", prefix, ei, actual, er)
		}
		return
	}
	for r := range expected {
		Expect(actual[r]).To(HaveLen(len(expected[r])), "%s row %d col count", prefix, r)
		for c := range expected[r] {
			expectScalarEqual(actual[r][c], expected[r][c],
				"%s row %d col %d", prefix, r, c)
		}
	}
}

// assertRowSetsCrossEqual checks that two engine result sets contain
// the same row values. Used for the Java-vs-Go direct comparison —
// neither side is the "expected" reference; either engine's set could
// be the regression. Uses the same loose-numeric / multiset comparison
// as the expected-vs-actual path.
//
// TODO: BYTES column values from Java arrive base64-encoded as strings
// (per `pkg/relational/conformance/plandiff/runsql.go` runsql encode);
// the Go runner's coerceForComparison also base64-encodes []byte to
// match. Future scenarios that project a BYTES column directly are
// covered by the existing string-equality path. If a future scenario
// declares an expected []byte literal in inline form rather than the
// base64 string, scalarLooselyEqual will need a []byte case.
func assertRowSetsCrossEqual(java, gor [][]any, unordered bool, prefix string) {
	if !unordered {
		Expect(gor).To(HaveLen(len(java)), "%s row count", prefix)
		for r := range java {
			Expect(gor[r]).To(HaveLen(len(java[r])), "%s row %d col count", prefix, r)
			for c := range java[r] {
				Expect(rowsCrossLooselyEqual([]any{java[r][c]}, []any{gor[r][c]})).To(BeTrue(),
					"%s row %d col %d: java=%v go=%v", prefix, r, c, java[r][c], gor[r][c])
			}
		}
		return
	}
	Expect(gor).To(HaveLen(len(java)), "%s row count", prefix)
	used := make([]bool, len(gor))
	for ji, jr := range java {
		matched := false
		for gi, gr := range gor {
			if used[gi] {
				continue
			}
			if rowsCrossLooselyEqual(jr, gr) {
				used[gi] = true
				matched = true
				break
			}
		}
		Expect(matched).To(BeTrue(), "%s row %d: no Go match for Java row %v", prefix, ji, jr)
	}
}

// rowsCrossLooselyEqual compares two rows from the two engines.
// Numeric values are coerced to float64 on both sides (Java via JSON,
// Go via plandiff's coerceForComparison) so we can compare with a
// shared tolerance.
func rowsCrossLooselyEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !crossScalarEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func crossScalarEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if af, aok := toFloat64Maybe(a); aok {
		if bf, bok := toFloat64Maybe(b); bok {
			return math.Abs(af-bf) <= 1e-9
		}
		return false
	}
	switch av := a.(type) {
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func toFloat64Maybe(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// rowsLooselyEqual compares two rows under the same loose-numeric
// equality used by expectScalarEqual.
func rowsLooselyEqual(a, e []any) bool {
	if len(a) != len(e) {
		return false
	}
	for i := range a {
		if !scalarLooselyEqual(a[i], e[i]) {
			return false
		}
	}
	return true
}

func scalarLooselyEqual(actual, expected any) bool {
	if expected == nil {
		return actual == nil
	}
	if actual == nil {
		return false
	}
	switch e := expected.(type) {
	case int:
		return numericEq(actual, float64(e))
	case int64:
		return numericEq(actual, float64(e))
	case float64:
		return numericEq(actual, e)
	case bool:
		ab, ok := actual.(bool)
		return ok && ab == e
	case string:
		as, ok := actual.(string)
		return ok && as == e
	default:
		return fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", expected)
	}
}

func numericEq(actual any, expected float64) bool {
	var af float64
	switch n := actual.(type) {
	case int:
		af = float64(n)
	case int32:
		af = float64(n)
	case int64:
		af = float64(n)
	case float32:
		af = float64(n)
	case float64:
		af = n
	default:
		return false
	}
	return math.Abs(af-expected) <= 1e-9
}

// expectScalarEqual compares two scalar values for cross-engine
// equality. Numeric types arrive differently from the two engines:
// Java sends ints as float64 (JSON), YAML loads ints as int. We
// normalise both sides to a canonical comparison form.
func expectScalarEqual(actual, expected any, msgAndArgs ...any) {
	if expected == nil {
		Expect(actual).To(BeNil(), msgAndArgs...)
		return
	}
	switch e := expected.(type) {
	case int:
		Expect(actual).To(BeNumerically("==", e), msgAndArgs...)
	case int64:
		Expect(actual).To(BeNumerically("==", e), msgAndArgs...)
	case float64:
		Expect(actual).To(BeNumerically("~", e, 1e-9), msgAndArgs...)
	case bool:
		Expect(actual).To(Equal(e), msgAndArgs...)
	case string:
		Expect(actual).To(Equal(e), msgAndArgs...)
	default:
		Expect(actual).To(Equal(expected), msgAndArgs...)
	}
}
