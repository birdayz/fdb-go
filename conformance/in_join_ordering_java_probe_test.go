//go:build bazelrunfiles

package conformance_test

// Does a sorted IN source deliver the ORDER BY without a sort in both engines?
//
// Java's OrderingProperty.visitInJoinPlan makes the IN value the leading
// sorted key of a sorted InJoin, so Java plans `INJOIN ... SORTED [DESC]` with
// no sort over a primary or index scan bound to the IN value. Go ports the
// claim. Over a non-covering index Java's ASC plan merges an IN-union under a
// fetch, because PushInJoinThroughFetchRule is not registered for
// RecordQueryInComparandJoinPlan (PlanningRuleSet.java:151-152); Go pushes
// every InJoin through the fetch (DIVERGENCES.md, RFC-191). The arm pinning
// Java's IN-union flags the day Java registers the rule.

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

var _ = Describe("InJoinOrderingJavaProbe", func() {
	It("plans a sorted IN-join without a sort in both engines", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("injoinord_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		const schema = "CREATE TABLE tbl (id BIGINT, k BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id, k)) " +
			"CREATE INDEX ia ON tbl (a) " +
			"CREATE TABLE t5 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX i5 AS SELECT col1 FROM t5 ORDER BY col1"
		setup := []string{
			"INSERT INTO tbl VALUES (1, 1, 10, 100), (1, 2, 20, 200), (2, 1, 30, 300), (3, 5, 40, 400), (4, 1, 50, 500)",
			"INSERT INTO t5 VALUES (1, 20, 1), (2, 10, NULL), (3, 20, 3), (4, NULL, NULL), (5, 10, 5)",
		}

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

		// javaWant is empty where Java answers want; Java cannot plan the
		// three-column secondary-index order (UnableToPlanException). javaPlan
		// is the operator Java elects.
		arms := []struct{ sql, want, javaWant, javaPlan string }{
			{"SELECT * FROM tbl WHERE id IN (1, 2, 3) ORDER BY id, k", "[1 1 10 100] [1 2 20 200] [2 1 30 300] [3 5 40 400]", "", "INJOIN"},
			{"SELECT * FROM tbl WHERE id IN (1, 2, 3) ORDER BY id DESC, k DESC", "[3 5 40 400] [2 1 30 300] [1 2 20 200] [1 1 10 100]", "", "INJOIN"},
			{"SELECT * FROM tbl WHERE id IN (1, 2, 3) ORDER BY id DESC, k", "[3 5 40 400] [2 1 30 300] [1 1 10 100] [1 2 20 200]", "", "INJOIN"},
			{"SELECT * FROM tbl WHERE id IN (1, 2, 3) ORDER BY id, k DESC", "[1 2 20 200] [1 1 10 100] [2 1 30 300] [3 5 40 400]", "", "INJOIN"},
			{"SELECT * FROM tbl WHERE a IN (30, 10, 20) ORDER BY a", "[1 1 10 100] [1 2 20 200] [2 1 30 300]", "", "INUNION"},
			{"SELECT * FROM tbl WHERE a IN (30, 10, 20) ORDER BY a DESC", "[2 1 30 300] [1 2 20 200] [1 1 10 100]", "", "INJOIN"},
			{"SELECT * FROM tbl WHERE a IN (30, 10, 20) ORDER BY a DESC, id DESC, k DESC", "[2 1 30 300] [1 2 20 200] [1 1 10 100]", "ERR(plandiff: java UnableToPlanException", ""},
			{"SELECT * FROM tbl WHERE a IN (30, 10, 20) ORDER BY a, id, k", "[1 1 10 100] [1 2 20 200] [2 1 30 300]", "ERR(plandiff: java UnableToPlanException", ""},
			{"SELECT id, col1 FROM t5 WHERE col1 IN (20, 10) ORDER BY col1", "[2 10] [5 10] [1 20] [3 20]", "", "INJOIN"},
			{"SELECT id, col1 FROM t5 WHERE col1 IN (20, 10) ORDER BY col1 DESC", "[1 20] [3 20] [2 10] [5 10]", "", "INJOIN"},
		}
		for _, a := range arms {
			javaPlan := explain(javaRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+a.sql))
			goPlan := explain(goRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+a.sql))
			javaRows := rows(javaRunner.RunWithSetup(ctx, schema, setup, a.sql))
			goRows := rows(goRunner.RunWithSetup(ctx, schema, setup, a.sql))
			fmt.Fprintf(GinkgoWriter, "%s\n  java %s -> %s\n  go   %s -> %s\n", a.sql, javaPlan, javaRows, goPlan, goRows)
			if a.javaWant != "" {
				Expect(javaRows).To(HavePrefix(a.javaWant), "%s: Java answer", a.sql)
			} else {
				Expect(javaRows).To(Equal(a.want), "%s: Java rows", a.sql)
			}
			Expect(goRows).To(Equal(a.want), "%s: Go rows", a.sql)
			Expect(goPlan).To(ContainSubstring("InJoin("), "%s: Go no longer plans an InJoin", a.sql)
			Expect(goPlan).NotTo(ContainSubstring("InMemorySort"), "%s: Go sorts", a.sql)
			if a.javaPlan != "" {
				Expect(javaPlan).To(ContainSubstring(a.javaPlan), "%s: Java's plan moved", a.sql)
			}
		}
	})
})
