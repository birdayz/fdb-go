//go:build bazelrunfiles

package conformance_test

// Measures BOTH engines on the positions where a boolean-valued expression can
// appear as an operand, and ASSERTS the two agree.
//
// It exists because a Go code comment justified a scoping decision with the
// claim "Java rejects `WHERE CASE WHEN ... THEN a < b`" and no measurement or
// citation stood behind it. The Java source contradicts the claim on its face:
// ExpressionVisitor.visitCaseFunctionCall asserts BOOLEAN on the CONDITION only
// (`argument of case when must be of boolean type`), feeds the consequent
// through visitFunctionArg with no type check at all, and hands the
// alternatives to __pick_value → PickValue.PickValueFn, which folds them with
// Type.maximumType and rejects only when no common type exists. Upstream's own
// yamsql corpus has no comparison consequent anywhere, so the claim was not
// sourced there either.
//
// A claim about another engine's behaviour is either measured or it is
// folklore, and folklore hardened into a test becomes an asserted parity
// invariant that nobody can question. This probe is the measurement. It
// asserts agreement rather than merely printing, so the day either engine
// moves, the build says so and names the shape.
//
// WHAT IT MEASURED, the first time it ran. Java ACCEPTS a comparison
// consequent; Go rejected it with 0AF00 — the claim was inverted, and four
// tests across the driver suite and the yamsql corpus had hardened the
// inversion into pins (two of them describing it as a tracked parity gap
// waiting on a port that turned out not to be needed). Java also REJECTS every
// ordering comparison over BOOLEAN — `f > FALSE`, `f >= FALSE`,
// `f BETWEEN FALSE AND TRUE` — which Go accepted, returning rows no Java
// client could ever see. Both are now fixed and pinned; this probe is what
// keeps them honest.
//
// The CASE consequent and the paren-wrapped operand shapes are deliberately
// SEPARATE groups: they are different questions. The consequent question is
// about CASE's type rules; the operand question is about how `(a > 3)` is
// analyzed at all, since the grammar has no parenthesized-expression
// alternative in expressionAtom — the only rule matching it is
// recordConstructor, a single-element record.

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

// outcome is the comparable shape of one engine's answer: whether it accepted,
// and if not, the SQLSTATE. Row VALUES are deliberately excluded — the engines
// encode numbers differently over their respective transports, and this probe
// is about which shapes are ACCEPTED, not about row equality (rowdiff and the
// factory corpus own that).
type boolPosOutcome struct {
	accepted bool
	sqlState string
}

func (o boolPosOutcome) String() string {
	if o.accepted {
		return "ACCEPT"
	}
	return "REJECT(" + o.sqlState + ")"
}

