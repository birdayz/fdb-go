//go:build bazelrunfiles

package conformance_test

// PROBE (investigation scratch): what does JAVA's fdb-relational answer for a
// grouped SUM whose group holds a PRESENT-zero aggregate-index key and only
// NULL-valued rows?
//
// Sequence per group:
//   g=10  UPDATE the last non-NULL value to NULL (a NULL row remains)
//   g=11  DELETE the last non-NULL row        (a NULL row remains)
//   g=12  control: all-NULL from the start (no SUM key was ever written)
//   g=13  control: live, non-zero
//
// The indexed table `ai` carries the SUM index; `ao` carries none and is the
// scan oracle. If Java answers 0 for g=10/g=11 from its index and NULL from
// its scan, Java has the same index-vs-scan divergence Go does.

import (
	"context"
	"fmt"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PROBE zero-key all-NULL group (Java)", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer,
			fmt.Sprintf("zkanprobe_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
	})
	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	const indexedTemplate = "CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) " +
		"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g"
	const plainTemplate = "CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk))"

	setup := []string{
		"INSERT INTO ai (pk,g,v) VALUES (1,10,7),(2,10,NULL),(3,11,9),(4,11,NULL)," +
			"(5,12,NULL),(6,12,NULL),(7,13,3),(8,13,4)",
		"UPDATE ai SET v = NULL WHERE pk = 1",
		"DELETE FROM ai WHERE pk = 3",
	}
	const q = "SELECT g, SUM(v) FROM ai GROUP BY g"

	It("reports Java's indexed and unindexed answers", func() {
		eng := plandiff.NewJavaEngineHTTP(javaBaseURL(java), env.ClusterFile)
		plan := eng.Plan(ctx, plandiff.Query{
			Name: "zkan_plan", SQL: q, SchemaTemplate: indexedTemplate,
		})
		GinkgoWriter.Printf("JAVA PLAN (indexed) err=%v\n%s\n", plan.Err, plan.Tree)
		planPlain := eng.Plan(ctx, plandiff.Query{
			Name: "zkan_plan_plain", SQL: q, SchemaTemplate: plainTemplate,
		})
		GinkgoWriter.Printf("JAVA PLAN (no index) err=%v\n%s\n", planPlain.Err, planPlain.Tree)

		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile).(plandiff.SetupRunner)

		idx := runner.RunWithSetup(ctx, indexedTemplate, setup, q)
		GinkgoWriter.Printf("JAVA ROWS (indexed) err=%v rows=%v\n", idx.Err, idx.Rows.Rows)
		plainRes := runner.RunWithSetup(ctx, plainTemplate, setup, q)
		GinkgoWriter.Printf("JAVA ROWS (no index) err=%v rows=%v\n", plainRes.Err, plainRes.Rows.Rows)

		// Java 4.12.11.0's Cascades cannot plan a GROUP BY with no matching
		// aggregate index, so the scan oracle has to be reached another way:
		// a predicate the aggregate index cannot serve forces the streaming
		// aggregation over base records even though the index exists.
		for _, oq := range []string{
			"SELECT g, SUM(v) FROM ai WHERE pk >= 0 GROUP BY g",
			"SELECT g, SUM(v) FROM ai WHERE v IS NULL OR v IS NOT NULL GROUP BY g",
			"SELECT g, SUM(v) FROM ai GROUP BY g HAVING SUM(v) IS NULL",
		} {
			p := eng.Plan(ctx, plandiff.Query{Name: "o", SQL: oq, SchemaTemplate: indexedTemplate})
			r := runner.RunWithSetup(ctx, indexedTemplate, setup, oq)
			GinkgoWriter.Printf("JAVA ORACLE-ATTEMPT %q\n  plan=%v %s\n  err=%v rows=%v\n",
				oq, p.Err, p.Tree, r.Err, r.Rows.Rows)
		}
		// Java's own COUNT(*)/COUNT(v) view of the same groups, for the
		// group-existence picture.
		for _, cq := range []string{
			"SELECT g, COUNT(*) FROM ai GROUP BY g",
			"SELECT g, COUNT(v) FROM ai GROUP BY g",
			"SELECT g, MIN(v) FROM ai GROUP BY g",
		} {
			tpl := "CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) " +
				"CREATE INDEX ai_c AS " + cq
			p := eng.Plan(ctx, plandiff.Query{Name: "c", SQL: cq, SchemaTemplate: tpl})
			r := runner.RunWithSetup(ctx, tpl, setup, cq)
			GinkgoWriter.Printf("JAVA OTHER-AGG %q\n  plan=%v %s\n  err=%v rows=%v\n",
				cq, p.Err, p.Tree, r.Err, r.Rows.Rows)
		}

		Expect(idx.Err).NotTo(HaveOccurred())
		GinkgoWriter.Printf("PROBE SUMMARY java-indexed=%v\n", idx.Rows.Rows)

		// THE MEASURED JAVA ANSWER. g=10 (last non-NULL UPDATEd away) and g=11
		// (last non-NULL row DELETEd) both read 0 from Java's own aggregate
		// index, exactly as Go's group-existence merge does — the SUM index
		// cannot distinguish "the non-NULL values sum to zero" from "there are
		// no non-NULL values", in either engine. g=12 (all-NULL from the start,
		// so no SUM key was ever written) is DROPPED by Java entirely, where Go
		// emits (12, NULL); that is the one axis on which Go is ahead.
		//
		// If this expectation ever fails, Java changed and the shared-defect
		// classification has to be revisited — Go would then be uniquely wrong.
		Expect(fmt.Sprint(idx.Rows.Rows)).To(Equal("[[10 0] [11 0] [13 7]]"),
			"Java's aggregate-index SUM for the present-zero all-NULL group")

		// Java 4.12.11.0 cannot plan a grouped aggregate with no matching
		// aggregate index, so there is no executable Java scan oracle for this
		// query shape. Pinned because it is why the comparison above is
		// index-vs-index rather than index-vs-scan.
		Expect(plainRes.Err).To(HaveOccurred())
		Expect(plainRes.Err.Error()).To(ContainSubstring("UnableToPlanException"))
	})
})
