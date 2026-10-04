//go:build bazelrunfiles

package conformance_test

// Does an IN-union keep records that tie on the projected ORDER BY columns?
//
// Java does not. Over an (a, s, b) index, `SELECT s, b ... WHERE a IN (1, 2)
// ORDER BY s, b` plans
//
//	[IN …] INUNION q0 -> { COVERING(T_ASB [EQUALS q0] …) | MAP (_.S AS S, _.B AS B) } COMPARE BY (_.S, _.B)
//
// and UnionCursor drops every row whose comparison key ties one already
// emitted, so records of different IN branches with equal (s, b) collapse.
// ImplementInUnionRule never asks whether the key identifies a row, and
// Ordering.pullUp keeps isDistinct after the projection drops the primary key.
// Go merges only when the baked inner proves its rows distinct over
// coordinates inside the key (rule_implement_in_union.go,
// inUnionMergeKeyIdentifiesRows), and sorts otherwise. Direction
// DivergenceJavaWrongRowsGoCorrect; TODO.md section 9 books the upstream report.
//
// The controls keep the primary key in the projection, where both engines
// merge soundly, or ask for no order, where neither merges.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("InUnionProjectionDedupJavaProbe", func() {
	It("measures both engines on an IN-union whose projection ties distinct records", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("inuproj_%s", uuid.New().String())
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

		const schema = "CREATE TABLE t (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (pk1, pk2)) " +
			"CREATE INDEX t_asb ON t (a, s, b)"
		// (x, 1) is shared by two a = 1 records and one a = 2 record.
		setup := []string{
			"INSERT INTO t VALUES (1, 1, 1, 1, 'x'), (1, 2, 1, 1, 'x'), (2, 1, 2, 1, 'x'), (2, 2, 2, 2, 'y'), (3, 1, 3, 1, 'x')",
		}

		render := func(r plandiff.RunResult, ordered bool) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			parts := make([]string, 0, len(r.Rows.Rows))
			for _, row := range r.Rows.Rows {
				parts = append(parts, fmt.Sprint(row))
			}
			if !ordered {
				sort.Strings(parts)
			}
			return strings.Join(parts, " ")
		}

		type arm struct {
			name, sql, want string
			ordered         bool
			javaWrong       bool
		}
		arms := []arm{
			{
				name: "projection_s_b", javaWrong: true, ordered: true, want: "[x 1] [x 1] [x 1] [y 2]",
				sql: "SELECT s, b FROM t WHERE a IN (1, 2) ORDER BY s, b",
			},
			{
				name: "projection_s", javaWrong: true, ordered: true, want: "[x] [x] [x] [y]",
				sql: "SELECT s FROM t WHERE a IN (1, 2) ORDER BY s",
			},
			{
				// Ties on (s, b) may come back in either engine's order.
				name: "projection_keeps_pk", want: "[1 1 x 1] [1 2 x 1] [2 1 x 1] [2 2 y 2]",
				sql: "SELECT pk1, pk2, s, b FROM t WHERE a IN (1, 2) ORDER BY s, b",
			},
			{
				name: "unordered", want: "[x 1] [x 1] [x 1] [y 2]",
				sql: "SELECT s, b FROM t WHERE a IN (1, 2)",
			},
		}

		var wrong, agreed int
		for _, a := range arms {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, a.sql), a.ordered)
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, a.sql), a.ordered)
			fmt.Fprintf(GinkgoWriter, "%-20s java=%-40s go=%s\n", a.name, javaOut, goOut)
			Expect(goOut).To(Equal(a.want),
				"%s: Go's rows changed; an IN-union merging on a key that ties distinct records is back.\n  sql: %s",
				a.name, a.sql)
			if a.javaWrong {
				wrong++
				Expect(javaOut).NotTo(Equal(a.want),
					"%s: Java now answers this shape correctly, so the upstream defect is fixed: move the "+
						"arm to the controls and update the TODO.md section 9 and DIVERGENCES.md entries.\n  java: %s\n  sql: %s",
					a.name, javaOut, a.sql)
			} else {
				agreed++
				Expect(javaOut).To(Equal(goOut),
					"%s: a control disagrees, so the fixture or harness moved and the wrong-arm readings "+
						"are not interpretable.\n  java: %s\n  go: %s\n  sql: %s", a.name, javaOut, goOut, a.sql)
			}
		}
		Expect(wrong).To(BeNumerically(">=", 2), "no arm records Java's wrong answer any more")
		Expect(agreed).To(BeNumerically(">=", 2), "the control group collapsed")
		fmt.Fprintf(GinkgoWriter, "\nMEASURED DIVERGENCE: Java's IN-union drops records that tie on a projected "+
			"comparison key; Go merges only on a key that identifies rows. Direction: %s.\n",
			plandiff.DivergenceJavaWrongRowsGoCorrect)
	})
})
