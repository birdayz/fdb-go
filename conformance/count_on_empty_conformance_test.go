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

// Java's QueryVisitor rewrites an ungrouped query's COUNT to COALESCE(count, 0)
// (LogicalOperator.adjustCountOnEmpty, SQL 4.16.4) in the select list and the
// HAVING clause; a grouped COUNT keeps the raw, nullable CountValue.
var _ = Describe("CountOnEmptyConformance", func() {
	It("answers COUNT's rows and nullability as the target does", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "countempty_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		file := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(file)
		goRunner := plandiff.NewGoSQLSetupRunner(file)
		const schema = `CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id)) CREATE TABLE e (id BIGINT, g BIGINT, PRIMARY KEY (id))`
		setup := []string{`INSERT INTO t VALUES (1, 1), (2, 1), (3, 2)`}
		// Declared divergences: Java returns one row per input row for HAVING over a
		// select list without an aggregate (SQL makes the table one group:
		// DIVERGENCES.md, "HAVING without GROUP BY"); Go plans a grouped query
		// without an index (a read extension).
		divergent := map[string][2]string{
			`SELECT 7 FROM e HAVING COUNT(*) = 0`: {`OK [INTEGER] [NOT NULL] []`, `OK [INTEGER] [NOT NULL] [[7]]`},
			`SELECT 7 FROM t HAVING COUNT(*) > 0`: {`OK [INTEGER] [NOT NULL] [[7] [7] [7]]`, `OK [INTEGER] [NOT NULL] [[7]]`},
			`SELECT 7 AS x, 'a' FROM t HAVING COUNT(*) = 3`: {
				`OK [INTEGER STRING] [NOT NULL NOT NULL] [[7 a] [7 a] [7 a]]`,
				`OK [INTEGER STRING] [NOT NULL NOT NULL] [[7 a]]`,
			},
			`SELECT g, COUNT(*) FROM t GROUP BY g`: {
				`ERROR 0AF00 UnableToPlanException "Cascades planner could not plan query"`,
				`OK [BIGINT BIGINT] [NULL NULL] [[1 2] [2 1]]`,
			},
		}
		var failures []string
		for _, sql := range []string{
			`SELECT COUNT(*) FROM t`,
			`SELECT COUNT(*) FROM e`,
			`SELECT COUNT(g) FROM e`,
			`SELECT COUNT(*) + 1 FROM e`,
			`SELECT COUNT(*), MAX(g) FROM e`,
			`SELECT 7 FROM e HAVING COUNT(*) = 0`,
			`SELECT 7 FROM t HAVING COUNT(*) > 0`,
			`SELECT 7 AS x, 'a' FROM t HAVING COUNT(*) = 3`,
			`SELECT COUNT(*) FROM e HAVING COUNT(*) = 0`,
			`SELECT COUNT(*) FROM t HAVING COUNT(*) > 0`,
			`SELECT g, COUNT(*) FROM t GROUP BY g`,
		} {
			render := func(r plandiff.RunResult) string { return wseRender(sql, r) }
			javaLine, goLine := render(javaRunner.RunWithSetup(ctx, schema, setup, sql)), render(goRunner.RunWithSetup(ctx, schema, setup, sql))
			fmt.Fprintf(GinkgoWriter, "COUNT-ON-EMPTY %s\n  java=%s\n  go=%s\n", sql, javaLine, goLine)
			if want, ok := divergent[sql]; ok {
				if javaLine != want[0] || goLine != want[1] {
					failures = append(failures, fmt.Sprintf("%s: java %s, go %s; want %s and %s", sql, javaLine, goLine, want[0], want[1]))
				}
				continue
			}
			if javaLine != goLine || !strings.HasPrefix(javaLine, "OK ") {
				failures = append(failures, fmt.Sprintf("%s: java %s, go %s", sql, javaLine, goLine))
			}
		}
		Expect(failures).To(BeEmpty())
	})
})
