//go:build bazelrunfiles

package conformance_test

// Measures BOTH engines on a searched CASE whose condition is PARENTHESIZED,
// and asserts they agree on the rows.
//
// Go used to answer these wrongly, and silently. This grammar — ported from
// Java's — has no parenthesized-expression alternative in expressionAtom, so
// the only rule matching `( expr )` is recordConstructor, a single-element
// record. Go's searched CASE resolved its condition as a VALUE first and so
// accepted that record as the condition, comparing a RECORD with TRUE: never
// equal, every row took the ELSE branch. The repair resolves the condition as a
// predicate first, which is what a searched CASE's WHEN is.
//
// WHAT IT MEASURED. Java REJECTS every parenthesized condition with SQLSTATE
// 42804 (datatype mismatch) — its visitCaseFunctionCall asserts the condition is
// BOOLEAN and the record is not one. That holds for the SIMPLE `(a = 1)` as much
// as for `(a = 1 AND b = 1)`; Java draws no distinction, because to Java both are
// records. Go accepts all of them and now answers all of them correctly.
//
// So the shapes here sit in the direction the harness already names
// DivergenceJavaErrorsGoCorrect: Java errors, Go returns the right rows. This
// file therefore asserts the MEASURED STATE rather than agreement — the
// alternative reading, that Go should reject these too for strict parity, is a
// live option and an owner's call, not something to settle by writing the
// assertion one way and moving on. The pin fails if EITHER engine moves: if Java
// starts accepting, the divergence is closing and this becomes an agreement
// test; if Go starts rejecting or mis-evaluating, the repair regressed or the
// parity decision was taken.
//
// The prior behaviour is what makes leaving Go permissive defensible for now:
// before the repair Go accepted these too, so this is not a widening of the
// accepted surface. It only changed WRONG answers into right ones. Narrowing to
// Java's rejection would be a separate, deliberate change.
//
// ROWS are compared, not just accept/reject: the defect produced perfectly
// well-formed answers with the wrong values in them, which an accept/reject
// probe cannot see.

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

