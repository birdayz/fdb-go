//go:build bazelrunfiles

package conformance_test

// A JOIN on a comma-separated FROM source (`FROM a, b JOIN c ON …`),
// compared with Java's rows.

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

var _ = Describe("CommaJoinJavaProbe", func() {
	It("joins a comma-separated source as Java does", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "commajoin_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)
		const schema = "CREATE TABLE T1 (ID BIGINT, NAME STRING, T2_REFS BIGINT ARRAY, PRIMARY KEY (ID)) " +
			"CREATE TABLE T2 (ID BIGINT, NAME STRING, PRIMARY KEY (ID))"
		setup := []string{
			"INSERT INTO T2 VALUES (1000, 'B1'), (2000, 'B2'), (3000, 'B3')",
			"INSERT INTO T1 VALUES (1, 'A1', [1000, 2000]), (2, 'A2', [2000]), (3, 'A3', [3000]), (4, 'A4', [1000])",
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
			{"unnest_then_join", "SELECT T1.NAME, T2.NAME FROM T1, T1.T2_REFS AS REF JOIN T2 ON REF = T2.ID WHERE T2.ID IN (1000, 2000)"},
			{"table_then_join", "SELECT A.NAME, B.NAME FROM T1 AS A, T2 AS C JOIN T2 AS B ON B.ID = C.ID WHERE A.ID < 3 AND C.ID = 1000"},
			{"table_then_left_join_reads_first", "SELECT A.ID, C.ID, B.ID FROM T1 AS A, T2 AS C LEFT JOIN T2 AS B ON B.ID = C.ID AND A.ID = 1"},
			{"unnest_then_left_join", "SELECT T1.ID, REF, T2.NAME FROM T1, T1.T2_REFS AS REF LEFT JOIN T2 ON REF = T2.ID AND T2.ID > 1000"},
			{"two_joins_on_comma", "SELECT A.ID, B.ID, D.ID FROM T1 AS A, T2 AS B JOIN T2 AS C ON C.ID = B.ID JOIN T1 AS D ON D.ID = A.ID"},
		} {
			j := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			GinkgoWriter.Printf("COMMAJOIN %s\n  java %s\n  go   %s\n", c.name, j, g)
			// A LEFT JOIN whose left side holds a lateral unnest is a Go
			// unnest-lowering limit (DIVERGENCES.md "A LEFT JOIN over a
			// lateral unnest"); the row reddens when it changes.
			if c.name == "unnest_then_left_join" {
				if g != "ERR 0AF00" || j == g {
					mismatches = append(mismatches, fmt.Sprintf("%s: java %s, go %s", c.name, j, g))
				}
				continue
			}
			if j != g {
				mismatches = append(mismatches, fmt.Sprintf("%s: java %s, go %s", c.name, j, g))
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})
})
