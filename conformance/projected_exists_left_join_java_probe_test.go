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

var _ = Describe("FixedFactorUnionRangeJava", func() {
	It("plans the indexed disjunction beside a correlated range EXISTS", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("unionrange_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		var trace map[string]any
		request := map[string]any{
			"clusterFile":    env.ClusterFile,
			"schemaTemplate": "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY(id)) CREATE INDEX idx_a ON T_RD(a) CREATE INDEX idx_e ON T_RD(e) CREATE INDEX idx_s ON T_RD(s)",
			"setupSqls":      []string{},
			"querySql":       "SELECT * FROM t_rd WHERE (((s IS NOT NULL) AND (a=8) AND (e BETWEEN 3.0 AND 4.0)) OR ((d IN (2,5)) AND (COALESCE(a,8)<5))) AND EXISTS (SELECT 1 FROM t_rd AS r WHERE r.c<t_rd.c) ORDER BY c DESC NULLS FIRST,id",
			"rules":          []string{"NormalizePredicatesRule", "PredicateToLogicalUnionRule", "PartitionBinarySelectRule", "ImplementFilterRule"},
		}
		err = srv.InvokeAs(ctx, "planRuleTrace", request, &trace)
		var javaErr *JavaError
		Expect(errors.As(err, &javaErr)).To(BeTrue(), "Java has no access path ordered by C")
		Expect(javaErr.ExceptionClass).To(Equal("UnableToPlanException"))
		Expect(javaErr.SQLState).To(Equal("0AF00"))
		request["querySql"] = "SELECT * FROM t_rd WHERE (((s IS NOT NULL) AND (a=8) AND (e BETWEEN 3.0 AND 4.0)) OR ((d IN (2,5)) AND (COALESCE(a,8)<5))) AND EXISTS (SELECT 1 FROM t_rd AS r WHERE r.c<t_rd.c)"
		err = srv.InvokeAs(ctx, "planRuleTrace", request, &trace)
		fmt.Fprintf(GinkgoWriter, "UNIONRANGE unordered trace=%v error=%v\n", trace, err)
		Expect(err).NotTo(HaveOccurred())
		Expect(trace["explain"]).NotTo(BeEmpty())
	})
})

var _ = Describe("FixedFactorUnionScalarJava", func() {
	It("pins front-end rejection and the equivalent relational planner failure", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("unionscalar_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		var trace map[string]any
		err = srv.InvokeAs(ctx, "planRuleTraceOutcome", map[string]any{
			"clusterFile":    env.ClusterFile,
			"schemaTemplate": "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)",
			"setupSqls":      []string{},
			"querySql":       "SELECT * FROM t_rd WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (ABS(c) = 4))) AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9) AND c <= (SELECT MIN(a) FROM t_rd) ORDER BY b, id",
			"rules":          []string{"TASK-COUNT", "NormalizePredicatesRule", "PredicateToLogicalUnionRule", "PartitionBinarySelectRule", "ImplementFilterRule"},
		}, &trace)
		Expect(err).NotTo(HaveOccurred())
		Expect(trace["sqlState"]).To(Equal("42601"))
		Expect(trace["error"]).NotTo(BeEmpty())
		Expect(trace["tasksPerPhase"]).To(BeEmpty())
		const schema = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)"
		const pairedSQL = "SELECT t_rd.* FROM t_rd, (SELECT MIN(a) AS min_a FROM t_rd) AS m WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (c = 4 OR c = -4))) AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9) AND c <= m.min_a"
		for _, order := range []string{" ORDER BY b, id", " ORDER BY b, id", "", ""} {
			trace = nil
			err = srv.InvokeAs(ctx, "planRuleTraceOutcome", map[string]any{
				"clusterFile": env.ClusterFile, "schemaTemplate": schema, "setupSqls": []string{},
				"querySql": pairedSQL + order,
				"rules":    []string{"TASK-COUNT", "NormalizePredicatesRule", "PredicateToLogicalUnionRule", "PartitionBinarySelectRule", "ImplementFilterRule"},
			}, &trace)
			fmt.Fprintf(GinkgoWriter, "UNIONSCALAR order=%q trace=%v error=%v\n", order, trace, err)
			Expect(err).NotTo(HaveOccurred())
			Expect(trace["tasksPerKind"]).NotTo(BeEmpty())
			Expect(trace["exceptionClass"]).To(Equal("StackOverflowError"))
			Expect(trace["explain"]).To(BeEmpty())
		}
	})
})

