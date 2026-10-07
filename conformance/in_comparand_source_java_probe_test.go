//go:build bazelrunfiles

package conformance_test

// IN lists over an index under ORDER BY (the in-union and in-join): literal,
// duplicated, computed and faulting items, compared with Java's rows. A list
// item that fails to evaluate (`1 / 0`) raises when the plan opens, even over
// an empty table, as Java's arrayDistinct(comparand) explode does.

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

var _ = Describe("InComparandSourceJavaProbe", func() {
	It("answers IN lists over an ordered index as Java does", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "incmp_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)
		const schema = "CREATE TABLE T (ID BIGINT, A BIGINT, B BIGINT, PRIMARY KEY (ID)) " +
			"CREATE INDEX IA AS SELECT A, B FROM T ORDER BY A, B " +
			"CREATE TABLE E (ID BIGINT, A BIGINT, B BIGINT, PRIMARY KEY (ID)) " +
			"CREATE INDEX EA AS SELECT A, B FROM E ORDER BY A, B " +
			"CREATE TABLE S (ID BIGINT, NAME STRING, PRIMARY KEY (ID)) " +
			"CREATE TABLE BT (ID BIGINT, D BOOLEAN, PRIMARY KEY (ID)) " +
			"CREATE TABLE AR (ID BIGINT, B BIGINT, XS BIGINT ARRAY, PRIMARY KEY (ID))"
		setup := []string{"INSERT INTO T VALUES (1, 1, 10), (2, 2, 20), (3, 3, 30), (4, 1, 40)", "INSERT INTO BT VALUES (1, TRUE), (2, FALSE)", "INSERT INTO AR VALUES (1, 2, [1, 2]), (2, 5, [1, 2])"}
		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				var je *plandiff.JavaError
				var ge *api.Error
				if errors.As(r.Err, &je) {
					// Java's ArithmeticException is unmapped (XXXXX); Go
					// reports 22012 (DIVERGENCES.md, the SQLSTATE table).
					if je.ExceptionClass == "ArithmeticException" && je.Message == "/ by zero" {
						return "ERR 22012 / by zero"
					}
					return "ERR " + je.SQLState + " " + je.Message
				}
				if errors.As(r.Err, &ge) {
					return "ERR " + string(ge.Code) + " " + ge.Message
				}
				return "ERR " + r.Err.Error()
			}
			return fmt.Sprint(r.Rows.Rows)
		}
		var mismatches []string
		for _, c := range []struct{ name, sql string }{
			{"literal", "SELECT ID FROM T WHERE A IN (1, 3) ORDER BY B"},
			{"duplicate_literal", "SELECT ID FROM T WHERE A IN (1, 1, 3) ORDER BY B"},
			{"duplicate_computed", "SELECT ID FROM T WHERE A IN (1, 1 + 0, 3) ORDER BY B"},
			{"computed", "SELECT ID FROM T WHERE A IN (2 - 1, 3) ORDER BY B"},
			{"cast_item", "SELECT ID FROM T WHERE A IN (CAST('1' AS BIGINT), 3) ORDER BY B"},
			{"duplicate_cast", "SELECT ID FROM T WHERE A IN (CAST('1' AS BIGINT), 1) ORDER BY B"},
			{"div_zero_item", "SELECT ID FROM T WHERE A IN (1 / 0, 3) ORDER BY B"},
			{"div_zero_item_empty", "SELECT ID FROM E WHERE A IN (1 / 0, 3) ORDER BY B"},
			{"div_zero_unordered", "SELECT ID FROM T WHERE A IN (1 / 0, 3)"},
			{"div_zero_nonindexed", "SELECT ID FROM T WHERE ID IN (1 / 0, 3) ORDER BY ID"},
			{"div_zero_equality", "SELECT ID FROM T WHERE A = 1 / 0 ORDER BY B"},
			{"div_zero_select", "SELECT 1 / 0 FROM T"},
			{"mixed_types_computed_item_first", "SELECT ID FROM T WHERE A IN (2 + 1, 'x')"},
			{"mixed_types_literal", "SELECT ID FROM T WHERE A IN (1, 'x')"},
			{"mixed_literal_int_double", "SELECT ID FROM T WHERE A IN (1, 2.5)"},
			{"mixed_literal_int_long", "SELECT ID FROM T WHERE A IN (1, 3000000000)"},
			{"mixed_literal_null", "SELECT ID FROM T WHERE A IN (1, NULL)"},
			{"mixed_literal_negative", "SELECT ID FROM T WHERE A IN (1, -1)"},
			{"mixed_computed_int_double", "SELECT ID FROM T WHERE A IN (2 + 1, 2.5)"},
			{"mixed_computed_int_long", "SELECT ID FROM T WHERE A IN (2 + 1, 3000000000)"},
			{"computed_vs_lhs_string", "SELECT ID FROM T WHERE A IN (2 + 1, 4 + 0) AND 'x' IN (2 + 1)"},
			{"literal_vs_lhs_string", "SELECT ID FROM T WHERE 'x' IN (1, 2)"},
			{"string_col_in_int_literals", "SELECT ID FROM S WHERE NAME IN (1, 2)"},
			{"string_col_in_int_computed", "SELECT ID FROM S WHERE NAME IN (1 + 0, 2)"},
			{"int_col_in_string_literals", "SELECT ID FROM T WHERE A IN ('x', 'y')"},
			{"int_col_in_string_cast", "SELECT ID FROM T WHERE A IN (CAST(1 AS STRING), 'y')"},
			{"string_col_not_in_int_literals", "SELECT ID FROM S WHERE NAME NOT IN (1, 2)"},
			{"string_col_eq_int", "SELECT ID FROM S WHERE NAME = 1"},
			{"bool_in_literal", "SELECT ID FROM BT WHERE D IN (TRUE)"},
			{"bool_in_literals", "SELECT ID FROM BT WHERE D IN (TRUE, FALSE)"},
			{"bool_in_comparison_item", "SELECT ID FROM BT WHERE D IN (3 < 4)"},
			{"bool_in_comparison_items", "SELECT ID FROM BT WHERE D IN (3 < 4, FALSE)"},
			{"column_item_beside_string", "SELECT ID FROM T WHERE B IN (A, 'x')"},
			{"column_items_promotable", "SELECT ID FROM T WHERE B IN (A, 10.0)"},
			{"bracketed_array_item", "SELECT ID FROM AR WHERE B IN (XS)"},
			{"cast_null_item", "SELECT ID FROM T WHERE A IN (1, CAST(NULL AS BIGINT))"},
			{"two_lists", "SELECT ID FROM T WHERE A IN (1, 3) AND B IN (10, 30, 40) ORDER BY ID"},
			{"order_by_a", "SELECT ID FROM T WHERE A IN (3, 1, 1) ORDER BY A, B"},
			{"order_by_a_desc", "SELECT ID FROM T WHERE A IN (3, 1 + 0) ORDER BY A DESC, B DESC"},
			{"unordered_dup", "SELECT ID FROM T WHERE A IN (1, 1 + 0)"},
		} {
			j := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			GinkgoWriter.Printf("INCMP %s\n  java %s\n  go   %s\n", c.name, j, g)
			if j != g {
				mismatches = append(mismatches, fmt.Sprintf("%s: java %s, go %s", c.name, j, g))
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})
})
