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
			"INSERT INTO OT VALUES (1000, 50), (2000, -5), (3000, 5)",
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
			{"unnest_frame_multisource", `SELECT X FROM ST, ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM ST, MA WHERE ST."C" < X)`},
			{"unnest_frame_multisource_outer_ref", `SELECT X FROM ST, ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM ST AS "S2", MA WHERE MA."C" < X AND ST."C" > 10)`},
			{"unnest_frame_same_name_inner", `SELECT X FROM ST, ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM MA, ST WHERE ST."ID" = 2 AND MA."C" < X)`},
			{"on_before_later_same_name", `SELECT "O"."ID" FROM ST AS "O" WHERE EXISTS (SELECT 1 FROM ST AS "A" JOIN MA AS "B" ON "B"."C" = "O"."C" JOIN ST AS "O" ON "O"."ID" = "A"."ID")`},
			{"unnest_reuse_inner_only", `SELECT X FROM ST, ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM ST, MA WHERE ST."C" = MA."C")`},
			{"unnest_reuse_leftbox", `SELECT X FROM ST LEFT JOIN OT ON ST."ID" = OT."ID", ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM ST, MA WHERE ST."C" = MA."C")`},
			{"unnest_reuse_second_leg", `SELECT X FROM ST, OT, ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM OT, MA WHERE OT."K" = MA."C" + 45 AND MA."C" = ST."C")`},
			{"unnest_reuse_buried", `SELECT X FROM ST, ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM ST, MA WHERE ST."C" = MA."C" AND ST."C" > 3)`},
			{"unnest_reuse_corr_other_leg", `SELECT X FROM ST, OT, ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM ST, MA WHERE ST."C" = MA."C" AND OT."K" > 0)`},
			{"unnest_reuse_corr_elem", `SELECT X FROM ST, OT, ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM OT, MA WHERE OT."K" > MA."C" AND MA."C" < X)`},
			{"unnest_reuse_leftbox_corr", `SELECT X FROM ST LEFT JOIN OT ON ST."ID" = OT."ID", ST."ARR" AS X WHERE EXISTS (SELECT 1 FROM ST, MA WHERE ST."C" = MA."C" AND MA."C" < X)`},
			{"unnest_reuse_leftbox_oth", `SELECT X FROM MA LEFT JOIN OT ON MA."ID" = OT."ID", MA."ARR" AS X WHERE EXISTS (SELECT 1 FROM MA, ST WHERE ST."C" = MA."C" AND ST."ID" < X)`},
			{"unnest_reuse_notexists", `SELECT X FROM ST, OT, ST."ARR" AS X WHERE NOT EXISTS (SELECT 1 FROM OT, MA WHERE OT."K" = MA."C" + 45 AND MA."C" = ST."C")`},
			{"outer_join_on_corr_left", `SELECT "O"."ID" FROM ST AS "O" WHERE EXISTS (SELECT 1 FROM MA AS "A" LEFT JOIN ST AS "B" ON "B"."C" = "O"."C" WHERE "A"."C" > 0)`},
			{"outer_join_on_corr_left_null", `SELECT "O"."ID" FROM ST AS "O" WHERE EXISTS (SELECT 1 FROM MA AS "A" LEFT JOIN ST AS "B" ON "B"."C" = "O"."C" AND "B"."ID" = "A"."ID" - 10 WHERE "B"."ID" IS NULL)`},
			{"outer_join_on_corr_projected", `SELECT "O"."ID", EXISTS (SELECT 1 FROM MA AS "A" LEFT JOIN ST AS "B" ON "B"."C" = "O"."C" AND "B"."ID" = "A"."ID" - 10 WHERE "B"."ID" IS NULL) FROM ST AS "O"`},
			// Lifting the ON's correlation to a filter, or dropping it, answers
			// differently from keeping it in the null-extending ON.
			{"outer_join_on_corr_discriminating", `SELECT "O"."ID", EXISTS (SELECT 1 FROM MA AS "A" LEFT JOIN ST AS "B" ON "B"."C" = "O"."C" AND "B"."ID" = "A"."ID" - 10 WHERE "B"."ID" IS NULL AND "A"."ID" = 11) FROM ST AS "O"`},
			{"outer_join_on_corr_where_exists", `SELECT "O"."ID" FROM ST AS "O" WHERE EXISTS (SELECT 1 FROM MA AS "A" LEFT JOIN ST AS "B" ON "B"."C" = "O"."C" AND "B"."ID" = "A"."ID" - 10 WHERE "B"."ID" IS NULL AND "A"."ID" = 11)`},
			{"outer_join_on_corr_not_exists", `SELECT "O"."ID" FROM ST AS "O" WHERE NOT EXISTS (SELECT 1 FROM MA AS "A" LEFT JOIN ST AS "B" ON "B"."C" = "O"."C" AND "B"."ID" = "A"."ID" - 10 WHERE "B"."ID" IS NULL AND "A"."ID" = 11)`},
			{"corr_on_before_right_discriminating", `SELECT OT."ID" FROM OT WHERE EXISTS (SELECT 1 FROM MA AS "A" JOIN ST AS "B" ON "B"."C" = OT."K" RIGHT JOIN MA AS "M" ON "M"."ID" = "A"."ID" WHERE "A"."ID" IS NULL)`},
			{"right_join_on_corr", `SELECT OT."ID" FROM OT WHERE EXISTS (SELECT 1 FROM ST AS "B" RIGHT JOIN MA AS "M" ON "B"."C" = OT."K" AND "M"."C" = OT."K" WHERE "B"."ID" IS NULL)`},
			// An enclosing name re-bound inside a nested EXISTS below the outer
			// join: the ON's O."C" stays on the LEFT JOIN.
			{"outer_join_on_corr_nested_shadow", `SELECT "O"."ID" FROM ST AS "O" WHERE EXISTS (SELECT 1 FROM MA AS "A" LEFT JOIN ST AS "B" ON "B"."ID" = "A"."ID" AND "B"."C" = "O"."C" WHERE EXISTS (SELECT 1 FROM ST AS "O" WHERE "O"."ID" = "A"."ID" - 10))`},
			{"outer_join_on_corr_nested_control", `SELECT "O"."ID" FROM ST AS "O" WHERE EXISTS (SELECT 1 FROM MA AS "A" LEFT JOIN ST AS "B" ON "B"."ID" = "A"."ID" AND "B"."C" = "O"."C" WHERE "B"."ID" IS NULL AND EXISTS (SELECT 1 FROM MA AS "M2" WHERE "M2"."C" = "O"."C"))`},
			{"corr_on_before_right", `SELECT "O"."ID" FROM ST AS "O" WHERE EXISTS (SELECT 1 FROM MA AS "A" JOIN ST AS "B" ON "B"."C" = "O"."C" RIGHT JOIN MA AS "M" ON "M"."ID" = "A"."ID")`},
			{"on_nested_exists", `SELECT "O"."ID" FROM ST AS "O" WHERE EXISTS (SELECT 1 FROM MA AS "A" JOIN ST AS "B" ON EXISTS (SELECT 1 FROM MA AS "G" WHERE "G"."C" = "O"."C"))`},
			{"int_head_colliding", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(1, ST."C") = 1 AND OI."K" = OT."K")`},
			{"nonfoldable_colliding", `SELECT OT."K" FROM ST, OT WHERE EXISTS (SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(ST."C", 1) < OT."K")`},
		} {
			j := outcome(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := outcome(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			fmt.Fprintf(GinkgoWriter, "EXSHADOW %s\n  java %s\n  go   %s\n", c.name, j, g)
			// Go still refuses a nested EXISTS inside a correlated EXISTS's
			// JOIN ON (DIVERGENCES.md "A nested EXISTS inside a JOIN ON of a
			// correlated EXISTS"); the row reddens when that changes.
			if c.name == "on_nested_exists" {
				if j != "[2]" || !strings.Contains(g, "a nested subquery inside a JOIN ON clause is not supported") {
					mismatches = append(mismatches, c.name)
				}
				continue
			}
			if g != j {
				mismatches = append(mismatches, c.name)
			}
		}
		Expect(mismatches).To(BeEmpty())
	})
})
