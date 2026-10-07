//go:build bazelrunfiles

package conformance_test

// A correlated EXISTS whose FROM re-declares an outer FROM source by name (an
// inner ST under an outer ST): Java resolves the inner reference to the inner
// source (the inner shadows the outer). These are the shapes Go declined as
// scope-ambiguous until each subquery leg got its own binding
// (`sqldriver/exists_scope_shadow_fdb_test.go` holds the Go-only twins).

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
			"INSERT INTO OT VALUES (1000, 50)",
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
		// Rows Go declines for a reason of its own, not the shadow: an
		// outer-only conjunct under NOT EXISTS beside a nested EXISTS (the
		// anti-join arm, as for the non-colliding twin). Java answers [11]
		// [12].
		goDeclines := map[string]string{
			"case1_notexists_colliding_foldable": "requires positive predicate consumption",
		}
		var mismatches []string
		for _, c := range []struct{ name, sql string }{
			{"multisource_colliding", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K")`},
			{"multisource_colliding_nested_bypass", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K" AND EXISTS (SELECT 1 FROM MA WHERE MA."C" > 0))`},
			{"multisource_colliding_notexists", `SELECT OT."K" FROM ST, OT WHERE NOT EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE ST."C" < OT."K")`},
			{"multisource_colliding_first_leg", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM ST, OT AS "OI" WHERE ST."C" < OT."K")`},
			{"case1_notexists_colliding_foldable", `SELECT MA."ID" FROM MA, OT WHERE NOT EXISTS (SELECT 1 FROM ST, MA WHERE COALESCE(1, MA."C") = 1 AND OT."K" < 0 AND EXISTS (SELECT 1 FROM OT AS "OX" WHERE OX."K" > 0))`},
			{"int_head_colliding", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(1, ST."C") = 1 AND OI."K" = OT."K")`},
			{"nonfoldable_colliding", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(ST."C", 1) < OT."K")`},
		} {
			j := outcome(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := outcome(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			fmt.Fprintf(GinkgoWriter, "EXSHADOW %s\n  java %s\n  go   %s\n", c.name, j, g)
			if want, declined := goDeclines[c.name]; declined {
				Expect(g).To(ContainSubstring(want), c.name)
				continue
			}
			if g != j {
				mismatches = append(mismatches, c.name)
			}
		}
		Expect(mismatches).To(BeEmpty())
	})
})
