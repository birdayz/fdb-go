//go:build bazelrunfiles

package conformance_test

// MEASURES cross-engine ROW ORDER for an unconstrained comma join, with and
// without a leg-independent EXISTS, and PINS both sides.
//
// The conformance harness compares rows with reflect.DeepEqual, so join order
// is observable and an order flip is a real divergence. This file exists
// because one showed up and the question "which order is correct" is one only
// the JVM answers.
//
// WHAT IT FOUND, measured on both engines at 4.12.11.0:
//
//	Java puts the FIRST FROM item outermost, always — [5 7 9 5 7 9] on every
//	shape below. Go puts the SECOND outermost for a plain cross product
//	([5 5 7 7 9 9]) and agrees with Java once an EXISTS is present, EXCEPT when
//	the existential's subquery is structurally identical to a leg's own scan.
//
// THE CAUSE IS NOT THE EXISTS, and it is not new. Go's cost model reaches a
// genuine TIE on the two nestings of an unconstrained cross product and
// resolves it by its own hash; Java rarely reaches that tie at all, because it
// prunes each Reference to one member mid-phase and Go does not
// (planning_cost_model.go:562 states this). So the plain-cross-product row on
// the first line is a PRE-EXISTING divergence measured identically at the
// merge-base, and the EXISTS rows are the same tie surfacing in one more shape
// once the three-quantifier NLJ arm stopped forcing a particular nesting.
// Mutation-checked: inverting the statistics rung that sits above the hash
// tie-break changes none of these plans, so that rung is not the decider.
//
// DIRECTION OF THE ALARM. Java's column is the reference and must not move.
// Go's column pins TODAY'S behaviour, divergence included, so that closing the
// gap turns this test RED rather than silently changing what conformance
// means. A red here after a cost-model or prune-to-1 change is the good case:
// update Go's column to Java's and delete the divergence note.