var _ = Describe("CaseParenthesizedConditionJavaProbe", func() {
	It("measures both engines on parenthesized searched-CASE conditions", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("caseparen_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		schema := "CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, f BOOLEAN, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T VALUES (1, 1, 1, TRUE), (2, 1, 2, FALSE), (3, 2, 1, TRUE), (4, NULL, 9, NULL)",
		}

		describe := func(r plandiff.RunResult) string {
			if r.Err == nil {
				return "ACCEPT rows=" + fmt.Sprint(r.Rows.Rows)
			}
			var je *plandiff.JavaError
			if errors.As(r.Err, &je) {
				if je.SQLState == "" {
					return "REJECT(msg:" + je.Message + ")"
				}
				return "REJECT(" + je.SQLState + ")"
			}
			var ge *api.Error
			if errors.As(r.Err, &ge) {
				return "REJECT(" + string(ge.Code) + ")"
			}
			return "REJECT(?:" + r.Err.Error() + ")"
		}

		// Each pair is the SAME condition, bare and parenthesized. The bare form
		// is the control: if the engines disagree on it, the disagreement is not
		// about parentheses and this probe is measuring the wrong thing.
		type pair struct{ name, bare, wrapped string }
		pairs := []pair{
			{"and", "a = 1 AND b = 1", "(a = 1 AND b = 1)"},
			{"or", "a = 1 OR b = 1", "(a = 1 OR b = 1)"},
			{"not", "NOT (a = 1)", "(NOT (a = 1))"},
			{"simple", "a = 1", "(a = 1)"},
			{"bool_col", "f", "(f)"},
			{"three_way", "a = 1 AND b = 1 AND id = 1", "(a = 1 AND b = 1 AND id = 1)"},
			{"null_arm", "a IS NULL OR b = 9", "(a IS NULL OR b = 9)"},
		}

		// BARE conditions must AGREE — that is the control. A disagreement there
		// is not about parentheses, and would mean this probe is measuring
		// something other than what it claims.
		var bareDisagreements []string
		// WRAPPED conditions are the measured divergence: Java rejects 42804,
		// Go accepts and must return the SAME rows as its own bare spelling,
		// since parentheses do not change meaning.
		var wrappedUnexpected []string

		for _, p := range pairs {
			bareSQL := fmt.Sprintf("SELECT id, CASE WHEN %s THEN 1 ELSE 0 END FROM T ORDER BY id", p.bare)
			wrapSQL := fmt.Sprintf("SELECT id, CASE WHEN %s THEN 1 ELSE 0 END FROM T ORDER BY id", p.wrapped)

			javaBare := describe(javaRunner.RunWithSetup(ctx, schema, setup, bareSQL))
			goBare := describe(goRunner.RunWithSetup(ctx, schema, setup, bareSQL))
			if javaBare != goBare {
				bareDisagreements = append(bareDisagreements,
					fmt.Sprintf("%s:\n    java: %s\n    go  : %s\n    sql : %s", p.name, javaBare, goBare, bareSQL))
			}

			javaWrap := describe(javaRunner.RunWithSetup(ctx, schema, setup, wrapSQL))
			goWrap := describe(goRunner.RunWithSetup(ctx, schema, setup, wrapSQL))

			if !strings.HasPrefix(javaWrap, "REJECT(42804)") {
				wrappedUnexpected = append(wrappedUnexpected, fmt.Sprintf(
					"%s: JAVA no longer rejects a parenthesized condition with 42804 (got %s).\n"+
						"    The divergence is closing: re-read whether Go and Java now agree, and if so "+
						"convert this to an agreement assertion.\n    sql: %s", p.name, javaWrap, wrapSQL))
			}
			// Go must accept AND must answer exactly what its own bare spelling
			// answers. This is the arm that caught the defect: before the repair
			// the wrapped form returned all-ELSE while the bare form was right.
			if goWrap != goBare {
				wrappedUnexpected = append(wrappedUnexpected, fmt.Sprintf(
					"%s: GO answers differently with and without the parentheses.\n"+
						"    bare   : %s\n    wrapped: %s\n"+
						"    Parentheses group a condition; they do not change it. Either the searched-CASE "+
						"repair regressed, or Go was deliberately narrowed to Java's rejection — in which "+
						"case this pin needs re-arming to expect REJECT on both engines.\n    sql: %s",
					p.name, goBare, goWrap, wrapSQL))
			}
			fmt.Fprintf(GinkgoWriter, "   %-16s bare    java=%-40s go=%s\n", p.name, javaBare, goBare)
			fmt.Fprintf(GinkgoWriter, "   %-16s wrapped java=%-40s go=%s\n", p.name, javaWrap, goWrap)
		}

		Expect(bareDisagreements).To(BeEmpty(),
			"the engines disagree on an UNPARENTHESIZED searched-CASE condition, which is this probe's "+
				"control — the question it asks is about parentheses, and a control failure means it is "+
				"measuring something else.\n"+strings.Join(bareDisagreements, "\n"))
		Expect(wrappedUnexpected).To(BeEmpty(), strings.Join(wrappedUnexpected, "\n"))

		// The aggregate form, where the defect was a wrong NUMBER rather than a
		// wrong column and so is the shape a reader is least likely to notice.
		// Compared Go-to-Go across the two spellings, for the same reason as
		// above: Java rejects the wrapped one outright.
		aggBare := describe(goRunner.RunWithSetup(ctx, schema, setup,
			"SELECT SUM(CASE WHEN a = 1 AND b = 1 THEN 1 ELSE 0 END) FROM T"))
		aggWrapped := describe(goRunner.RunWithSetup(ctx, schema, setup,
			"SELECT SUM(CASE WHEN (a = 1 AND b = 1) THEN 1 ELSE 0 END) FROM T"))
		fmt.Fprintf(GinkgoWriter, "\n=== aggregate over the CASE\n   bare   : %s\n   wrapped: %s\n",
			aggBare, aggWrapped)
		Expect(aggWrapped).To(Equal(aggBare),
			"an aggregate over a CASE answers differently with and without parentheses around the "+
				"condition — the defect this file covers, in the form where it reads as a plausible number")
	})
})