var _ = Describe("BooleanExpressionPositionJavaProbe", func() {
	It("measures both engines on boolean-valued operand positions", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("boolpos_%s", uuid.New().String())
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

		schema := "CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, s STRING, f BOOLEAN, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T VALUES (1, 1, 10, 'x', TRUE), (2, 5, 20, 'y', FALSE), (3, NULL, 30, NULL, NULL), " +
				"(4, 9, 40, 'z', TRUE), (5, NULL, 50, 'w', FALSE)",
		}

		classify := func(r plandiff.RunResult) boolPosOutcome {
			if r.Err == nil {
				return boolPosOutcome{accepted: true}
			}
			var je *plandiff.JavaError
			if errors.As(r.Err, &je) {
				// Some Java rejections arrive with an empty SQLSTATE. Fall back
				// to the message so a reject is never reported as an anonymous
				// "REJECT()" that two different causes could both produce.
				if je.SQLState == "" {
					return boolPosOutcome{sqlState: "msg:" + je.Message}
				}
				return boolPosOutcome{sqlState: je.SQLState}
			}
			var ge *api.Error
			if errors.As(r.Err, &ge) {
				return boolPosOutcome{sqlState: string(ge.Code)}
			}
			return boolPosOutcome{sqlState: "?:" + r.Err.Error()}
		}

		type probe struct{ name, sql string }
		groups := []struct {
			label  string
			probes []probe
		}{
			{
				// The claim under test, plus a control. If the control
				// disagrees the harness is broken, not the engines.
				label: "CASE consequent",
				probes: []probe{
					{"case_int_consequent_control", "SELECT CASE WHEN id = 1 THEN 1 ELSE 0 END FROM T"},
					{"case_comparison_consequent", "SELECT CASE WHEN id = 1 THEN a > 3 ELSE FALSE END FROM T"},
				},
			},
			{
				// The operand positions Java resolves with one shared
				// visit(ctx.expressionAtom()) across its IS / IN / BETWEEN /
				// binary-comparison arms.
				label: "parenthesised comparison as operand",
				probes: []probe{
					{"is_null", "SELECT id FROM T WHERE (a > 3) IS NULL"},
					{"eq_comparison", "SELECT id FROM T WHERE (a > 3) = (b > 1)"},
					{"eq_literal", "SELECT id FROM T WHERE (a > 3) = TRUE"},
					{"is_distinct_from", "SELECT id FROM T WHERE (a > 3) IS DISTINCT FROM (b > 1)"},
					{"in_list", "SELECT id FROM T WHERE (a > 3) IN (TRUE, FALSE)"},
					{"between", "SELECT id FROM T WHERE (a > 3) BETWEEN FALSE AND TRUE"},
					{"projected_paren", "SELECT (a > 3) FROM T"},
					{"projected_bare", "SELECT a > 3 FROM T"},
				},
			},
			{
				// Positions where a boolean operand is a TYPE ERROR in both
				// engines. Pinned so a future widening of the operand walk
				// cannot quietly start accepting them.
				//
				// ACCEPT/REJECT agrees; the error CLASS does not, and that gap
				// is open. Measured Java wording, for whoever closes it:
				// arithmetic → "unable to encapsulate arithmetic operation due
				// to type mismatch(es)"; LIKE → "The like operator expects
				// string operands but was invoked with an operand of another
				// type." Go answers both with 0AF00 (unsupported shape) because
				// the operand walk declines a comparison here and the decline
				// surfaces as unsupported rather than as a type mismatch.
				// Contrast the ordering group below, where Go already emits
				// Java's exact message.
				label: "boolean operand where it is a type error",
				probes: []probe{
					{"arithmetic", "SELECT id FROM T WHERE (a > 3) + 1 = 2"},
					{"like", "SELECT id FROM T WHERE (a > 3) LIKE 'x'"},
				},
			},
			{
				// ORDERING a boolean. BETWEEN desugars to `>= lo AND <= hi`, so
				// whether `(a > 3) BETWEEN FALSE AND TRUE` is legal is really the
				// question of whether BOOLEAN has an order — asked here directly,
				// on a bare boolean COLUMN as well as on a comparison, so the
				// answer cannot be confused with a record-constructor artifact.
				label: "ordering a boolean",
				probes: []probe{
					{"col_eq_literal", "SELECT id FROM T WHERE f = TRUE"},
					{"col_gt_literal", "SELECT id FROM T WHERE f > FALSE"},
					{"col_ge_literal", "SELECT id FROM T WHERE f >= FALSE"},
					{"col_between", "SELECT id FROM T WHERE f BETWEEN FALSE AND TRUE"},
					{"cmp_ge_literal", "SELECT id FROM T WHERE (a > 3) >= FALSE"},
					{"str_gt_control", "SELECT id FROM T WHERE s > 'x'"},
				},
			},
		}

		var disagreements []string
		for _, g := range groups {
			fmt.Fprintf(GinkgoWriter, "\n=== %s\n", g.label)
			for _, p := range g.probes {
				java := classify(javaRunner.RunWithSetup(ctx, schema, setup, p.sql))
				goSide := classify(goRunner.RunWithSetup(ctx, schema, setup, p.sql))
				mark := "  "
				if java.accepted != goSide.accepted {
					mark = "!!"
					disagreements = append(disagreements,
						fmt.Sprintf("%s: java=%s go=%s\n    %s", p.name, java, goSide, p.sql))
				}
				fmt.Fprintf(GinkgoWriter, "%s %-28s java=%-14s go=%-14s %s\n",
					mark, p.name, java, goSide, p.sql)
			}
		}

		Expect(disagreements).To(BeEmpty(), "the engines disagree on which boolean-operand positions are legal.\n"+
			"Each line is a conformance divergence: the shared query surface must accept and reject the same shapes.\n"+
			strings.Join(disagreements, "\n"))

		// A boolean-valued CASE used AS a WHERE predicate — the shape two
		// corpus scenarios pinned as an 0AF00 parity gap. Accept/reject is not
		// enough for those: restoring their row expectations means freezing
		// actual rows, so the rows are compared here, on both engines, rather
		// than computed by hand and hoped over.
		caseSchema := "CREATE TABLE PRODUCTS (id BIGINT, category BIGINT, price BIGINT, PRIMARY KEY (id))"
		caseSetup := []string{
			"INSERT INTO PRODUCTS VALUES (1, 10, 100), (2, 10, 200), (3, 10, 300), (4, 20, 150), " +
				"(5, 20, 250), (6, 30, 500), (7, 30, 700), (8, 40, 50)",
		}
		// The plain form, the CTE form (a different scope resolution path), and
		// the form the driver suite pinned as rejected.
		for _, caseSQL := range []string{
			"SELECT id FROM PRODUCTS WHERE CASE WHEN category = 10 THEN price > 150 ELSE price > 400 END ORDER BY id",
			// The CTE form exercises a different scope-resolution path into the
			// same CASE. Deliberately WITHOUT ORDER BY: Java's server rejects
			// `order by is not supported in subquery` for this shape, which is a
			// CTE limitation and would mask the CASE question being asked here.
			"WITH c AS (SELECT id, category, price FROM PRODUCTS) " +
				"SELECT id FROM c WHERE CASE WHEN category = 10 THEN price > 150 ELSE price > 400 END",
		} {
			javaRes := javaRunner.RunWithSetup(ctx, caseSchema, caseSetup, caseSQL)
			goRes := goRunner.RunWithSetup(ctx, caseSchema, caseSetup, caseSQL)
			fmt.Fprintf(GinkgoWriter, "\n=== boolean CASE as a WHERE predicate\n   java: %s rows=%v err=%v\n   go  : %s rows=%v\n   sql: %s\n",
				classify(javaRes), javaRes.Rows.Rows, javaRes.Err, classify(goRes), goRes.Rows.Rows, caseSQL)
			Expect(classify(goRes).accepted).To(Equal(classify(javaRes).accepted),
				"the engines disagree on whether a boolean-valued CASE may BE a WHERE predicate:\n  %s", caseSQL)
			Expect(fmt.Sprint(goRes.Rows.Rows)).To(Equal(fmt.Sprint(javaRes.Rows.Rows)),
				"the engines return different rows for a boolean-valued CASE as a WHERE predicate:\n  %s", caseSQL)
		}
	})
})