import (
	"context"
	"fmt"
	"os"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DupAliasExistsOrderProbe", func() {
	It("pins cross-engine row order for a comma join with and without EXISTS", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("dupexorder_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		schema := "CREATE TABLE T_DUP_EIP (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE T_DUP_EIQ (qid BIGINT, PRIMARY KEY (qid))"
		setup := []string{
			"INSERT INTO T_DUP_EIP VALUES (1, 10), (2, 20)",
			"INSERT INTO T_DUP_EIQ VALUES (5), (7), (9)",
		}

		// fromOrder is Java's answer everywhere: the first FROM item outermost,
		// so its 2 rows are the OUTER loop and qid cycles 5,7,9 twice.
		// Rendered rather than typed: the two runners box integers differently and
		// the claim here is about ORDER, so comparing the rendering keeps the
		// assertion on the thing being measured.
		render := func(r plandiff.RunResult) string { return fmt.Sprint(r.Rows.Rows) }
		fromOrder := "[[5] [7] [9] [5] [7] [9]]"
		// reversed is Go's answer where the tie falls the other way: the SECOND
		// FROM item outermost, so each qid repeats adjacently.
		reversed := "[[5] [5] [7] [7] [9] [9]]"

		probes := []struct {
			name     string
			sql      string
			wantJava string
			wantGo   string
			note     string
		}{
			{
				// THE CONTROL, and the whole diagnosis: no existential anywhere,
				// and the engines already disagree. Measured identical at the
				// merge-base, so nothing about the EXISTS work caused it.
				name:     "plain_cross_product_no_exists",
				sql:      "SELECT a.qid FROM T_DUP_EIP AS a, T_DUP_EIQ AS a",
				wantJava: fromOrder,
				wantGo:   reversed,
				note:     "PRE-EXISTING tie divergence; Go lacks Java's prune-to-1",
			},
			{
				// The subquery is structurally identical to the first leg's own
				// scan, which is the only EXISTS shape where Go's tie falls the
				// reversed way.
				name:     "exists_scans_first_legs_table",
				sql:      "SELECT a.qid FROM T_DUP_EIP AS a, T_DUP_EIQ AS a WHERE EXISTS (SELECT 1 FROM T_DUP_EIP)",
				wantJava: fromOrder,
				wantGo:   reversed,
				note:     "same tie as the control, surfaced through the peel",
			},
			{
				// A different table in the subquery — engines AGREE. Kept because
				// an all-divergent set cannot show that the divergence is
				// tie-specific rather than EXISTS-specific.
				name:     "exists_scans_second_legs_table",
				sql:      "SELECT a.qid FROM T_DUP_EIP AS a, T_DUP_EIQ AS a WHERE EXISTS (SELECT 1 FROM T_DUP_EIQ)",
				wantJava: fromOrder,
				wantGo:   fromOrder,
			},
			{
				// Same table as the first leg but FILTERED, so not structurally
				// identical — engines AGREE. This is the pair that localises the
				// trigger to structural identity.
				name:     "exists_scans_first_legs_table_filtered",
				sql:      "SELECT a.qid FROM T_DUP_EIP AS a, T_DUP_EIQ AS a WHERE EXISTS (SELECT 1 FROM T_DUP_EIP WHERE id = 1)",
				wantJava: fromOrder,
				wantGo:   fromOrder,
			},
			{
				// The shadowing spelling: the subquery rebinds `a`. Engines AGREE.
				name:     "shadowing_exists",
				sql:      "SELECT a.qid FROM T_DUP_EIP AS a, T_DUP_EIQ AS a WHERE EXISTS (SELECT 1 FROM T_DUP_EIP AS a WHERE a.id = 1)",
				wantJava: fromOrder,
				wantGo:   fromOrder,
			},
		}

		// THE NAME-SENSITIVITY DEMONSTRATION, and the reason this divergence reads
		// as arbitrary rather than as a rule about FROM order. The corpus entries
		// that surfaced this run the SAME two shapes over tables named SHP/SHQ
		// instead of EIP/EIQ, and they fall the OTHER way — the shadowing spelling
		// above agrees with Java here and diverges there. Nothing about the query
		// differs; only the identifiers the tie-break hash consumes do.
		nameSchema := "CREATE TABLE T_DUP_SHP (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE T_DUP_SHQ (qid BIGINT, PRIMARY KEY (qid))"
		nameSetup := []string{
			"INSERT INTO T_DUP_SHP VALUES (1, 10), (2, 20)",
			"INSERT INTO T_DUP_SHQ VALUES (5), (7), (9)",
		}
		const nameSQL = "SELECT a.qid FROM T_DUP_SHP AS a, T_DUP_SHQ AS a " +
			"WHERE EXISTS (SELECT 1 FROM T_DUP_SHP AS a WHERE a.id = 1)"
		nj := runner.RunWithSetup(ctx, nameSchema, nameSetup, nameSQL)
		ng := goRunner.RunWithSetup(ctx, nameSchema, nameSetup, nameSQL)
		fmt.Fprintf(GinkgoWriter, "DUPEXORDER renamed_shadowing_exists\n  JAVA %v\n  GO   %v\n  sql: %s\n",
			nj.Rows.Rows, ng.Rows.Rows, nameSQL)
		Expect(nj.Err).NotTo(HaveOccurred())
		Expect(ng.Err).NotTo(HaveOccurred())
		Expect(render(nj)).To(Equal(fromOrder),
			"renamed_shadowing_exists: Java must still put the first FROM item outermost")
		Expect(render(ng)).To(Equal(reversed),
			"renamed_shadowing_exists: this is the SAME query as shadowing_exists with the\n"+
				"tables renamed, and it must still fall the OTHER way. If both spellings now\n"+
				"agree with Java the tie-break stopped depending on identifiers, which is the\n"+
				"gap closing — re-pin both and un-annotate the corpus entries.")

		for _, p := range probes {
			jr := runner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			fmt.Fprintf(GinkgoWriter, "DUPEXORDER %s\n  JAVA %v\n  GO   %v\n  sql: %s\n",
				p.name, jr.Rows.Rows, gr.Rows.Rows, p.sql)

			Expect(jr.Err).NotTo(HaveOccurred(), "%s: Java must answer this query", p.name)
			Expect(gr.Err).NotTo(HaveOccurred(), "%s: Go must answer this query", p.name)

			Expect(render(jr)).To(Equal(p.wantJava),
				"%s: JAVA's row order moved. Java is the reference here and it puts the "+
					"first FROM item outermost; if this is red, re-measure before touching "+
					"anything on the Go side.", p.name)

			if p.note != "" {
				Expect(render(gr)).To(Equal(p.wantGo),
					"%s: Go's row order changed from the pinned divergence (%s). If Go now "+
						"MATCHES Java, that is the gap closing: replace wantGo with fromOrder, "+
						"drop this note, and un-annotate the two dup-alias corpus entries.",
					p.name, p.note)
				continue
			}
			Expect(render(gr)).To(Equal(p.wantGo),
				"%s: Go and Java AGREED on this shape and no longer do. This is the "+
					"cost-model tie spreading to a shape that was conforming.", p.name)
		}
	})
})