var _ = Describe("FixedFactorUnionAccessJava", func() {
	DescribeTable("records the bounded anti-EXISTS planning outcome", func(term, outcome string, taskLimit int) {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("unionaccess_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		sql := "SELECT * FROM t_rd WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (" + term + "))) AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9)"
		if term == "" {
			sql = "SELECT * FROM t_rd WHERE a = 1"
		}
		var trace map[string]any
		err = srv.InvokeAs(ctx, "planRuleTraceWithinBudget", map[string]any{
			"clusterFile":    env.ClusterFile,
			"schemaTemplate": "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)",
			"setupSqls":      []string{},
			"querySql":       sql,
			"rules":          []string{"TASK-COUNT", "REWRITING-RESULT", "NormalizePredicatesRule", "PredicateToLogicalUnionRule", "PartitionBinarySelectRule", "ImplementFilterRule"},
			"taskLimit":      taskLimit,
		}, &trace)
		fmt.Fprintf(GinkgoWriter, "UNIONACCESS term=%q trace=%v error=%v\n", term, trace, err)
		Expect(err).NotTo(HaveOccurred())
		Expect(trace["traceTaskLimit"]).To(Equal(float64(taskLimit)))
		tasks := 0.0
		for _, count := range trace["tasksPerPhase"].(map[string]any) {
			tasks += count.(float64)
		}
		switch outcome {
		case "unsupported":
			Expect(trace["traceTaskLimitReached"]).To(BeFalse())
			Expect(trace["sqlState"]).To(Equal("0AF00"))
			Expect(trace["error"]).To(ContainSubstring("Unsupported operator ABS"))
			Expect(tasks).To(BeZero())
		case "stack":
			Expect(trace["traceTaskLimitReached"]).To(BeFalse())
			Expect(trace["exceptionClass"]).To(Equal("StackOverflowError"))
			Expect(tasks).To(BeNumerically(">", 0))
		case "budget":
			Expect(trace["traceTaskLimitReached"]).To(BeTrue())
			Expect(tasks).To(Equal(float64(taskLimit)))
			Expect(trace).NotTo(HaveKey("exceptionClass"), "a probe stop is not a Java planning error")
		case "planned":
			Expect(trace["traceTaskLimitReached"]).To(BeFalse())
			Expect(trace["explain"]).NotTo(BeEmpty())
			Expect(tasks).To(BeNumerically(">", 0))
			Expect(tasks).To(BeNumerically("<", taskLimit))
		default:
			Fail("unrecognized oracle outcome")
		}
		if outcome != "planned" {
			Expect(trace["explain"]).To(BeEmpty())
		}
	},
		Entry("absolute value", "ABS(c) = 4", "unsupported", 5000),
		Entry("equivalent disjunction", "c = 4 OR c = -4", "stack", 5000),
		Entry("single comparison", "c = 4", "budget", 5000),
		Entry("single comparison at Go budget", "c = 4", "budget", 150000),
		Entry("completed control", "", "planned", 5000),
		Entry("stopped control", "", "budget", 1),
	)
})

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
			// Engine-to-engine, not both-to-a-literal. The two comparisons above go
			// through fmt.Sprint, which equates float64(10) with "10"; this one is
			// exact, so a divergence in how the two engines TYPE the same answer
			// cannot hide behind a shared rendering.
			Expect(gr.Rows.Rows).To(Equal(jr.Rows.Rows),
				"%s: the engines render alike but their values differ in TYPE.", p.name)
		}
	})
})
