//go:build bazelrunfiles

package conformance_test

// Measures Java's live behaviour (tag 4.12.11.0 conformance server) for
// DUPLICATE GROUP BY expressions and asserts Go matches it.
//
// Java's rejection point is Expressions.pullUp (Expressions.java:112): after
// LogicalOperator.generateGroupBy builds the GroupByExpression, the grouping
// expressions and aggregates are pulled up over its result value
// (LogicalOperator.java:454); a grouping value that contains the same
// expression twice maps that subexpression to TWO output columns, and the
// size()==1 assertion fails with AMBIGUOUS_COLUMN → 42702. The check is
// value-identity based, so it also fires when the duplicate is spelled
// differently (`category` vs `T.category` resolving to the same column) and
// does NOT fire for same-named columns of DIFFERENT sources (`a.k`, `b.k`).
//
// The upstream corpus witnesses this only through bitmap-aggregate-index
// negatives (blocked on the AS-SELECT index DDL); these plain-SQL shapes pin
// the same architectural point without that dependency.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DuplicateGroupByJavaProbe", func() {
	It("Go matches Java's live outcome for duplicate GROUP BY shapes", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("dupgroup_%s", uuid.New().String())
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

		schema := "CREATE TABLE T_G1 (id BIGINT, category STRING, amount BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE T_G2 (id BIGINT, category STRING, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T_G1 VALUES (1, 'a', 10), (2, 'a', 20), (3, 'b', 30)",
			"INSERT INTO T_G2 VALUES (7, 'a')",
		}

		// expect:
		//   "both_42702"  — both engines reject the duplicate grouping with
		//                   AMBIGUOUS_COLUMN (Java's message carries a
		//                   correlation-prefixed rendering; the shared
		//                   "Ambiguous columns for" wording is asserted).
		//   "go_extends"  — the duplicate-free CONTROL: live Java's Cascades
		//                   DECLINES a GROUP BY with no matching aggregate
		//                   index ("Cascades planner could not plan query"),
		//                   while Go's streaming aggregation serves it — the
		//                   sanctioned read-side extension (query reach, not
		//                   wire). Pinned so a change on either side is
		//                   noticed: the control proves Java's 42702 fires
		//                   BEFORE planning (the same shape without the
		//                   duplicate fails LATER, in the planner).
		probes := []struct{ name, sql, expect string }{
			{"dup_bare_with_agg", "SELECT category, COUNT(*) FROM T_G1 GROUP BY category, category", "both_42702"},
			{"dup_bare_agg_only", "SELECT COUNT(*) FROM T_G1 GROUP BY category, category", "both_42702"},
			{"dup_bare_no_agg", "SELECT category FROM T_G1 GROUP BY category, category", "both_42702"},
			{"dup_triple", "SELECT category, COUNT(*) FROM T_G1 GROUP BY category, category, category", "both_42702"},
			{"dup_qualified_vs_bare", "SELECT category, COUNT(*) FROM T_G1 GROUP BY category, T_G1.category", "both_42702"},
			{"dup_separated", "SELECT category, amount, COUNT(*) FROM T_G1 GROUP BY category, amount, category", "both_42702"},
			// EXPRESSION keys: Java's identity is the resolved value, so
			// the duplicate rejects regardless of source spelling — the
			// whitespace-variant spelling is the load-bearing shape (a
			// raw-text identity catches only the byte-identical twin).
			{"rev_expr_same_text", "SELECT amount+1, COUNT(*) FROM T_G1 GROUP BY amount+1, amount+1", "both_42702"},
			{"rev_expr_diff_space", "SELECT amount+1, COUNT(*) FROM T_G1 GROUP BY amount+1, amount + 1", "both_42702"},
			{"distinct_exprs_ok", "SELECT COUNT(*) FROM T_G1 GROUP BY amount+1, amount+2", "go_extends"},
			{"distinct_keys_ok", "SELECT category, amount, COUNT(*) FROM T_G1 GROUP BY category, amount", "go_extends"},
			{"single_key_ok", "SELECT category, COUNT(*) FROM T_G1 GROUP BY category", "go_extends"},
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
		errMsg := func(r plandiff.RunResult) string {
			if r.Err == nil {
				return ""
			}
			var je *plandiff.JavaError
			if errors.As(r.Err, &je) {
				return je.Message
			}
			var ge *api.Error
			if errors.As(r.Err, &ge) {
				return ge.Message
			}
			return r.Err.Error()
		}
		var divergences []string
		for _, p := range probes {
			jr := runner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			fmt.Fprintf(GinkgoWriter, "PROBE %s\n  %s\n  %s\n  sql: %s\n",
				p.name, render("JAVA", jr), render("GO  ", gr), p.sql)
			switch p.expect {
			case "both_42702":
				if jr.Err == nil || !strings.Contains(errMsg(jr), "Ambiguous columns for") {
					divergences = append(divergences, fmt.Sprintf("probe %s: Java no longer rejects the duplicate grouping\n  java: %s",
						p.name, render("JAVA", jr)))
				}
				var ge *api.Error
				if gr.Err == nil || !errors.As(gr.Err, &ge) || ge.Code != api.ErrCodeAmbiguousColumn ||
					!strings.Contains(ge.Message, "Ambiguous columns for") {
					divergences = append(divergences, fmt.Sprintf("probe %s: Go does not reject 42702\n  go: %s",
						p.name, render("GO  ", gr)))
				}
			case "go_extends":
				if jr.Err == nil || !strings.Contains(errMsg(jr), "could not plan") {
					divergences = append(divergences, fmt.Sprintf("probe %s: Java's planner decline no longer reproduces — re-measure\n  java: %s",
						p.name, render("JAVA", jr)))
				}
				if gr.Err != nil {
					divergences = append(divergences, fmt.Sprintf("probe %s: Go stopped serving the duplicate-free GROUP BY\n  go: %s",
						p.name, render("GO  ", gr)))
				}
			}
		}
		Expect(divergences).To(BeEmpty())
	})
})
