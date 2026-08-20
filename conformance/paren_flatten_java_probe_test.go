//go:build bazelrunfiles

package conformance_test

// Where does a one-field record built by `( expr )` get FLATTENED back to a
// scalar?
//
// This grammar parses `( expr )` as a one-field RECORD constructor — the same
// production that builds `(x, y)` — so `(5)` is `{_0: 5}` and not `5`. Java
// does the same thing and resolves the ambiguity by POSITION rather than by
// parse: RecordConstructorValue always builds the record, and every FUNCTION
// ARGUMENT is flattened back by SemanticAnalyzer.resolveScalarFunction, whose
// flattenSingleItemRecords parameter BaseVisitor.resolveFunction defaults to
// true (BaseVisitor.java:254-256).
//
// Go mirrors that with walkFunctionOperand, which flattens for arithmetic,
// comparison, bitwise, logical, NOT, IS NULL, LIKE, IN's left operand and
// BETWEEN — and for a named scalar call's arguments. Java's ExpressionVisitor
// calls resolveFunction at more positions than that list covers. Two of them
// have no Go flatten:
//
//	ExpressionVisitor.java:425  resolveFunction("__pick_value", …)      CASE arms
//	ExpressionVisitor.java:656  resolveFunction("__internal_array", …)  IN-list items
//
// Reading the two visitors says Java flattens there and Go does not. What Java
// ANSWERS is a separate question from what its visitor is shaped like — that
// distinction is exactly what made the permuted-MIN measurement worth taking —
// so this probe asks both engines instead of concluding from the source.
//
// The CONDITION arm is deliberately not probed here: it is measured in
// case_parenthesized_condition_java_probe_test.go, which found Java rejecting
// every parenthesized condition with 42804. That rejection is consistent with
// this file's reading — visitCaseFunctionCall builds ConditionSelectorValue
// from the UNFLATTENED conditions and asserts BOOLEAN on them before
// resolveFunction ever runs, so a condition is the one CASE position Java
// never flattens.

import (
	"context"
	"fmt"
	"os"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ParenFlattenJavaProbe", func() {
	It("measures both engines on a parenthesized operand in every position", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("parenflat_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		const schema = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO t (id,a,b) VALUES (1,1,10),(2,2,20),(3,1,30)"}

		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}

		cases := []struct{ name, sql string }{
			// --- control: no parentheses anywhere. Both engines must agree, and
			// its agreement is what makes a disagreement below attributable to
			// the parentheses rather than to CASE or IN themselves.
			{"control_case_bare", "SELECT id, CASE WHEN a = 1 THEN 5 ELSE 6 END FROM t ORDER BY id"},
			{"control_in_bare", "SELECT id FROM t WHERE b IN (10, 20) ORDER BY id"},

			// --- CASE consequents (ExpressionVisitor.java:425).
			{"case_then_paren", "SELECT id, CASE WHEN a = 1 THEN (5) ELSE 6 END FROM t ORDER BY id"},
			{"case_else_paren", "SELECT id, CASE WHEN a = 1 THEN 5 ELSE (6) END FROM t ORDER BY id"},
			{"case_both_paren", "SELECT id, CASE WHEN a = 1 THEN (5) ELSE (6) END FROM t ORDER BY id"},
			{"case_then_paren_col", "SELECT id, CASE WHEN a = 1 THEN (b) ELSE 0 END FROM t ORDER BY id"},
			{"case_then_nested_paren", "SELECT id, CASE WHEN a = 1 THEN ((5)) ELSE 6 END FROM t ORDER BY id"},
			// A TWO-element record must NOT flatten in either engine — the guard
			// that says a flatten fix did not turn into "unwrap every paren".
			{"case_then_pair", "SELECT id, CASE WHEN a = 1 THEN (5, 6) ELSE (7, 8) END FROM t ORDER BY id"},

			// --- IN-list items (ExpressionVisitor.java:656).
			{"in_list_paren_first", "SELECT id FROM t WHERE b IN ((10), 20) ORDER BY id"},
			{"in_list_paren_all", "SELECT id FROM t WHERE b IN ((10), (20)) ORDER BY id"},
			{"in_list_paren_nested", "SELECT id FROM t WHERE b IN (((10)), 20) ORDER BY id"},

			// --- positions Go ALREADY flattens, as positive controls that the
			// flatten machinery is reachable at all in this fixture.
			{"arith_paren", "SELECT id FROM t WHERE (b) + 0 = 10 ORDER BY id"},
			{"cmp_paren", "SELECT id FROM t WHERE (b) = 10 ORDER BY id"},
			{"between_paren", "SELECT id FROM t WHERE b BETWEEN (10) AND (20) ORDER BY id"},
		}

		var disagreements []string
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			mark := "  "
			if javaOut != goOut {
				mark = "!!"
				disagreements = append(disagreements,
					fmt.Sprintf("%s\n    java: %s\n    go  : %s\n    sql : %s", c.name, javaOut, goOut, c.sql))
			}
			fmt.Fprintf(GinkgoWriter, "%s %-24s java=%-40s go=%s\n", mark, c.name, javaOut, goOut)
		}

		fmt.Fprintf(GinkgoWriter, "\nDISAGREEMENTS: %d of %d\n", len(disagreements), len(cases))
		for _, d := range disagreements {
			fmt.Fprintf(GinkgoWriter, "%s\n", d)
		}
		Expect(disagreements).To(BeEmpty(),
			"the engines disagree on where a one-field record built by `( expr )` is flattened "+
				"back to a scalar\n%v", disagreements)
	})
})
