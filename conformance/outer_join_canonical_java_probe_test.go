//go:build bazelrunfiles

package conformance_test

// Where does each engine put a LEFT JOIN's WHERE conjuncts?
//
// Java's REWRITING keeps RewriteOuterJoinRule's canonical form (outerJoinCount
// ranks first), merges the enclosing block into it and pushes conjuncts down:
// a preserved-side conjunct probes the preserved leg, a null-supplied-side one
// filters above ON EMPTY NULL, an EXISTS over the null-supplied leg joins
// inside that leg's FlatMap, and a conjunct rejecting the null-extended row
// turns the join inner. Go plans the same shapes; for an unindexed anti-join
// it may instead pick its materialized outer join (RFC-152), so that arm pins
// rows only.

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

var _ = Describe("OuterJoinCanonicalJavaProbe", func() {
	It("places LEFT JOIN conjuncts as Java does", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("ojcanon_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		explain := func(r plandiff.RunResult) string {
			if r.Err != nil || len(r.Rows.Rows) == 0 {
				return fmt.Sprintf("ERR(%v)", r.Err)
			}
			return fmt.Sprint(r.Rows.Rows[0][0])
		}
		rows := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			parts := make([]string, 0, len(r.Rows.Rows))
			for _, row := range r.Rows.Rows {
				parts = append(parts, fmt.Sprint(row))
			}
			sort.Strings(parts)
			return strings.Join(parts, " ")
		}

		const whq = "CREATE TABLE w (id BIGINT, f BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE h (id BIGINT, f BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id)) " +
			"CREATE TABLE q (id BIGINT, PRIMARY KEY (id)) CREATE INDEX hf AS SELECT f FROM h"
		whqSetup := []string{
			"INSERT INTO w VALUES (1, 10), (2, 20), (3, 30)",
			"INSERT INTO h VALUES (1, 10, [1]), (2, 20, [2])",
			"INSERT INTO q VALUES (1)",
		}
		const ints = "CREATE TABLE t_int (id BIGINT, val INTEGER, label STRING, PRIMARY KEY (id)) " +
			"CREATE TABLE t_int2 (id BIGINT, ref_id INTEGER, amount INTEGER, PRIMARY KEY (id))"
		intsSetup := []string{
			"INSERT INTO t_int VALUES (1, 5, 'a'), (2, 6, 'b'), (3, 7, 'c')",
			"INSERT INTO t_int2 VALUES (1, 6, 100), (2, 6, 200), (3, 9, 300)",
		}
		const empDept = "CREATE TABLE emp (eid BIGINT, etags BIGINT ARRAY NOT NULL, PRIMARY KEY (eid)) " +
			"CREATE TABLE dept (did BIGINT, dtags BIGINT ARRAY NOT NULL, PRIMARY KEY (did))"
		empDeptSetup := []string{
			"INSERT INTO emp VALUES (1, [1]), (2, [2]), (3, [3])",
			"INSERT INTO dept VALUES (2, [20])",
		}
		const ab = "CREATE TABLE a (id BIGINT, k BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE b (id BIGINT, k BIGINT, label STRING, PRIMARY KEY (id))"
		abSetup := []string{
			"INSERT INTO a VALUES (1, 1), (2, 2), (3, 3)",
			"INSERT INTO b VALUES (1, 1, 'x'), (2, 2, NULL)",
		}

		arms := []struct {
			name, schema, sql, want string
			setup                   []string
			java, goPlan            []string
			javaNot, goNot          []string
		}{
			{
				name: "exists_over_null_supplied", schema: whq, setup: whqSetup,
				sql:    "SELECT w.id, h.id FROM w LEFT JOIN h ON h.f = w.f WHERE NOT EXISTS (SELECT 1 FROM h.arr x WHERE x = 2)",
				want:   "[1 1] [3 <nil>]",
				java:   []string{"SCAN([IS W]) | FLATMAP q0 -> { ISCAN(HF [EQUALS q0.F]) | ON EMPTY NULL | FLATMAP"},
				goPlan: []string{"FlatMap(outer=Scan(W), inner=FlatMap(outer=DefaultOnEmpty(IndexScan(HF, [=]))"},
			},
			{
				name: "preserved_side_probe", schema: ints, setup: intsSetup,
				sql:    "SELECT t_int.label, t_int2.amount FROM t_int LEFT JOIN t_int2 ON t_int.val = t_int2.ref_id WHERE t_int.id = 2",
				want:   "[b 100] [b 200]",
				java:   []string{"SCAN([IS T_INT, EQUALS", "ON EMPTY NULL"},
				goPlan: []string{"FlatMap(outer=Scan(T_INT, [=]), inner=DefaultOnEmpty("},
			},
			{
				name: "null_supplied_filter_above_on_empty", schema: empDept, setup: empDeptSetup,
				sql:    "SELECT d.eid FROM (SELECT a.eid, a.etags, b.did, b.dtags FROM emp AS a LEFT JOIN dept AS b ON b.did = a.eid) AS d WHERE d.dtags IS NULL ORDER BY d.eid",
				want:   "[1] [3]",
				java:   []string{"ON EMPTY NULL | FILTER _.DTAGS IS_NULL"},
				goPlan: []string{"FlatMap(outer=Scan(EMP), inner=PredicatesFilter(DefaultOnEmpty(Scan(DEPT, [=])), [1 preds]))"},
			},
			{
				name: "null_rejecting_lateral_turns_inner", schema: whq, setup: whqSetup,
				sql:     "SELECT w.id, d.x FROM w LEFT JOIN h ON h.id = w.id, (SELECT q.id AS x FROM q WHERE q.id = h.id) AS d",
				want:    "[1 1]",
				javaNot: []string{"ON EMPTY NULL"},
				goNot:   []string{"DefaultOnEmpty", "LEFT OUTER"},
			},
			{
				name: "unindexed_anti_join", schema: ab, setup: abSetup,
				sql:  "SELECT a.id FROM a LEFT OUTER JOIN b ON a.k = b.k WHERE b.label IS NULL",
				want: "[2] [3]",
				java: []string{"ON EMPTY NULL | FILTER _.LABEL IS_NULL"},
			},
		}
		for _, a := range arms {
			javaPlan := explain(javaRunner.RunWithSetup(ctx, a.schema, a.setup, "EXPLAIN "+a.sql))
			goPlan := explain(goRunner.RunWithSetup(ctx, a.schema, a.setup, "EXPLAIN "+a.sql))
			javaRows := rows(javaRunner.RunWithSetup(ctx, a.schema, a.setup, a.sql))
			goRows := rows(goRunner.RunWithSetup(ctx, a.schema, a.setup, a.sql))
			fmt.Fprintf(GinkgoWriter, "%s\n  java %s -> %s\n  go   %s -> %s\n", a.name, javaPlan, javaRows, goPlan, goRows)
			for _, s := range a.java {
				Expect(javaPlan).To(ContainSubstring(s), "%s: Java's plan moved", a.name)
			}
			for _, s := range a.javaNot {
				Expect(javaPlan).NotTo(ContainSubstring(s), "%s: Java's plan moved", a.name)
			}
			for _, s := range a.goPlan {
				Expect(goPlan).To(ContainSubstring(s), "%s: Go no longer plans Java's shape", a.name)
			}
			for _, s := range a.goNot {
				Expect(goPlan).NotTo(ContainSubstring(s), "%s: Go no longer plans Java's shape", a.name)
			}
			Expect(javaRows).To(Equal(a.want), "%s: Java rows", a.name)
			Expect(goRows).To(Equal(a.want), "%s: Go rows", a.name)
		}
	})
})
