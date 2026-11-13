//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Java probes both emp legs on dept's did whatever the cross-leg predicate; Go
// reads the innermost leg whole once a predicate ties it to the outermost (TODO.md).
var _ = Describe("ThreeWayJoinSargJavaProbe", func() {
	It("reports each engine's innermost emp leg for join_optimization_probes' three-way joins", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "threewaysarg_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)

		const schema = "CREATE TABLE dept (did BIGINT, dname STRING, PRIMARY KEY (did)) " +
			"CREATE TABLE emp (eid BIGINT, did BIGINT, ename STRING, salary BIGINT, PRIMARY KEY (eid)) " +
			"CREATE INDEX idx_emp_did ON emp (did)"
		setup := []string{
			"INSERT INTO dept VALUES (1, 'eng'), (2, 'sales'), (3, 'hr')",
			"INSERT INTO emp VALUES (10, 1, 'Alice', 100), (20, 1, 'Bob', 90), (30, 2, 'Charlie', 80), (40, 2, 'Diana', 110), (50, 3, 'Eve', 95)",
		}
		explain := func(r plandiff.RunResult) string {
			if r.Err != nil || len(r.Rows.Rows) == 0 {
				return fmt.Sprintf("ERR(%v)", r.Err)
			}
			return fmt.Sprint(r.Rows.Rows[0][0])
		}
		const from = "SELECT a.ename, b.ename FROM emp AS a, emp AS b, dept AS d WHERE a.did = d.did AND b.did = d.did"
		for _, c := range []struct {
			name, sql, goInner string
		}{
			{"no_cross_leg", from, "inner=IndexScan(IDX_EMP_DID, [=])"},
			{"b_d_arithmetic", from + " AND b.salary > d.did * 5", "inner=IndexScan(IDX_EMP_DID, [=])"},
			{"a_b_arithmetic", from + " AND a.salary - b.salary > d.did * 5", "inner=PredicatesFilter(IndexScan(IDX_EMP_DID, [*]), [2 preds])"},
			{"a_b_comparison", from + " AND a.eid < b.eid", "inner=PredicatesFilter(IndexScan(IDX_EMP_DID, [*]), [2 preds])"},
		} {
			j := explain(javaRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+c.sql))
			g := explain(goRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+c.sql))
			GinkgoWriter.Printf("THREEWAY %s\n  java %s\n  go   %s\n", c.name, j, g)
			Expect(j).To(HavePrefix("SCAN([IS DEPT]) | FLATMAP q0 -> "), "java %s: %s", c.name, j)
			Expect(strings.Count(j, "IDX_EMP_DID [EQUALS q0.DID]")).To(Equal(2), "java %s: %s", c.name, j)
			Expect(g).To(HavePrefix("FlatMap(outer=IndexScan(IDX_EMP_DID, [*]), inner=FlatMap(outer="), "go %s: %s", c.name, g)
			Expect(g).To(HaveSuffix(c.goInner+"))"), "go %s: %s", c.name, g)
		}
	})
})
