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
// The last branch is the one that mattered. A NON-CONSTANT item among
// constants (`b IN (a, 999)`) is an ordinary __internal_array to Java, compared
// per row; Go folded every IN list at PLAN time and had no other path, so
// ResolveIn answered "element N is not constant" and the query surfaced as
// 0AF00 "Cascades planner could not plan query". MEASURED here before the
// repair: Java [[1] [3]], Go 0AF00, on four separate shapes.
//
// Go now takes the same fork — a non-constant list resolves to an
// ArrayConstructorValue, and the four arms agree with Java exactly. Those arms
// are ASSERTED, not merely printed: each is compared against Java AND pinned to
// its absolute answer, so neither a regression nor the two engines drifting
// together can satisfy it.
//
// THE PLANNER HALF IS WHY THIS IS NOT JUST A RESOLVER CHANGE, and it is
// recorded here because the probe alone would not have caught it.
// InComparisonToExplodeRule folds an IN list with Operand.Evaluate(nil) and
// explodes over the result; fieldValue.Evaluate(nil) answers (nil, nil) rather
// than an error, so accepting the shape in the resolver WITHOUT hardening that
// guard would have exploded over [NULL, 999] — planning cleanly, running, and
// silently answering a different query. The guard now tests IsConstantValue.
// The plan-shape half of that is pinned in
// pkg/relational/sqldriver/in_list_non_constant_fdb_test.go, which asserts both
// that a non-constant list stays a residual filter and that a CONSTANT one
// still reaches the InJoin path.
//
// STILL OPEN, measured and not closed: an ARRAY-typed column as the whole list
// (`b IN (arr)`) is Java's second branch and Go's InColumnRefError.
//
// The subquery form agrees in OUTCOME — both refuse — but Java arrives there by
// NullPointerException rather than by its own "IN predicate does not support
// nested SELECT" assert, so only the outcome is comparable and that arm is
// asserted separately.

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

		cases := []struct {
			name, sql string
			// want is the answer BOTH engines must give. Empty means the arm is
			// measured rather than asserted (the subquery arm, where the two
			// refusals have different wording).
			want string
		}{
			// The branch both engines wire. Its agreement is what makes the rest
			// interpretable.
			{
				name: "all_constant_items", want: "[[1] [2]]",
				sql: "SELECT id FROM t WHERE b IN (10, 20) ORDER BY id",
			},

			// A COLUMN among the items. id=1 and id=3 have a = b, so a correct
			// answer is [[1] [3]] — and note it is NOT the same answer as any
			// constant list, so an engine that silently ignored the column item
			// would be visible here rather than accidentally right.
			{
				name: "column_item_among_constants", want: "[[1] [3]]",
				sql: "SELECT id FROM t WHERE b IN (a, 999) ORDER BY id",
			},
			{
				name: "column_item_only", want: "[[1] [3]]",
				sql: "SELECT id FROM t WHERE b IN (a) ORDER BY id",
			},
			{
				name: "column_item_arithmetic", want: "[[1] [3]]",
				sql: "SELECT id FROM t WHERE b IN (a + 0, 999) ORDER BY id",
			},
			{
				name: "column_item_negated", want: "[[2]]",
				sql: "SELECT id FROM t WHERE b NOT IN (a, 999) ORDER BY id",
			},

			// Refused by BOTH, and measured rather than compared: Java reaches
			// its refusal by NullPointerException rather than by its own
			// "IN predicate does not support nested SELECT" assert, so the two
			// messages cannot be equal and only the OUTCOME is comparable.
			{
				name: "subquery",
				sql:  "SELECT id FROM t WHERE b IN (SELECT b FROM t WHERE id = 1) ORDER BY id",
			},
		}

		var disagree []string
		var asserted int
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

			if c.want == "" {
				continue
			}
			asserted++
			// Compared AND pinned absolutely. The comparison is what makes this
			// a conformance statement; the absolute pin is what stops two
			// engines drifting together from satisfying it.
			Expect(goOut).To(Equal(javaOut),
				"%s: the engines disagree.\n  java: %s\n  go  : %s\n  sql : %s",
				c.name, javaOut, goOut, c.sql)
			Expect(goOut).To(Equal(c.want),
				"%s: both engines agree on the WRONG answer, or the fixture moved.\n"+
					"  got  %s\n  want %s\n  sql : %s", c.name, goOut, c.want, c.sql)
		}
		fmt.Fprintf(GinkgoWriter, "\nDISAGREEMENTS: %d of %d\n", len(disagree), len(cases))
		for _, d := range disagree {
			fmt.Fprintf(GinkgoWriter, "%s\n", d)
		}

		// The vacuity floor: every assertion above is inside the loop and
		// guarded by a non-empty want, so clearing the wants would leave this
		// spec green having compared nothing.
		Expect(asserted).To(Equal(5),
			"%d arms were asserted, not the 5 this file measured against the live JVM — one "+
				"constant list and four non-constant ones. A green from a shrunken set says "+
				"nothing", asserted)

		// The subquery arm, measured rather than compared: both engines must
		// REFUSE it, and only the outcome is comparable because Java arrives at
		// its refusal by NullPointerException.
		javaSub := render(javaRunner.RunWithSetup(ctx, schema, setup, cases[len(cases)-1].sql))
		goSub := render(goRunner.RunWithSetup(ctx, schema, setup, cases[len(cases)-1].sql))
		Expect(javaSub).To(ContainSubstring("ERR"),
			"Java now accepts IN (SELECT …). Its refusal is what Go's decline is aligned with, "+
				"so that alignment is now a gap rather than parity: re-measure and open it.")
		Expect(goSub).To(ContainSubstring("ERR"),
			"Go now accepts IN (SELECT …) while Java refuses it — the conformance principle runs "+
				"the other way here (doesn't work in Java -> doesn't work in Go)")
	})
})
