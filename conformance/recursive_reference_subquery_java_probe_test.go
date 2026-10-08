//go:build bazelrunfiles

package conformance_test

// A recursive CTE's self-reference read inside a subquery of its own
// recursive leg (`NOT EXISTS (SELECT … FROM c …)` where c is the leg's alias
// of the reference), compared with Java's rows.

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

var _ = Describe("RecursiveReferenceInSubqueryJavaProbe", func() {
	It("reads a recursive self-reference in a subquery as Java does", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "recsubq_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)
		const schema = "CREATE TABLE E (ID BIGINT, NAME STRING, M BIGINT, PRIMARY KEY (ID)) " +
			"CREATE TABLE G (A BIGINT, B BIGINT, PRIMARY KEY (A, B))"
		setup := []string{
			"INSERT INTO E VALUES (1, 'Alice', NULL), (2, 'Bob', 1), (3, 'Carol', 1), (4, 'David', 2), (5, 'Eve', 2), (6, 'Frank', 3), (7, 'Grace', 3)",
			"INSERT INTO G VALUES (1, 2), (1, 3), (2, 4), (3, 4), (4, 5), (2, 5)",
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
			{"not_exists_same_name", "WITH RECURSIVE r AS (SELECT ID, NAME, M FROM E WHERE ID = 2 UNION ALL " +
				"SELECT e.ID, e.NAME, e.M FROM r AS c, E AS e WHERE c.ID = e.M AND NOT EXISTS (SELECT NAME FROM c WHERE NAME = e.NAME)) " +
				"TRAVERSAL ORDER level_order SELECT NAME FROM r"},
			{"exists_previous_level", "WITH RECURSIVE r AS (SELECT ID, NAME, M FROM E WHERE ID = 1 UNION ALL " +
				"SELECT e.ID, e.NAME, e.M FROM r AS c, E AS e WHERE c.ID = e.M AND EXISTS (SELECT ID FROM c WHERE ID = 1)) " +
				"TRAVERSAL ORDER level_order SELECT NAME FROM r"},
			{"not_exists_diamond", "WITH RECURSIVE p AS (SELECT A, B FROM G WHERE A = 1 UNION ALL " +
				"SELECT g.A, g.B FROM p AS c, G AS g WHERE c.B = g.A AND NOT EXISTS (SELECT A FROM c WHERE B = g.B)) " +
				"TRAVERSAL ORDER level_order SELECT A, B FROM p"},
		} {
			j := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			GinkgoWriter.Printf("RECSUBQ %s\n  java %s\n  go   %s\n", c.name, j, g)
			if j != g {
				mismatches = append(mismatches, fmt.Sprintf("%s: java %s, go %s", c.name, j, g))
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})
})
