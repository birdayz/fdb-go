package conformance_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The target's planner task count on the six-table joins Go's planning-time
// regressions use, so the engines' costs are compared on the same query. The
// target's tasks are not Go's units: these pins are the target's own.
var _ = Describe("Six-table chain planning cost", func() {
	It("records the target's planner task count and time for both six-table chains", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
		defer cancel()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		java := NewJavaInvoker()

		// planner_budget_test's star schema, joined as an FK chain (sixTableChainSQL).
		starDDL := "CREATE TABLE H (id BIGINT, v BIGINT, PRIMARY KEY (id))"
		for i := 1; i <= 6; i++ {
			starDDL += fmt.Sprintf(" CREATE TABLE S%d (id BIGINT, hid BIGINT, PRIMARY KEY (id))", i)
		}
		// TODO.md's large-join probe: t1..t6 linked by next_id.
		nextDDL := ""
		for i := 1; i <= 6; i++ {
			nextDDL += fmt.Sprintf(" CREATE TABLE t%d (id BIGINT, next_id BIGINT, PRIMARY KEY (id))", i)
		}
		probes := []struct {
			name, schema, sql string
			tasks             int
		}{
			{"next_id_chain", nextDDL, "SELECT t1.id FROM t1, t2, t3, t4, t5, t6 WHERE t1.next_id = t2.id " +
				"AND t2.next_id = t3.id AND t3.next_id = t4.id AND t4.next_id = t5.id AND t5.next_id = t6.id", 122_839},
			{"star_chain", starDDL, "SELECT H.id, S1.id, S2.id, S3.id, S4.id, S5.id FROM H, S1, S2, S3, S4, S5 " +
				"WHERE H.id = S1.hid AND S1.id = S2.hid AND S2.id = S3.hid AND S3.id = S4.hid AND S4.id = S5.hid", 541_965},
		}
		for _, pr := range probes {
			var trace struct {
				Elapsed int64          `json:"elapsedNanos"`
				Tasks   map[string]int `json:"tasksPerPhase"`
			}
			Expect(java.InvokeAs(ctx, "planRuleTrace", map[string]any{
				"clusterFile": clusterFile, "schemaTemplate": pr.schema, "setupSqls": []string{},
				"querySql": pr.sql, "rules": []string{"TASK-COUNT"},
			}, &trace)).To(Succeed(), pr.name)
			total := 0
			for _, n := range trace.Tasks {
				total += n
			}
			fmt.Fprintf(GinkgoWriter, "SIX-TABLE-COST %s tasks=%d phases=%v elapsed=%s\n",
				pr.name, total, trace.Tasks, time.Duration(trace.Elapsed))
			// 2% matches the Go-side task bands; the target's search is deterministic.
			tol := pr.tasks / 50
			Expect(total).To(BeNumerically("~", pr.tasks, tol), pr.name)
		}
	})
})
