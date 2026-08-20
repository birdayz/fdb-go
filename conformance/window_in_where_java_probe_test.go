//go:build bazelrunfiles

package conformance_test

// A window function is not allowed in WHERE. Does Go say so?
//
// Java rejects it explicitly and by construction rather than by accident:
// visitWhereExpr walks the resolved expression and asserts no WindowedValue
// appears anywhere inside it (ExpressionVisitor.java:662-667), raising
// WINDOWING_ERROR "window functions are not allowed in WHERE". That is the
// standard SQL rule — a window is evaluated after WHERE, so a predicate cannot
// refer to one.
//
// Go has no such check. Grepping the non-generated relational tree for a
// WINDOWING_ERROR producer finds only the yamsql error-code TABLE
// (javayamsql/errorcodes.go maps it to 42F21), never a site that raises it. And
// Go does resolve windows: walk.go wires ROW_NUMBER() OVER (…) for the vector
// K-NN path, so the shape is expressible and reaches the resolver.
//
// This is the direction the conformance principle cares about most. Go
// REFUSING something Java accepts is a reach gap — loud, and visible the moment
// a user hits it. Go ACCEPTING something Java rejects is the silent kind: the
// query runs, returns rows, and nobody learns that the two engines disagree
// about what the rows mean. So the question is measured rather than assumed
// from the absence of a grep hit, which is exactly the sort of absence that has
// been wrong before.
//
// The OVER clauses here are the K-NN shape Go actually supports, so a rejection
// from Go cannot be dismissed as "windows are unsupported anyway": the same
// window in the SELECT list is probed beside it as the control that says the
// expression itself resolves.

import (
	"context"
	"fmt"
	"os"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WindowInWhereJavaProbe", func() {
	It("measures both engines on a window function outside the SELECT list", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("winwhere_%s", uuid.New().String())
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

		const schema = "CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO t (id,g,v) VALUES (1,1,10),(2,1,20),(3,2,30)"}

		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}

		cases := []struct{ name, sql string }{
			// The control: the same window in the position where it IS legal.
			// Whatever each engine does with it, that is the baseline against
			// which the WHERE arms are read — if an engine cannot resolve the
			// window at all, its WHERE rejection says nothing about WHERE.
			{
				"window_in_select_list",
				"SELECT id, ROW_NUMBER() OVER (PARTITION BY g ORDER BY v) FROM t ORDER BY id",
			},

			// The shape Java rejects with WINDOWING_ERROR.
			{
				"window_in_where",
				"SELECT id FROM t WHERE ROW_NUMBER() OVER (PARTITION BY g ORDER BY v) > 1 ORDER BY id",
			},
			{
				"window_in_where_equality",
				"SELECT id FROM t WHERE ROW_NUMBER() OVER (PARTITION BY g ORDER BY v) = 1 ORDER BY id",
			},
			// Buried inside a larger predicate — Java's check is a preOrderStream
			// over the WHOLE expression, so nesting must not hide it.
			{
				"window_buried_in_conjunction",
				"SELECT id FROM t WHERE g = 1 AND ROW_NUMBER() OVER (PARTITION BY g ORDER BY v) > 1 ORDER BY id",
			},
			{
				"window_buried_in_arithmetic",
				"SELECT id FROM t WHERE ROW_NUMBER() OVER (PARTITION BY g ORDER BY v) + 0 > 1 ORDER BY id",
			},
			// HAVING is a separate clause with its own visitor; probed to say
			// whether the rejection (wherever it lives) generalises.
			{
				"window_in_having",
				"SELECT g FROM t GROUP BY g HAVING ROW_NUMBER() OVER (PARTITION BY g ORDER BY g) > 1",
			},
		}

		var rows []string
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			javaRejects := len(javaOut) > 3 && javaOut[:3] == "ERR"
			goRejects := len(goOut) > 3 && goOut[:3] == "ERR"
			mark := "  "
			if javaRejects != goRejects {
				mark = "!!"
			}
			rows = append(rows, fmt.Sprintf("%s %-28s javaRejects=%-5v goRejects=%-5v\n     java: %s\n     go  : %s",
				mark, c.name, javaRejects, goRejects, javaOut, goOut))
			fmt.Fprintf(GinkgoWriter, "%s %-28s java=%-60s go=%s\n", mark, c.name, javaOut, goOut)
		}

		// This file MEASURES; the assertion is only that the measurement
		// happened over a non-empty population, so it cannot pass by running
		// nothing. What to do about a disagreement depends on which direction it
		// points, and that is the next question.
		Expect(rows).To(HaveLen(len(cases)))
		fmt.Fprintf(GinkgoWriter, "\nSUMMARY\n%s\n", joinLines(rows))
	})
})

func joinLines(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s + "\n"
	}
	return out
}
