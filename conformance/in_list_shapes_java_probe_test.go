//go:build bazelrunfiles

package conformance_test

// Which IN-list shapes does each engine accept?
//
// Go's resolver wires exactly ONE of the grammar's InList branches — the
// `expressions` list — and answers the others before the planner sees them:
// a subquery is declined as "IN with subquery" and anything else (a bare
// column reference, a prepared parameter) as InColumnRefError. Java's
// visitInList (ExpressionVisitor.java:635-657) has four branches:
//
//	preparedStatementParameter  -> the parameter supplies the list
//	fullColumnName              -> a column reference, asserted to be ARRAY-typed
//	all-constant expressions    -> an array literal through the literal pipeline
//	expressions                 -> resolveFunction("__internal_array", items…)
//
// So the reading is that Java accepts more here. Two of those are worth
// measuring rather than reading off, and one of them is not visible in the
// branch structure at all:
//
//   - a NON-CONSTANT item among constants (`b IN (a, 20)`) takes Java's LAST
//     branch and is a perfectly ordinary __internal_array. Go RESOLVES it and
//     then fails in the PLANNER with 0AF00 "Cascades planner could not plan
//     query" — a different layer from the declines above, which is why it does
//     not appear in the resolver's branch list.
//   - an ARRAY-typed column as the whole list (`b IN (arr)`) is Java's second
//     branch and Go's InColumnRefError.
//
// The subquery form is expected to agree: Java asserts
// "IN predicate does not support nested SELECT" and Go declines it too, which
// is the conformance principle working as intended (doesn't work in Java ->
// doesn't work in Go). It is probed as the CONTROL — the arm that says a
// disagreement elsewhere is a real gap and not this harness rejecting
// everything.
//
// This file MEASURES. It asserts only the control and prints the rest, because
// what to do about a gap depends on which layer refuses and that is the next
// question, not this one.

import (
	"context"
	"fmt"
	"os"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("InListShapesJavaProbe", func() {
	It("measures both engines on every IN-list branch", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("inlist_%s", uuid.New().String())
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
		setup := []string{"INSERT INTO t (id,a,b) VALUES (1,10,10),(2,7,20),(3,30,30)"}

		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}

		cases := []struct{ name, sql string }{
			// The branch both engines wire. Its agreement is what makes the rest
			// interpretable.
			{"all_constant_items", "SELECT id FROM t WHERE b IN (10, 20) ORDER BY id"},

			// A COLUMN among the items. id=1 and id=3 have a = b, so a correct
			// answer is [[1] [3]] — and note it is NOT the same answer as any
			// constant list, so an engine that silently ignored the column item
			// would be visible here rather than accidentally right.
			{"column_item_among_constants", "SELECT id FROM t WHERE b IN (a, 999) ORDER BY id"},
			{"column_item_only", "SELECT id FROM t WHERE b IN (a) ORDER BY id"},
			{"column_item_arithmetic", "SELECT id FROM t WHERE b IN (a + 0, 999) ORDER BY id"},
			{"column_item_negated", "SELECT id FROM t WHERE b NOT IN (a, 999) ORDER BY id"},

			// The CONTROL: expected to be refused by BOTH, Java by an explicit
			// assert and Go by an explicit decline.
			{"subquery", "SELECT id FROM t WHERE b IN (SELECT b FROM t WHERE id = 1) ORDER BY id"},
		}

		var disagree []string
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			mark := "  "
			if javaOut != goOut {
				mark = "!!"
				disagree = append(disagree, fmt.Sprintf("%s\n    java: %s\n    go  : %s\n    sql : %s",
					c.name, javaOut, goOut, c.sql))
			}
			fmt.Fprintf(GinkgoWriter, "%s %-28s java=%-46s go=%s\n", mark, c.name, javaOut, goOut)
		}
		fmt.Fprintf(GinkgoWriter, "\nDISAGREEMENTS: %d of %d\n", len(disagree), len(cases))
		for _, d := range disagree {
			fmt.Fprintf(GinkgoWriter, "%s\n", d)
		}

		// The two arms that are asserted. The constant list must agree — if it
		// does not, this fixture is not measuring IN-list BRANCHES, it is
		// measuring something broken about IN itself and nothing else here can
		// be read. The subquery must be refused by both, which is the
		// conformance principle holding.
		javaConst := render(javaRunner.RunWithSetup(ctx, schema, setup, cases[0].sql))
		goConst := render(goRunner.RunWithSetup(ctx, schema, setup, cases[0].sql))
		Expect(goConst).To(Equal(javaConst),
			"the engines disagree on a plain constant IN list, so nothing else in this file is "+
				"interpretable\n  java: %s\n  go  : %s", javaConst, goConst)
		Expect(goConst).To(Equal("[[1] [2]]"), "the constant-list control answered wrongly")

		javaSub := render(javaRunner.RunWithSetup(ctx, schema, setup, cases[len(cases)-1].sql))
		goSub := render(goRunner.RunWithSetup(ctx, schema, setup, cases[len(cases)-1].sql))
		Expect(javaSub).To(ContainSubstring("ERR"),
			"Java now accepts IN (SELECT …). Its explicit assert is what Go's decline is aligned "+
				"with, so that alignment is now a gap rather than parity: re-measure and open it.")
		Expect(goSub).To(ContainSubstring("ERR"),
			"Go now accepts IN (SELECT …) while Java refuses it — the conformance principle runs "+
				"the other way here (doesn't work in Java -> doesn't work in Go)")
	})
})
