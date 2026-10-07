//go:build bazelrunfiles

package conformance_test

// Qualified stars beside other items under GROUP BY, and stars over an
// enclosing query's source, compared with Java's rows.

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

var _ = Describe("StarScopeJavaProbe", func() {
	It("expands mixed and correlated stars as Java does", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "starscope_"+uuid.NewString())
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
			"CREATE INDEX A_IDX AS SELECT A1, A2, A3 FROM A ORDER BY A1, A2, A3"
		setup := []string{
			"INSERT INTO A VALUES (1, 10, 1), (2, 10, 2), (3, 20, 3)",
			"INSERT INTO B VALUES (1, 20), (2, 30), (5, 20)",
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
			{"mixed_local_star_group", "SELECT A.*, A1 FROM A GROUP BY A1, A2, A3"},
			{"mixed_local_star_group_missing_key", "SELECT A.*, A1 FROM A GROUP BY A1, A2"},
			{"mixed_two_sources_group", "SELECT A.*, B.* FROM A, B WHERE A.A1 = B.B1 GROUP BY A1"},
			{"exists_mixed_star_group_outer_col", "SELECT B1 FROM B WHERE EXISTS (SELECT A.*, B1 FROM A GROUP BY A1, A2, A3)"},
			{"exists_outer_star", "SELECT B.* FROM B WHERE EXISTS (SELECT A.*, B.* FROM A)"},
			{"exists_outer_star_correlated", "SELECT B1 FROM B WHERE EXISTS (SELECT B.* FROM A WHERE A.A1 = B.B1)"},
			{"from_outer_alias_exists", "SELECT B1 FROM B AS X WHERE EXISTS (SELECT 1 FROM X WHERE X.B2 > 25)"},
			{"from_outer_alias_correlated", "SELECT X.B1 FROM B AS X WHERE EXISTS (SELECT 1 FROM X AS Y WHERE Y.B2 = X.B2 AND Y.B1 <> X.B1)"},
			{"from_outer_alias_cte", "WITH C AS (SELECT A1 FROM A) SELECT Q.A1 FROM C AS Q WHERE EXISTS (SELECT 1 FROM Q WHERE Q.A1 > 2)"},
			{"exists_outer_star_filtered", "SELECT B1 FROM B WHERE EXISTS (SELECT B.* FROM A WHERE A.A1 = B.B1 AND B.B2 = 20)"},
			{"exists_outer_star_group", "SELECT B.* FROM B WHERE EXISTS (SELECT A.*, B.* FROM A GROUP BY A1, A2, A3)"},
			{"exists_outer_star_first_group", "SELECT B1 FROM B WHERE EXISTS (SELECT B.*, A1 FROM A GROUP BY A1)"},
			{"exists_outer_star_group_filtered", "SELECT B1 FROM B WHERE EXISTS (SELECT A1, B.* FROM A WHERE A.A1 = B.B1 GROUP BY A1)"},
			{"exists_outer_star_group_having", "SELECT B1 FROM B WHERE EXISTS (SELECT A1, B.* FROM A GROUP BY A1 HAVING A1 > B.B1)"},
		} {
			j := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			GinkgoWriter.Printf("STARSCOPE %s\n  java %s\n  go   %s\n", c.name, j, g)
			if j != g {
				mismatches = append(mismatches, fmt.Sprintf("%s: java %s, go %s", c.name, j, g))
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})
})
