package conformance_test

// Track A3 — yamsql semantic equivalence on the cross-engine plandiff
// harness. dayshift-55: drives hand-picked yamsql scenarios through Java
// fdb-relational via plandiff's runWithSetup step and asserts the result
// rows match what each scenario declares.
//
// The Go side already validates each scenario via yamsql.Run (driver
// + database/sql). This bridge proves that Java agrees too — the
// scenarios' expected rows aren't just Go-self-consistent but also
// match upstream Java's behaviour.
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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/plandiff"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/yamsql"
)

var _ = Describe("yamsql cross-engine equivalence (A3)", func() {
	var (
		ctx         context.Context
		java        *JavaInvoker
		clusterFile string
	)

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()
		var err error
		clusterFile, err = sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	for _, sf := range crossEngineScenarios() {
		s := sf()
		Describe("scenario "+s.Name, func() {
			s := s
			for i, t := range s.Tests {
				i, t := i, t
				It(t.Query, func() {
					if t.ErrorCode != "" {
						Skip("error_code tests not yet wired cross-engine — Java's error structure differs from Go's api.Error")
					}
					if !isSelectLike(t.Query) {
						Skip("non-query (DML) cross-engine tests need a different harness — runWithSetup expects exactly one query")
					}
					javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), clusterFile).(plandiff.SetupRunner)
					result := javaRunner.RunWithSetup(ctx, s.SchemaTemplate, s.Setup, t.Query)
					Expect(result.Err).NotTo(HaveOccurred(),
						"scenario %q test #%d query %q: Java executor errored", s.Name, i, t.Query)

					assertRowsMatch(result.Rows.Rows, t.Rows, t.Unordered,
						fmt.Sprintf("scenario %q test #%d", s.Name, i))
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
func crossEngineScenarios() []func() *yamsql.Scenario {
	return []func() *yamsql.Scenario{
		whereLiteralOnLeftScenario,
		arithmeticScenario,
		castScenario,
		compositePKScenario,
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

// isSelectLike — duplicated from yamsql.runner's isQuery (small enough
// to copy rather than export).
func isSelectLike(q string) bool {
	for _, r := range q {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '(' {
			continue
		}
		switch r {
		case 's', 'S', 'w', 'W', 'v', 'V':
			return true
		default:
			return false
		}
	}
	return false
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
	switch n := actual.(type) {
	case int:
		return float64(n) == expected
	case int32:
		return float64(n) == expected
	case int64:
		return float64(n) == expected
	case float32:
		return float64(n) == expected
	case float64:
		return n == expected
	default:
		return false
	}
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
