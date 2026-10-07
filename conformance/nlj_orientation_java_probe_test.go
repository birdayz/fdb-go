//go:build bazelrunfiles

package conformance_test

// Which side drives a join over a derived aggregate, in each engine, under
// both orders of two WHERE conjuncts? Where Java can plan the query (an
// index orders the GROUP BY, no ORDER BY over the join) both engines drive
// from the aggregate and probe the index per group, under either order. The
// shape whose orientation followed the conjunct order in Go, a materialized
// nested-loop join over an unordered aggregate with an in-memory sort, is
// one Java cannot plan (UnableToPlanException), so there is no Java
// orientation to follow there.

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

var _ = Describe("NLJOrientationJavaProbe", func() {
	It("reports each engine's nested-loop orientation under both conjunct orders", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("nljorient_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		const schema = "CREATE TABLE orders (id BIGINT, customer_id BIGINT, status STRING, PRIMARY KEY (id)) " +
			"CREATE INDEX oc AS SELECT customer_id FROM orders ORDER BY customer_id"
		setup := []string{
			"INSERT INTO orders VALUES (1, 10, 'pending'), (2, 10, 'shipped'), (3, 20, 'shipped'), (4, 20, 'pending')",
		}
		explain := func(r plandiff.RunResult) string {
			if r.Err != nil || len(r.Rows.Rows) == 0 {
				return fmt.Sprintf("ERR(%v)", r.Err)
			}
			return fmt.Sprint(r.Rows.Rows[0][0])
		}
		const derived = "(SELECT customer_id, COUNT(*) AS shipped_count FROM orders WHERE status = 'shipped' GROUP BY customer_id) AS s"
		for _, where := range []string{
			"o.customer_id = s.customer_id AND o.status = 'pending'",
			"o.status = 'pending' AND o.customer_id = s.customer_id",
		} {
			sql := "SELECT o.id, s.shipped_count FROM orders o, " + derived + " WHERE " + where
			j := explain(javaRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+sql))
			g := explain(goRunner.RunWithSetup(ctx, schema, setup, "EXPLAIN "+sql))
			fmt.Fprintf(GinkgoWriter, "NLJ-ORIENT %s\n  java %s\n  go   %s\n", where, j, g)
			// Both engines drive the join from the aggregate and probe the
			// index per group, whatever the conjunct order.
			Expect(j).To(ContainSubstring("AGG (count_star(*) AS _0) GROUP BY"), "java: %s", j)
			Expect(strings.Index(j, "AGG")).To(BeNumerically("<", strings.Index(j, "FLATMAP")), "java: %s", j)
			Expect(g).To(HavePrefix("FlatMap(outer=StreamingAgg("), "go: %s", g)
			Expect(g).To(ContainSubstring("inner=PredicatesFilter(IndexScan(OC, [=])"), "go: %s", g)
		}
	})
})
