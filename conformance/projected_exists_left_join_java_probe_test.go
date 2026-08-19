//go:build bazelrunfiles

package conformance_test

// Measures Java's live behaviour (tag 4.12.11.0 conformance server) for a
// PROJECTED EXISTS over a LEFT JOIN, and pins Go against it.
//
// The shape matters because it was the last consumer of Go's three-quantifier
// NLJ arm (RFC-235). A Go-side test asserts that "Java answers it — a
// Java-parity reach gap", and that claim was inherited rather than measured:
// nothing in the tree asks the JVM. Since retiring the arm turns the shape into
// a planner decline, whether that is a REGRESSION or CONFORMANCE depends
// entirely on the sentence nobody had checked.
//
// So this probe measures it. Whatever it finds is what Go is held to.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ProjectedExistsOverLeftJoinJavaProbe", func() {
	It("measures Java's outcome for projected EXISTS over LEFT JOIN", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("projexists_%s", uuid.New().String())
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

		schema := "CREATE TABLE P (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE Q (qid BIGINT, PRIMARY KEY (qid))" +
			" CREATE TABLE R (id BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO P VALUES (1, 10), (2, 20)",
			"INSERT INTO Q VALUES (7)",
			"INSERT INTO R VALUES (5)",
		}

		probes := []struct {
			name, sql string
			want      string
		}{
			{
				"null_padded_correlated_exists",
				"SELECT P.v, EXISTS (SELECT 1 FROM R WHERE R.id = Q.qid) FROM P LEFT JOIN Q ON Q.qid = P.id",
				"[[10 false] [20 false]]",
			},
			{
				"uncorrelated_exists",
				"SELECT P.v, EXISTS (SELECT 1 FROM R) FROM P LEFT JOIN Q ON Q.qid = P.id",
				"[[10 true] [20 true]]",
			},
			{
				"projected_exists_over_inner_join",
				"SELECT P.v, EXISTS (SELECT 1 FROM R WHERE R.id = Q.qid) FROM P, Q WHERE Q.qid = P.id",
				"[]",
			},
			// CONTROL: the same EXISTS in WHERE rather than the select list. If
			// Java answers this and refuses the projections above, the boundary is
			// the PROJECTION, not the join.
			{
				"where_exists_control",
				"SELECT P.v FROM P LEFT JOIN Q ON Q.qid = P.id WHERE EXISTS (SELECT 1 FROM R WHERE R.id = Q.qid)",
				"[]",
			},
			// A null-REJECTING conjunct on the null-supplying side. It makes the
			// LEFT JOIN semantically INNER, so a plan that drives from the
			// null-supplying leg and drops unmatched preserved rows is CORRECT.
			// Go now produces exactly that shape and a plan-only pin called it a
			// lost LEFT OUTER; this asks the JVM which reading is right.
			{
				"null_rejecting_conjunct_makes_it_inner",
				"SELECT Q.qid FROM Q LEFT JOIN P ON P.id = Q.qid WHERE P.v = 10",
				"[]",
			},
			// The ANTI-JOIN twin, which must KEEP the null extension: IS NULL is
			// satisfied only BY the null-extended rows. Kept beside the case above
			// because one without the other cannot tell a correct conversion from
			// an outer join being lost.
			{
				"is_null_conjunct_keeps_the_outer",
				"SELECT Q.qid FROM Q LEFT JOIN P ON P.id = Q.qid WHERE P.v IS NULL",
				"[[7]]",
			},
		}

		render := func(engine string, r plandiff.RunResult) string {
			if r.Err != nil {
				var je *plandiff.JavaError
				if errors.As(r.Err, &je) {
					return fmt.Sprintf("%s ERROR sqlstate=%q msg=%q", engine, je.SQLState, je.Message)
				}
				var ge *api.Error
				if errors.As(r.Err, &ge) {
					return fmt.Sprintf("%s ERROR sqlstate=%q msg=%q", engine, string(ge.Code), ge.Message)
				}
				return fmt.Sprintf("%s ERROR %v", engine, r.Err)
			}
			return fmt.Sprintf("%s OK rows=%v", engine, r.Rows.Rows)
		}

		// The full outcome is still printed — a probe whose verdict is its output
		// must be readable — but printing is NOT the verdict. Every outcome is
		// ASSERTED, because a spec that only prints passes when both engines error
		// on every shape, and GinkgoWriter is buffered, so in a normal CI run that
		// output is never even emitted. A silent probe and an agreeing probe are
		// the same green; this makes them different.
		//
		// Java is pinned as the REFERENCE, and Go is required to MATCH it rather
		// than to match a separately-written literal: the claim these shapes carry
		// is parity, so the assertion should fail the moment the two part company,
		// whatever either one says.
		Expect(probes).To(HaveLen(6), "the probe set shrank; a loop over an empty or "+
			"truncated table asserts nothing and reports green")
		for _, p := range probes {
			jr := runner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			fmt.Fprintf(GinkgoWriter, "PROJEXISTS %s\n  %s\n  %s\n  sql: %s\n",
				p.name, render("JAVA", jr), render("GO  ", gr), p.sql)

			Expect(jr.Err).NotTo(HaveOccurred(), "%s: JAVA must answer this shape", p.name)
			Expect(gr.Err).NotTo(HaveOccurred(), "%s: GO must answer this shape. A decline here is "+
				"the reach gap RFC-235 section 16 closed re-opening.", p.name)
			Expect(fmt.Sprint(jr.Rows.Rows)).To(Equal(p.want),
				"%s: Java's rows moved from the measured reference. Re-measure before changing "+
					"anything on the Go side.", p.name)
			Expect(fmt.Sprint(gr.Rows.Rows)).To(Equal(p.want),
				"%s: Go no longer agrees with Java on a shape that was measured to agree. For the "+
					"two projected-EXISTS-over-LEFT-JOIN rows this is the box regressing; for the "+
					"controls it is the null-extension being lost or kept wrongly.", p.name)
		}
	})
})
