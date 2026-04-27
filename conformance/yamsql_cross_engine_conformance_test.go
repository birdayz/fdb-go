package conformance_test

// Track A3 — yamsql semantic equivalence on the cross-engine plandiff
// harness. dayshift-55 starter wiring: drives a hand-picked yamsql
// scenario through Java fdb-relational via plandiff's runWithSetup
// step and asserts the result rows match what the scenario declares.
//
// The Go side already validates each scenario via yamsql.Run (driver
// + database/sql). This bridge proves that Java agrees too — the
// scenario's expected rows aren't just Go-self-consistent but also
// match upstream Java's behaviour.
//
// Scope (this shift, single scenario): one simple WHERE-literal-on-
// left scenario as proof-of-concept. Many existing yamsql scenarios
// trip Java limitations (GROUP BY, DISTINCT, LIMIT, multi-col ORDER
// BY, IS TRUE/FALSE, error_code mismatches) — those need either Java-
// side support or a per-scenario `cross_engine: false` opt-out.
// Wider rollout to all simple scenarios is the next-shift follow-on.

import (
	"context"

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

	It("Java agrees with where_literal_on_left scenario", func() {
		// Inline scenario — Bazel sandbox doesn't include the yamsql
		// testdata/ tree, and adding it as a data dep would couple
		// this conformance test to the yamsql package's data layout.
		// Mirror the byte-for-byte content of
		// pkg/relational/conformance/yamsql/testdata/where_literal_on_left.yaml
		// (which is itself a regression test for the same scenario
		// against the Go embedded engine — when this scenario evolves,
		// update both files together).
		scenario := whereLiteralOnLeftScenario()

		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), clusterFile).(plandiff.SetupRunner)

		for i, t := range scenario.Tests {
			i, t := i, t
			By(t.Query, func() {
				if t.ErrorCode != "" {
					Skip("error_code tests not yet wired cross-engine — Java's error structure differs from Go's api.Error")
				}
				if !isSelectLike(t.Query) {
					Skip("non-query (DML) cross-engine tests need a different harness — runWithSetup expects exactly one query")
				}
				result := javaRunner.RunWithSetup(ctx, scenario.SchemaTemplate, scenario.Setup, t.Query)
				Expect(result.Err).NotTo(HaveOccurred(),
					"scenario %q test #%d query %q: Java executor errored", scenario.Name, i, t.Query)

				// Compare row-by-row against the scenario's declared
				// expected rows. Java's RowSet.Rows is [][]any —
				// numeric values arrive as float64 (JSON-decoded);
				// the scenario's t.Rows is [][]any from YAML where
				// integers come through as int. Per-cell numeric
				// equality is loose.
				Expect(result.Rows.Rows).To(HaveLen(len(t.Rows)),
					"row count: scenario %q test #%d", scenario.Name, i)
				for r := range t.Rows {
					Expect(result.Rows.Rows[r]).To(HaveLen(len(t.Rows[r])),
						"col count: scenario %q test #%d row %d", scenario.Name, i, r)
					for c := range t.Rows[r] {
						expectScalarEqual(result.Rows.Rows[r][c], t.Rows[r][c],
							"scenario %q test #%d row %d col %d", scenario.Name, i, r, c)
					}
				}
			})
		}
	})
})

// whereLiteralOnLeftScenario mirrors testdata/where_literal_on_left.yaml
// with one cross-engine adaptation: the PK column drops `NOT NULL`. Per
// CLAUDE.md gotcha (swingshift-52), fdb-relational rejects `NOT NULL`
// outside ARRAY column types; primary-key columns are implicitly NOT
// NULL so the constraint isn't lost. The Go-side yamsql YAML still
// uses `NOT NULL` because Go's engine accepts it. Cross-engine yamsql
// expansion will need a per-scenario override mechanism; for the
// starter, hand-adapt.
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

// isSelectLike — duplicated from yamsql.runner's isQuery (small enough
// to copy rather than export).
func isSelectLike(q string) bool {
	for _, r := range q {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '(' {
			continue
		}
		// First non-space char.
		switch r {
		case 's', 'S', 'w', 'W', 'v', 'V':
			// Heuristic: SELECT/WITH/VALUES start with these.
			return true
		default:
			return false
		}
	}
	return false
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
