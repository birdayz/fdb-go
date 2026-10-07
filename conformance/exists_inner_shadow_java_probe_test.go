//go:build bazelrunfiles

package conformance_test

// Correlated EXISTS shapes Go used to decline. An inner FROM source that
// re-declares an outer one by name (an inner ST under an outer ST): Java
// resolves the reference to the inner source. A middle subquery with an
// outer-only conjunct beside a nested EXISTS, under NOT EXISTS or projected:
// the conjunct stays inside the existential. OT holds a row for which the
// outer-only conjuncts hold and one for which they do not.
// (`sqldriver/exists_scope_shadow_fdb_test.go` holds the Go-only twins.)

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ExistsInnerShadowJavaProbe", func() {
	It("answers an inner source that shadows an outer one as Java does", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("exshadow_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		const schema = "CREATE TABLE ST (ID BIGINT, C BIGINT, ARR BIGINT ARRAY, PRIMARY KEY (ID)) " +
			"CREATE TABLE MA (ID BIGINT, C BIGINT, ARR BIGINT ARRAY, PRIMARY KEY (ID)) " +
			"CREATE TABLE OT (ID BIGINT, K BIGINT, PRIMARY KEY (ID))"
		setup := []string{
			"INSERT INTO ST VALUES (1, 100, [10, 200]), (2, 5, [20, 300]), (3, 1000, [4])",
			"INSERT INTO MA VALUES (11, 11, [10, 11, 12]), (12, 5, [20, 21])",
			"INSERT INTO OT VALUES (1000, 50), (2000, -5)",
		}
		outcome := func(r plandiff.RunResult) string {
			if r.Err != nil {
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
			{"multisource_colliding", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K")`},
			{"multisource_colliding_nested_bypass", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K" AND EXISTS (SELECT 1 FROM MA WHERE MA."C" > 0))`},
			{"multisource_colliding_notexists", `SELECT OT."K" FROM ST, OT WHERE NOT EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K")`},
			{"multisource_colliding_first_leg", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM ST, OT AS "OI" WHERE ST."C" < OT."K")`},
			{"case1_notexists_colliding_foldable", `SELECT MA."ID" FROM MA, OT WHERE NOT EXISTS (SELECT 1 FROM ST, MA WHERE COALESCE(1, MA."C") = 1 AND OT."K" < 0 AND EXISTS (SELECT 1 FROM OT AS "OX" WHERE OX."K" > 0))`},
			{"case1_notexists_noncolliding", `SELECT MA."ID" FROM MA, OT WHERE NOT EXISTS (SELECT 1 FROM ST, MA AS "M2" WHERE OT."K" < 0 AND EXISTS (SELECT 1 FROM OT AS "OX" WHERE OX."K" > 0))`},
			{"case1_notexists_reffree", `SELECT MA."ID" FROM MA, OT WHERE NOT EXISTS (SELECT 1 FROM ST, MA AS "M2" WHERE "M2"."C" < OT."K" AND 1 = 0 AND EXISTS (SELECT 1 FROM OT AS "OX" WHERE OX."K" > 0))`},
			{"case1_notexists_mixed", `SELECT MA."ID" FROM MA, OT WHERE NOT EXISTS (SELECT 1 FROM ST, MA AS "M2" WHERE "M2"."C" > 0 AND OT."K" < 0 AND EXISTS (SELECT 1 FROM OT AS "OX" WHERE OX."K" > 0))`},
			{"case2_hoist_notexists", `SELECT MA."ID" FROM MA, OT WHERE NOT EXISTS (SELECT 1 FROM ST, MA AS "M2" WHERE EXISTS (SELECT 1 FROM OT AS "OX", ST AS "S2" WHERE OT."K" < 0 AND EXISTS (SELECT 1 FROM MA AS "M3" WHERE "M3"."C" > 0)))`},
			{"projected_notexists", `SELECT MA."ID", NOT EXISTS (SELECT 1 FROM ST, MA AS "M2" WHERE OT."K" < 0 AND EXISTS (SELECT 1 FROM OT AS "OX" WHERE OX."K" > 0)) FROM MA, OT`},
			{"projected_exists", `SELECT MA."ID", EXISTS (SELECT 1 FROM ST, MA AS "M2" WHERE OT."K" < 0 AND EXISTS (SELECT 1 FROM OT AS "OX" WHERE OX."K" > 0)) AS "E" FROM MA, OT`},
			{"int_head_colliding", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(1, ST."C") = 1 AND OI."K" = OT."K")`},
			{"nonfoldable_colliding", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(ST."C", 1) < OT."K")`},
		} {
			j := outcome(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := outcome(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			fmt.Fprintf(GinkgoWriter, "EXSHADOW %s\n  java %s\n  go   %s\n", c.name, j, g)
			if g != j {
				mismatches = append(mismatches, c.name)
			}
		}
		Expect(mismatches).To(BeEmpty())
	})
})
