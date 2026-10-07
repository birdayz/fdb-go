//go:build bazelrunfiles

package conformance_test

// Unknown-column and unknown-reference errors: SQLSTATE and message text
// across the clauses and qualifications a reference can take.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("UnknownReferenceTextJavaProbe", func() {
	It("reports an unknown column or reference with Java's code and text", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "unkref_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)
		const schema = "CREATE TABLE T (ID BIGINT, V BIGINT, PRIMARY KEY (ID)) CREATE TABLE U (ID BIGINT, W BIGINT, PRIMARY KEY (ID))"
		setup := []string{"INSERT INTO T VALUES (1, 10)", "INSERT INTO U VALUES (1, 20)"}
		render := func(result plandiff.RunResult) string {
			if result.Err == nil {
				return "OK"
			}
			var je *plandiff.JavaError
			var ge *api.Error
			if errors.As(result.Err, &je) {
				return fmt.Sprintf("%s %q", je.SQLState, je.Message)
			}
			if errors.As(result.Err, &ge) {
				return fmt.Sprintf("%s %q", ge.Code, ge.Message)
			}
			return fmt.Sprintf("ERROR %v", result.Err)
		}
		var mismatches []string
		for _, c := range []struct{ name, sql string }{
			{"select_bare", "SELECT ZZ FROM T"},
			{"select_qualified", "SELECT T.ZZ FROM T"},
			{"select_aliased", "SELECT A.ZZ FROM T AS A"},
			{"where_bare", "SELECT ID FROM T WHERE ZZ = 1"},
			{"where_qualified", "SELECT ID FROM T WHERE T.ZZ = 1"},
			{"where_aliased", "SELECT ID FROM T AS A WHERE A.ZZ = 1"},
			{"where_unknown_qualifier", "SELECT ID FROM T WHERE Q.ID = 1"},
			{"select_unknown_qualifier", "SELECT Q.ID FROM T"},
			{"qualified_star_unknown", "SELECT Q.* FROM T"},
			{"qualified_star_other_table", "SELECT U.* FROM T"},
			{"order_by_bare", "SELECT ID FROM T ORDER BY ZZ"},
			{"order_by_qualified", "SELECT ID FROM T ORDER BY T.ZZ"},
			{"group_by_bare", "SELECT ID FROM T GROUP BY ZZ"},
			{"group_by_qualified", "SELECT T.ID FROM T GROUP BY T.ZZ"},
			{"having_bare", "SELECT ID FROM T GROUP BY ID HAVING ZZ > 0"},
			{"join_on_qualified", "SELECT T.ID FROM T JOIN U ON T.ZZ = U.ID"},
			{"join_on_bare", "SELECT T.ID FROM T JOIN U ON ZZ = U.ID"},
			{"derived_qualified", "SELECT D.ZZ FROM (SELECT ID FROM T) AS D"},
			{"exists_inner_qualified", "SELECT ID FROM T WHERE EXISTS (SELECT 1 FROM U WHERE U.ZZ = T.ID)"},
			{"exists_outer_qualified", "SELECT ID FROM T WHERE EXISTS (SELECT 1 FROM U WHERE U.ID = T.ZZ)"},
			{"quoted_lower", `SELECT "zz" FROM T`},
			{"quoted_qualified_lower", `SELECT T."zz" FROM T`},
			{"update_where", "UPDATE T SET V = 1 WHERE ZZ = 1"},
			{"update_set", "UPDATE T SET ZZ = 1 WHERE ID = 1"},
			{"having_qualified", "SELECT T.ID FROM T GROUP BY T.ID HAVING T.ZZ > 0"},
			{"order_by_unknown_qualifier", "SELECT ID FROM T ORDER BY Q.ID"},
			{"using_missing", "SELECT T.ID FROM T JOIN U USING (ZZ)"},
			{"using_chain_missing", "SELECT T.ID FROM T JOIN U USING (ID) JOIN T AS T2 USING (ZZ)"},
			{"using_right_missing", "SELECT T.ID FROM T JOIN U USING (V)"},
			{"using_quoted_missing", `SELECT T.ID FROM T JOIN U USING ("zz")`},
			{"using_derived_left_missing", "SELECT D.ID FROM (SELECT ID FROM T) AS D JOIN U USING (W)"},
			{"recursive_qualified_missing", "WITH RECURSIVE R(N, UP) AS (SELECT ID, V FROM T WHERE ID = 1 UNION ALL SELECT B.ID, B.V FROM R AS A, T AS B WHERE B.ID = A.UP AND B.ID < 0) SELECT N FROM R"},
			{"delete_where", "DELETE FROM T WHERE T.ZZ = 1"},
		} {
			j := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			GinkgoWriter.Printf("UNKREF %s\n  java %s\n  go   %s\n", c.name, j, g)
			if j != g {
				mismatches = append(mismatches, fmt.Sprintf("%s: java %s, go %s", c.name, j, g))
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})
})
