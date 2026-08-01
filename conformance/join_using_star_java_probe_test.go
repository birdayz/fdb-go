//go:build bazelrunfiles

package conformance_test

// Measures Java's live behaviour (tag 4.12.11.0 conformance server) for
// JOIN … USING star expansion and asserts Go matches it.
//
// Java's USING resolution (QueryVisitor.resolveJoinUsingClause,
// QueryVisitor.java:397-420) marks the RIGHT-side copy of each USING column
// hidden (Expression.asHidden); star expansion filters hidden expressions
// (SemanticAnalyzer.expandStar → nonEphemeralVisible, for both bare `*` and
// `alias.*`), and UNQUALIFIED identifier resolution skips hidden attributes
// (SemanticAnalyzer.java:468) so a bare reference to the USING column
// resolves the LEFT copy instead of being ambiguous — while a QUALIFIED
// reference to the right copy still works.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("JoinUsingStarJavaProbe", func() {
	It("Go matches Java's live outcome for JOIN USING star shapes", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("usingstar_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		schema := "CREATE TABLE JA (c1 BIGINT, a2 STRING, PRIMARY KEY (c1))" +
			" CREATE TABLE JB (c1 BIGINT, b2 STRING, PRIMARY KEY (c1))" +
			" CREATE TABLE JD (c1 BIGINT, d2 STRING, PRIMARY KEY (c1))"
		setup := []string{
			"INSERT INTO JA VALUES (1, 'a1'), (2, 'a2')",
			"INSERT INTO JB VALUES (1, 'b1'), (3, 'b3')",
			"INSERT INTO JD VALUES (1, 'd1'), (2, 'd2')",
		}

		probes := []struct{ name, sql string }{
			{"star_single_using", "SELECT * FROM JA JOIN JB USING (c1)"},
			{"star_chained_using", "SELECT * FROM JA JOIN JB USING (c1) JOIN JD USING (c1)"},
			{"qualified_star_right", "SELECT JB.* FROM JA JOIN JB USING (c1)"},
			{"qualified_star_left", "SELECT JA.* FROM JA JOIN JB USING (c1)"},
			{"bare_using_col", "SELECT c1 FROM JA JOIN JB USING (c1)"},
			{"qualified_right_col", "SELECT JB.c1 FROM JA JOIN JB USING (c1)"},
			{"order_by_bare_using", "SELECT b2 FROM JA JOIN JB USING (c1) ORDER BY c1"},
			{"left_join_using_star", "SELECT * FROM JA LEFT JOIN JB USING (c1)"},
			{"star_on_join_control", "SELECT * FROM JA JOIN JB ON JA.c1 = JB.c1"},
		}

		render := func(engine string, r plandiff.RunResult) string {
			if r.Err != nil {
				var je *plandiff.JavaError
				if errors.As(r.Err, &je) {
					return fmt.Sprintf("%s ERROR sqlstate=%q msg=%q", engine, je.SQLState, je.Message)
				}
				var ge *api.Error
				if errors.As(r.Err, &ge) {
					return fmt.Sprintf("%s ERROR sqlstate=%q msg=%q", engine, string(ge.Code), ge.Message)
				}
				return fmt.Sprintf("%s ERROR %v", engine, r.Err)
			}
			cols := make([]string, 0, len(r.Rows.Columns))
			for _, c := range r.Rows.Columns {
				cols = append(cols, c.Name)
			}
			return fmt.Sprintf("%s OK cols=%v rows=%v", engine, cols, r.Rows.Rows)
		}
		var divergences []string
		for _, p := range probes {
			jr := runner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			fmt.Fprintf(GinkgoWriter, "PROBE %s\n  %s\n  %s\n  sql: %s\n",
				p.name, render("JAVA", jr), render("GO  ", gr), p.sql)
			if render("X", jr) != render("X", gr) {
				divergences = append(divergences, fmt.Sprintf(
					"probe %s diverged\n  java: %s\n  go:   %s\n  sql: %s",
					p.name, render("JAVA", jr), render("GO  ", gr), p.sql))
			}
		}
		Expect(divergences).To(BeEmpty())
	})
})
