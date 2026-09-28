//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/relational/api"
)

// An INSERT ... SELECT's row is admitted to the table as a whole: Java's
// RecordQueryInsertPlan.insertPlan computes PromoteValue.computePromotionsTrie
// over the table type and the source's flowed row, which checks the field count
// before any field (PromoteValue.java:419, INCOMPATIBLE_TYPE). So a projection
// too wide or too narrow is refused while planning, whether or not the source
// yields a row, and an unnested element does not spread over the table's
// columns. Each statement runs on both engines; the answers must be equal.
var _ = Describe("RFC-257 INSERT ... SELECT arity is admitted as the target admits it", func() {
	It("refuses a row of the wrong width while planning, rows or none", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		p := "WSJARITY" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		const body = "create table t1(id bigint, arr integer array, primary key(id)) " +
			"create table w(id bigint, x bigint, primary key(id)) " +
			"create table src(id bigint, v bigint, primary key(id)) " +
			"create table dst(id bigint, v bigint, primary key(id))"
		stmts := []string{
			// No rows yet: the verdict does not wait for one.
			`INSERT INTO dst SELECT id, v, id FROM src`,
			`INSERT INTO dst SELECT id FROM src`,
			`INSERT INTO dst SELECT "V" FROM t1, t1."ARR" AS "V", w`,
			`INSERT INTO dst SELECT t1."ID", "V" FROM t1, t1."ARR" AS "V", w`,
			`INSERT INTO dst SELECT id, v FROM src`,
			`INSERT INTO src VALUES (1, 10)`,
			`INSERT INTO t1 VALUES (1, [5])`,
			`INSERT INTO w VALUES (1, 7)`,
			// Rows now: the same verdicts, and the admitted ones write.
			`INSERT INTO dst SELECT id, v, id FROM src`,
			`INSERT INTO dst SELECT id FROM src`,
			`INSERT INTO dst SELECT "V" FROM t1, t1."ARR" AS "V", w`,
			`INSERT INTO dst SELECT id, v FROM src`,
			`INSERT INTO dst SELECT t1."ID" + 1, "V" FROM t1, t1."ARR" AS "V", w`,
		}

		javaDB := "/TEST/" + p + "_J"
		javaDDL := func(stmts ...string) {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{"clusterFile": clusterFile, "statements": stmts}, &out)).To(Succeed())
			for i, o := range out["outcomes"].([]any) {
				Expect(o).To(Equal("OK"), "%s", stmts[i])
			}
		}
		javaDDL("CREATE DATABASE "+javaDB, "CREATE SCHEMA TEMPLATE "+p+"_JT "+body, "CREATE SCHEMA "+javaDB+"/S WITH TEMPLATE "+p+"_JT")
		defer javaDDL("DROP DATABASE IF EXISTS "+javaDB, "DROP SCHEMA TEMPLATE IF EXISTS "+p+"_JT")
		javaExec := func(stmt string) string {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "sql": stmt,
			}, &out)).To(Succeed())
			o := out["outcome"].(string)
			if f := strings.SplitN(o, " ", 4); f[0] == "ERROR" && len(f) == 4 {
				return "ERROR " + f[1] + " " + f[3]
			}
			return o
		}
		javaRows := func(q string) string {
			var out struct {
				Rows [][]any `json:"rows"`
			}
			Expect(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "querySql": q,
			}, &out)).To(Succeed())
			return fmt.Sprint(out.Rows)
		}

		goDBPath := "/TEST/" + p + "_G"
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		for _, stmt := range []string{
			"CREATE DATABASE " + goDBPath, "CREATE SCHEMA TEMPLATE " + p + "_GT " + body,
			"CREATE SCHEMA " + goDBPath + "/S WITH TEMPLATE " + p + "_GT",
		} {
			_, err := sysDB.ExecContext(ctx, stmt)
			Expect(err).NotTo(HaveOccurred(), "%s", stmt)
		}
		defer func() {
			_, _ = sysDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+goDBPath)
			_, _ = sysDB.ExecContext(ctx, "DROP SCHEMA TEMPLATE IF EXISTS "+p+"_GT")
		}()
		gdb, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", goDBPath, clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer gdb.Close()
		goExec := func(stmt string) string {
			res, err := gdb.ExecContext(ctx, stmt)
			if err != nil {
				var ae *api.Error
				if errors.As(err, &ae) {
					return "ERROR " + string(ae.Code) + " " + ae.Message
				}
				return "ERROR ? " + err.Error()
			}
			n, err := res.RowsAffected()
			Expect(err).NotTo(HaveOccurred())
			return fmt.Sprintf("OK %d", n)
		}
		goRows := func(q string) string {
			rs, err := gdb.QueryContext(ctx, q)
			Expect(err).NotTo(HaveOccurred())
			defer rs.Close()
			var rows [][]any
			for rs.Next() {
				var id, v int64
				Expect(rs.Scan(&id, &v)).To(Succeed())
				rows = append(rows, []any{float64(id), float64(v)})
			}
			Expect(rs.Err()).NotTo(HaveOccurred())
			return fmt.Sprint(rows)
		}

		var diffs []string
		for _, stmt := range stmts {
			j, g := javaExec(stmt), goExec(stmt)
			GinkgoWriter.Printf("WSJARITY %s\n  java=%s\n  go  =%s\n", stmt, j, g)
			if j != g {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s", stmt, j, g))
			}
		}
		const q = "SELECT id, v FROM dst ORDER BY id"
		j, g := javaRows(q), goRows(q)
		GinkgoWriter.Printf("WSJARITY %s\n  java=%s\n  go  =%s\n", q, j, g)
		Expect(j).To(Equal("[[1 10] [2 5]]"), "the target's writes")
		if j != g {
			diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s", q, j, g))
		}
		Expect(diffs).To(BeEmpty())
	})
})
