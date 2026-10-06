//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
)

// wsgOutcome is an answer by SQLSTATE or by rows, so the two engines' error
// wording does not decide the comparison.
func wsgOutcome(sqlText string, r plandiff.RunResult) string {
	if r.Err != nil {
		var je *plandiff.JavaError
		if errors.As(r.Err, &je) {
			return "ERROR " + je.SQLState
		}
		var ge *api.Error
		if errors.As(r.Err, &ge) {
			return "ERROR " + string(ge.Code)
		}
		return fmt.Sprintf("ERROR %v", r.Err)
	}
	return wseRender(sqlText, r)
}

// An aggregate argument that rebuilds a whole record is simplified to the
// record on both sides of the grouping-output match (upstream #4481): the
// projection, HAVING and ORDER BY bind the one aggregate the group-by
// computes, whichever spelling of the record each uses. A bare name resolves
// to a column before a table alias.
// wsgProbe runs each probe on both engines over one schema and returns every
// answer that differs, other than the declared divergences.
func wsgProbe(schema string, setup, probes []string, divergent map[string][2]string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := SetupTenantEnvironment(ctx, sharedContainer, "wsg_"+uuid.NewString())
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = env.Cleanup(ctx) }()
	srv, err := NewIsolatedJavaInvoker()
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = srv.Close() }()
	javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
	file := writeClusterFileToTemp(env.ClusterFile)
	defer os.Remove(file)
	goRunner := plandiff.NewGoSQLSetupRunner(file)
	var failures []string
	for _, sql := range probes {
		javaLine := wsgOutcome(sql, javaRunner.RunWithSetup(ctx, schema, setup, sql))
		goLine := wsgOutcome(sql, goRunner.RunWithSetup(ctx, schema, setup, sql))
		fmt.Fprintf(GinkgoWriter, "WSG-PROBE %s\n  java=%s\n  go=%s\n", sql, javaLine, goLine)
		if want, ok := divergent[sql]; ok {
			if javaLine != want[0] || goLine != want[1] {
				failures = append(failures, fmt.Sprintf("%s: java %s, go %s; want %s and %s", sql, javaLine, goLine, want[0], want[1]))
			}
			continue
		}
		if javaLine != goLine {
			failures = append(failures, fmt.Sprintf("%s: java %s, go %s", sql, javaLine, goLine))
		}
	}
	return failures
}

// An aggregate argument that rebuilds a whole record is simplified to the
// record on both sides of the grouping-output match (upstream #4481): the
// projection, HAVING and ORDER BY bind the one aggregate the group-by
// computes, whichever spelling of the record each uses. A bare name resolves
// to a column before a table alias.
var _ = Describe("WSGGroupingOutputConformance", func() {
	It("binds aggregates over a record as the target does", func() {
		const schema = `CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) ` +
			`CREATE INDEX i1 AS SELECT col1 FROM t1 ` +
			`CREATE TABLE t3 (id BIGINT, t3 BIGINT, PRIMARY KEY (id))`
		setup := []string{
			`INSERT INTO t1 VALUES (1, 10, 1), (2, 10, 2), (3, 10, 3), (4, 10, 4), (5, 10, 5), (6, 20, 6), (7, 20, 7), ` +
				`(8, 20, 8), (9, 20, 9), (10, 20, 10), (11, 20, 11), (12, 20, 12), (13, 20, 13)`,
			`INSERT INTO t3 VALUES (1, 5), (2, NULL), (3, 7)`,
		}
		// Declared: the target has no in-memory sort, so an ORDER BY no
		// index supplies is unplannable there; Go sorts in memory.
		divergent := map[string][2]string{
			`SELECT col1 FROM t1 GROUP BY col1 ORDER BY COUNT((t1.*))`: {
				`ERROR 0AF00`, `OK [BIGINT] [NULL] [[10] [20]]`,
			},
		}
		probes := []string{
			`SELECT COUNT((t1.*)) FROM t1`,
			`SELECT COUNT((t1.id, t1.col1, t1.col2)) FROM t1`,
			`SELECT COUNT((t1.col2, t1.col1, t1.id)) FROM t1`,
			`SELECT COUNT((t1.id, t1.col2)) FROM t1`,
			`SELECT col1, COUNT((t1.*)) FROM t1 GROUP BY col1 ORDER BY col1`,
			`SELECT col1 FROM t1 GROUP BY col1 HAVING COUNT((t1.*)) > 5`,
			`SELECT col1 FROM t1 GROUP BY col1 ORDER BY COUNT((t1.*))`,
			`SELECT col1, COUNT((t1.id, t1.col1, t1.col2)) FROM t1 GROUP BY col1 HAVING COUNT((t1.*)) > 5`,
			`SELECT col1, COUNT((t1.*)) + 1 FROM t1 GROUP BY col1 ORDER BY col1`,
			`SELECT COUNT(t1) FROM t1`,
			`SELECT col1, COUNT(t1) FROM t1 GROUP BY col1 ORDER BY col1`,
			`SELECT COUNT(x) FROM t1 AS x`,
			`SELECT COUNT((x.*)) FROM t1 AS x`,
			`SELECT COUNT(t3) FROM t3`,
			`SELECT COUNT((t3.*)) FROM t3`,
			`SELECT col1, col1, COUNT((t1.*)) FROM t1 GROUP BY col1, col1`,
		}
		failures := wsgProbe(schema, setup, probes, divergent)
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})

	// ARRAY_AGG's LIMIT drops an element before NULL validation, but the
	// operand is still evaluated and every row of the group still consumed.
	It("evaluates ARRAY_AGG's operand past its cap as the target does", func() {
		const schema = `CREATE TABLE t (id BIGINT, g BIGINT, n BIGINT, PRIMARY KEY (id)) CREATE INDEX t_g ON t (g)`
		setup := []string{`INSERT INTO t VALUES (1, 1, 10), (2, 1, NULL), (3, 1, 0), (4, 2, 4), (5, 2, 2)`}
		probes := []string{
			`SELECT ARRAY_AGG(n LIMIT 1) FROM t WHERE g = 1`,
			`SELECT ARRAY_AGG(n LIMIT 0) FROM t WHERE g = 1`,
			`SELECT ARRAY_AGG(100 / n LIMIT 1) FROM t WHERE g = 1 AND n IS NOT NULL`,
			`SELECT ARRAY_AGG(100 / n LIMIT 0) FROM t WHERE g = 1 AND n IS NOT NULL`,
			`SELECT g, ARRAY_AGG(100 / n LIMIT 1), COUNT(*) FROM t WHERE n IS NOT NULL GROUP BY g`,
			`SELECT g, ARRAY_AGG(n LIMIT 1), COUNT(*), SUM(n) FROM t GROUP BY g`,
			`SELECT g, ARRAY_AGG(n IGNORE NULLS LIMIT 5), COUNT(n) FROM t GROUP BY g`,
		}
		// Declared: integral division by zero is 22012 where the target
		// reports its ArithmeticException as UNKNOWN (DIVERGENCES.md, 22012).
		divergent := map[string][2]string{
			probes[2]: {`ERROR XXXXX`, `ERROR 22012`},
			probes[3]: {`ERROR XXXXX`, `ERROR 22012`},
			probes[4]: {`ERROR XXXXX`, `ERROR 22012`},
		}
		failures := wsgProbe(schema, setup, probes, divergent)
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})
})
