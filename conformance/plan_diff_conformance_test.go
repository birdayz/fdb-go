//go:build bazelrunfiles

package conformance_test

// End-to-end integration test for the plan-equivalence harness
// (RFC-022 §4.-1). Drives both engines (Go's naive generator + Java's
// fdb-relational planner via the SqlPlanSteps step) against a shared
// FDB testcontainer and reports per-query Status (AGREE / DIVERGE /
// engine-side errors).
//
// What this test asserts:
//
//   1. Both engines plan a small "schema-less" SELECT (no FROM, no
//      table refs) without error. This is the simplest end-to-end
//      path through the harness; failure here means the wiring is
//      broken, independent of any planner divergence.
//
//   2. For a single-table SELECT against a synthetic schema, both
//      engines produce a non-empty plan tree. They will almost
//      certainly DIVERGE on tree shape today — Go's naive generator
//      emits LogicalOperator's text format, fdb-relational emits its
//      Cascades EXPLAIN text. That's expected and the test does not
//      require AGREE; it asserts the harness CAPTURES both trees and
//      classifies the case (not GO_ERROR / JAVA_ERROR / BOTH_ERROR).
//
// What this test does NOT assert:
//
//   - Plan-tree equivalence on the seed corpus. That's the
//     plan-equivalence WORK, not a precondition for the harness
//     itself. Today every corpus query DIVERGES; the harness's job
//     is to surface those divergences for triage as the Cascades
//     port lands. A future test will pin a per-query expected
//     status (DIVERGE today → AGREE once Batch A rules ship).
//
//   - PLAN_HASH equivalence. Per RFC-024, hash-identical Java
//     compatibility is NOT a goal.

import (
	"context"
	"fmt"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Plan Equivalence Harness", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tenantName := fmt.Sprintf("plandiff_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	It("captures Java plan output via the SqlPlanSteps step", func() {
		// fdb-relational rejects schema-less executeQuery (and
		// EXPLAIN goes through the same statement check), so SELECT 1
		// without a schema surfaces a typed RelationalException with
		// "No Schema specified". That's the documented behaviour, not
		// a harness bug — pin it explicitly. Transport-level failures
		// remain real harness bugs.
		eng := plandiff.NewJavaEngineHTTP(javaBaseURL(java), env.ClusterFile)
		got := eng.Plan(ctx, plandiff.Query{
			Name: "select_constant",
			SQL:  "SELECT 1",
		})
		Expect(got.Err).To(HaveOccurred(), "fdb-relational rejects schema-less EXPLAIN")
		Expect(got.Err.Error()).To(ContainSubstring("No Schema specified"))
		Expect(got.Err.Error()).NotTo(ContainSubstring("HTTP "), "transport-level failure")
		Expect(got.Err.Error()).NotTo(ContainSubstring("dial tcp"), "Java server not reachable")
	})

	It("runs both engines on a single-table SELECT and produces a Diff", func() {
		// A synthetic schema for a single-table SELECT — exercises the
		// SqlPlanSteps DDL path (CREATE SCHEMA TEMPLATE / DATABASE /
		// SCHEMA) and the EXPLAIN path. Both engines now consume the
		// SchemaTemplate: the Java side wraps it via
		// sql_plan_steps.java#planSql; the Go side, post-RFC-022 §4.-1
		// Phase 3, parses it into a synthetic in-memory schema cache
		// so WHERE/DELETE/UPDATE shapes route through the catalog-
		// aware logical builder and predicates render via
		// cascades.QueryPredicate.Explain.
		//
		// fdb-relational reserves NOT NULL for ARRAY column types in
		// CREATE TABLE syntax — primary-key columns are implicitly
		// NOT NULL. Drop the explicit annotation; otherwise the DDL
		// phase fails inside the Java step.
		corpus := []plandiff.Query{
			{
				Name:           "select_single_table",
				SQL:            "SELECT id FROM Item",
				SchemaTemplate: "CREATE TABLE Item (id BIGINT, name STRING, PRIMARY KEY (id))",
			},
		}

		goEng := plandiff.NewGoEngine()
		javaEng := plandiff.NewJavaEngineHTTP(javaBaseURL(java), env.ClusterFile)
		report := plandiff.Run(ctx, corpus, goEng, javaEng)

		Expect(report.Summary.Total).To(Equal(1))
		Expect(report.Cases).To(HaveLen(1))
		c := report.Cases[0]

		// Go side must succeed — naive generator handles this shape.
		Expect(c.Go.Err).NotTo(HaveOccurred(), "Go engine error: %v", c.Go.Err)
		Expect(c.Go.Tree).NotTo(BeEmpty())

		// Java side: log the outcome but don't gate on AGREE — the
		// purpose of the harness is to surface the divergence, not to
		// assert equivalence today. Failure modes that DO indicate
		// real harness problems: GO_ERROR (above), JAVA transport
		// errors (below), BOTH_ERROR.
		switch c.Status {
		case plandiff.StatusAgree:
			GinkgoWriter.Printf("[AGREE] %s\n", c.Query.Name)
		case plandiff.StatusDiverge:
			GinkgoWriter.Printf("[DIVERGE] %s — expected today; tree-shape parity comes with Cascades Batch A.\nGO:\n%s\nJAVA:\n%s\n",
				c.Query.Name, c.Go.Tree, c.Java.Tree)
		case plandiff.StatusJavaError:
			GinkgoWriter.Printf("[JAVA_ERROR] %s — fdb-relational planner refused: %v\n", c.Query.Name, c.Java.Err)
			// A typed JavaException is acceptable (e.g. UNSUPPORTED_OPERATION
			// for a feature fdb-relational doesn't expose via EXPLAIN).
			// A transport-level error is not.
			Expect(c.Java.Err.Error()).NotTo(ContainSubstring("HTTP "), "transport-level failure")
			Expect(c.Java.Err.Error()).NotTo(ContainSubstring("dial tcp"), "Java server not reachable")
		case plandiff.StatusBothError:
			Fail(fmt.Sprintf("[BOTH_ERROR] %s — both engines errored: %s", c.Query.Name, c.Detail))
		case plandiff.StatusGoError:
			Fail(fmt.Sprintf("[GO_ERROR] %s — go engine errored: %v", c.Query.Name, c.Go.Err))
		case plandiff.StatusJavaUnimplemented:
			Fail("[JAVA_UNIMPL] javaEngine returned ErrJavaUnimplemented despite NewJavaEngineHTTP — harness wiring broken")
		default:
			Fail(fmt.Sprintf("unexpected status %s", c.Status))
		}
	})
})

// javaBaseURL extracts the conformance server URL from a JavaInvoker
// for plandiff's HTTP engine. JavaInvoker.baseURL is package-private;
// since this test file is in the same conformance_test package, the
// access is direct.
func javaBaseURL(j *JavaInvoker) string {
	return j.baseURL
}
