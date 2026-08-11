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
			// THE PARENTHESISED TWIN — a MEASURED divergence, pinned in the
			// direction it actually runs rather than assumed either way.
			//
			// Go builds a RecordConstructorValue for `(amount+1)` and a bare
			// ArithmeticValue for `amount+1`, and that is CORRECT: Java's
			// visitRecordConstructor does not unwrap a one-element constructor
			// either (ExpressionVisitor.java:918-925), which is why
			// walkRecordConstructorInner deliberately does not. The natural
			// inference — that Java therefore sees the same asymmetry and also
			// declines to refuse — is WRONG, and only the live JVM could say so.
			//
			// Java refuses anyway, and its message says why: `Ambiguous columns
			// for q…._0.AMOUNT + @c12` names the UNWRAPPED arithmetic. Java's
			// guard is Expressions.pullUp over FieldPaths (Expressions.java:112),
			// which descends THROUGH the record wrapper and finds the same
			// derivation twice. The wrapper asymmetry is real on both sides; only
			// Java's guard looks past it.
			//
			// So this is not a normalization divergence. It is Go's duplicate
			// gate comparing whole Values where Java's pull-up compares
			// derivations — booked in TODO.md Phase 12.
			{"paren_twin_aggonly", "SELECT COUNT(*) FROM T_G1 GROUP BY (amount+1), amount+1", "java_42702_go_plans"},
			{"paren_twin_proj", "SELECT amount+1, COUNT(*) FROM T_G1 GROUP BY (amount+1), amount+1", "java_42702_go_plans"},
			{"paren_twin_having", "SELECT COUNT(*) FROM T_G1 GROUP BY (amount+1), amount+1 HAVING amount+1 > 11", "java_42702_go_plans"},
			// The COMPARISON spelling diverges the same way, by a second and
			// independent route: Go's gate resolves with WalkExpression
			// (posPredicate), which will not fold a comparison to a Value at all,
			// so identity falls to a GetText compare the parentheses defeat.
			{"cmp_twin", "SELECT COUNT(*) FROM T_G1 GROUP BY (amount=1), amount=1", "java_42702_go_plans"},
			// THE CONTROLS that keep the three above honest: with the spellings
			// made to coincide, Go's gate DOES refuse, so what is pinned is the
			// mixed-spelling hole and not a claim that the gate never fires.
			{"paren_both_sides", "SELECT COUNT(*) FROM T_G1 GROUP BY (amount+1), (amount+1)", "both_42702"},
			{"cmp_same_text", "SELECT COUNT(*) FROM T_G1 GROUP BY amount=1, amount=1", "both_42702"},
			// THE JOIN SPELLING — the OTHER route (name-keyed, plain FieldValue
			// keys), measured here rather than inferred from the single-source
			// `dup_qualified_vs_bare` analogue. Go's duplicate gate strips a
			// leading alias only when the query has NO joins
			// (visitSelectGroupBy's `len(fs.joins) == 0`), so under a join
			// `a.amount` and bare `amount` compare as different names.
			// `amount` exists ONLY in T_G1, so the bare reference is
			// unambiguous and cannot be refused for an unrelated reason — using
			// `category`, which both tables declare, would confound this.
			{"join_qualified_vs_bare", "SELECT COUNT(*) FROM T_G1 a JOIN T_G2 b ON a.category = b.category GROUP BY a.amount, amount", "java_42702_go_plans"},
			{"join_control_single_source", "SELECT COUNT(*) FROM T_G1 a GROUP BY a.amount, amount", "both_42702"},
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
			case "java_42702_go_plans":
				// A PINNED DIVERGENCE, asserted in BOTH directions so neither
				// side can move unnoticed. Java must keep refusing (if it stops,
				// Go is conformant and the Phase 12 entry is wrong); Go must keep
				// planning (if it starts refusing, the fix landed and these move
				// to both_42702).
				if jr.Err == nil || !strings.Contains(errMsg(jr), "Ambiguous columns for") {
					divergences = append(divergences, fmt.Sprintf(
						"probe %s: Java NO LONGER refuses the parenthesised duplicate.\n"+
							"  That would make Go's current behaviour CONFORMANT and the TODO.md\n"+
							"  Phase 12 booking wrong — re-measure before changing Go.\n  java: %s",
						p.name, render("JAVA", jr)))
				}
				if gr.Err != nil {
					divergences = append(divergences, fmt.Sprintf(
						"probe %s: Go now REFUSES, so the divergence is closed.\n"+
							"  Move this probe to both_42702 and flip the Go-side arms in\n"+
							"  sqldriver/groupby_computed_key_having_fdb_test.go.\n  go: %s",
						p.name, render("GO  ", gr)))
				}
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
