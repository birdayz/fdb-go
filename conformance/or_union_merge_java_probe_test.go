//go:build bazelrunfiles

package conformance_test

// Does a disjunction over the primary key plan a merged union in both engines?
//
// Java's union merge or-s the legs' different equality bindings of ID, so
// `WHERE id = 1 OR id = 3` merges the two point scans by ID whether or not the
// query orders, and NOT BETWEEN merges its two ranges the same way. Go's
// ImplementDistinctUnionRule yields the same merge-sort union; the rows are
// pinned beside the plans so a merge that dropped or repeated a record shows.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("OrUnionMergeJavaProbe", func() {
	It("plans a merged union for a primary-key disjunction in both engines", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("orumerge_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		const schema = "CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)"}

		explain := func(r plandiff.RunResult) string {
			if r.Err != nil || len(r.Rows.Rows) == 0 {
				return fmt.Sprintf("ERR(%v)", r.Err)
			}
			return fmt.Sprint(r.Rows.Rows[0][0])
		}
		rows := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			parts := make([]string, 0, len(r.Rows.Rows))
			for _, row := range r.Rows.Rows {
				parts = append(parts, fmt.Sprint(row))
			}
			return strings.Join(parts, " ")
		}

		arms := []struct{ sql, want string }{
			{"SELECT id FROM t WHERE id = 1 OR id = 3 ORDER BY id", "[1] [3]"},
			{"SELECT id FROM t WHERE id = 1 OR id = 3 ORDER BY id DESC", "[3] [1]"},
			{"SELECT id FROM t WHERE id = 1 OR id = 3", "[1] [3]"},
			{"SELECT id FROM t WHERE id NOT BETWEEN 2 AND 4 ORDER BY id", "[1] [5]"},
		}
		for _, a := range arms {
			javaPlan := explain(javaRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+a.sql))
			goPlan := explain(goRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+a.sql))
			javaRows := rows(javaRunner.RunWithSetup(ctx, schema, setup, a.sql))
			goRows := rows(goRunner.RunWithSetup(ctx, schema, setup, a.sql))
			fmt.Fprintf(GinkgoWriter, "%s\n  java %s -> %s\n  go   %s -> %s\n", a.sql, javaPlan, javaRows, goPlan, goRows)
			Expect(javaPlan).To(ContainSubstring("∪"), "%s: Java no longer plans a union", a.sql)
			Expect(javaPlan).To(ContainSubstring("COMPARE BY (_.ID)"), "%s: Java's union no longer merges by ID", a.sql)
			Expect(goPlan).To(ContainSubstring("MergeSortUnion("), "%s: Go no longer merges the union legs", a.sql)
			Expect(javaRows).To(Equal(a.want), "%s: Java rows", a.sql)
			Expect(goRows).To(Equal(a.want), "%s: Go rows", a.sql)
		}
	})
})
