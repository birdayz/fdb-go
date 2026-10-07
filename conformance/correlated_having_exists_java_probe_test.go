//go:build bazelrunfiles

package conformance_test

// An EXISTS over a grouped body whose HAVING reads the enclosing row
// (`EXISTS (SELECT … GROUP BY … HAVING … outer.c …)`), compared with Java's
// rows.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CorrelatedHavingExistsJavaProbe", func() {
	It("answers a correlated HAVING inside EXISTS as Java does", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "corrhaving_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)
		const schema = "CREATE TABLE A (A1 BIGINT, A2 BIGINT, A3 BIGINT, PRIMARY KEY (A1)) " +
			"CREATE TABLE B (B1 BIGINT, B2 BIGINT, PRIMARY KEY (B1)) " +
			"CREATE INDEX A_IDX AS SELECT A2, A1 FROM A ORDER BY A2, A1"
		setup := []string{
			"INSERT INTO A VALUES (1, 10, 1), (2, 10, 2), (3, 20, 3), (4, 20, 4), (5, 20, 5), (6, 30, 6)",
			"INSERT INTO B VALUES (1, 10), (2, 20), (3, 30), (4, 40)",
		}
		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				var je *plandiff.JavaError
				var ge *api.Error
				if errors.As(r.Err, &je) {
					return "ERR " + je.SQLState
				}
				if errors.As(r.Err, &ge) {
					return "ERR " + string(ge.Code)
				}
				return "ERR " + r.Err.Error()
			}
			rows := make([]string, 0, len(r.Rows.Rows))
			for _, row := range r.Rows.Rows {
				rows = append(rows, fmt.Sprint(row))
			}
			sort.Strings(rows)
			return strings.Join(rows, " ")
		}
		var mismatches []string
		for _, c := range []struct{ name, sql string }{
			{"having_key_vs_outer", "SELECT B1 FROM B WHERE EXISTS (SELECT A2 FROM A GROUP BY A2 HAVING A2 > B.B2)"},
			{"not_exists_having", "SELECT B1 FROM B WHERE NOT EXISTS (SELECT A2 FROM A GROUP BY A2 HAVING A2 = B.B2)"},
			{"having_count_vs_outer", "SELECT B1 FROM B WHERE EXISTS (SELECT A2 FROM A GROUP BY A2 HAVING COUNT(*) > B.B1)"},
			{"having_and_where_correlated", "SELECT B1 FROM B WHERE EXISTS (SELECT A2 FROM A WHERE A.A1 >= B.B1 GROUP BY A2 HAVING MAX(A1) > B.B2 / 10)"},
			{"having_uncorrelated_where_correlated", "SELECT B1 FROM B WHERE EXISTS (SELECT A2 FROM A WHERE A.A2 = B.B2 GROUP BY A2 HAVING COUNT(*) > 1)"},
			{"having_ungrouped", "SELECT B1 FROM B WHERE EXISTS (SELECT COUNT(*) FROM A WHERE A.A2 = B.B2 HAVING COUNT(*) > 1)"},
		} {
			j := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			GinkgoWriter.Printf("CORRHAVING %s\n  java %s\n  go   %s\n", c.name, j, g)
			if j != g {
				mismatches = append(mismatches, fmt.Sprintf("%s: java %s, go %s", c.name, j, g))
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})
})
