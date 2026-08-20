//go:build bazelrunfiles

package conformance_test

// What happens when a searched CASE's WHEN condition is not boolean?
//
// Java answers this by construction: visitCaseFunctionCall asserts the resolved
// condition's type is BOOLEAN before building anything
// (ExpressionVisitor.java:411-412), raising DATATYPE_MISMATCH. `CASE WHEN 1
// THEN 'a' ELSE 'b' END` is therefore a rejected query, not a query with a
// surprising answer.
//
// Go answered 'q' for every row instead. walkCaseCondition asks WalkPredicate
// first and, on any error, fell back to a value walk. WalkPredicate's bare-value
// lift DOES raise DATATYPE_MISMATCH for a definitively-typed non-boolean —
// Go has Java's check — but the fallback swallowed it: the value walk then
// succeeded, the integer became the condition, it compared unequal to TRUE, and
// every row took the ELSE branch.
//
// MEASURED, six shapes, before the repair. Java: "argument of case when must be
// of boolean type". Go: [[1 q] [2 q]] for all six.
//
//	CASE WHEN 1 THEN 'p' ELSE 'q' END        integer literal
//	CASE WHEN 0 …                            zero, in case truthiness was the story
//	CASE WHEN a …                            integer column
//	CASE WHEN 'x' … / CASE WHEN s …          string literal and column
//	CASE WHEN a + 1 …                        arithmetic
//
// The repair distinguishes the two failure kinds instead of treating every
// error as a decline: an UnsupportedExpressionShapeError means "no predicate
// reading of this shape exists" and the value walk may have one, while a
// DATATYPE_MISMATCH means "this IS a condition and its type is wrong" and the
// value walk has the same wrong value. Only the first falls through.
//
// THE DIRECTION IS WHY THIS MATTERED MORE THAN IT LOOKED. Go refusing what Java
// accepts is a reach gap: loud, and the user sees it immediately. Go ACCEPTING
// what Java rejects is the silent kind — the query runs, returns rows, and the
// two engines quietly disagree about what those rows mean. It is the same shape
// as the parenthesized-condition defect repaired just before it, where a
// plausible-looking zero was the whole symptom.
//
// NULL is the one arm that stays permissive, and it is not an oversight: see
// the comment on that case.
//
// The boolean-column and comparison arms are the controls. They must be
// accepted by both engines — if they are not, this fixture is measuring
// something broken about CASE itself and the non-boolean rows below cannot be
// read.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CaseConditionTypingJavaProbe", func() {
	It("measures both engines on a non-boolean CASE condition", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("casetype_%s", uuid.New().String())
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

		const schema = "CREATE TABLE t (id BIGINT, a BIGINT, s STRING, f BOOLEAN, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO t (id,a,s,f) VALUES (1,1,'x',true),(2,0,'y',false)"}

		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}
		rejects := func(s string) bool { return strings.HasPrefix(s, "ERR(") }

		cases := []struct {
			name string
			sql  string
			// control marks the arms both engines must ACCEPT, without which
			// nothing else here is interpretable.
			control bool
			// bothReject marks the arms measured as rejected by Java AND, since
			// the repair, by Go. Before it Go answered 'q' for every row of each
			// of these — a rejected query turned into a silently wrong one.
			bothReject bool
		}{
			{
				name: "comparison condition", control: true,
				sql: "SELECT id, CASE WHEN a = 1 THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
			{
				name: "boolean column condition", control: true,
				sql: "SELECT id, CASE WHEN f THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
			// --- the non-boolean conditions.
			{
				bothReject: true,
				name:       "integer literal condition",
				sql:        "SELECT id, CASE WHEN 1 THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
			{
				bothReject: true,
				name:       "zero literal condition",
				sql:        "SELECT id, CASE WHEN 0 THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
			{
				bothReject: true,
				name:       "integer COLUMN condition",
				sql:        "SELECT id, CASE WHEN a THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
			{
				bothReject: true,
				name:       "string literal condition",
				sql:        "SELECT id, CASE WHEN 'x' THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
			{
				bothReject: true,
				name:       "string COLUMN condition",
				sql:        "SELECT id, CASE WHEN s THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
			{
				bothReject: true,
				name:       "arithmetic condition",
				sql:        "SELECT id, CASE WHEN a + 1 THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
			{
				// MEASURED, and not what this arm was written expecting. A NULL
				// condition is type-compatible with boolean in SQL and a
				// non-TRUE condition takes the ELSE branch, so this was drafted
				// as a control both engines would accept. Java REJECTS it with
				// the same "must be of boolean type" as the arms above — a NULL
				// literal's type is not BOOLEAN to its check.
				//
				// So Go answering 'q' here is standard-correct and Java is
				// stricter, which is the opposite direction from the arms above
				// and is why NULL is deliberately left working: the lift folds a
				// NULL to an unknown ConstantPredicate BEFORE the type switch
				// that raises the mismatch. It is the same permissive divergence
				// as the parenthesized condition, and the same open owner
				// decision.
				name: "NULL condition",
				sql:  "SELECT id, CASE WHEN NULL THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			},
		}

		var divergent []string
		var controls int
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			mark := "  "
			if rejects(javaOut) != rejects(goOut) || javaOut != goOut {
				mark = "!!"
				divergent = append(divergent, fmt.Sprintf("%s (javaRejects=%v goRejects=%v)\n"+
					"    java: %s\n    go  : %s\n    sql : %s",
					c.name, rejects(javaOut), rejects(goOut), javaOut, goOut, c.sql))
			}
			fmt.Fprintf(GinkgoWriter, "%s %-28s java=%-52s go=%s\n", mark, c.name, javaOut, goOut)

			if c.control {
				Expect(rejects(goOut)).To(BeFalse(),
					"%s is a CONTROL and Go rejected it, so this fixture is not measuring "+
						"non-boolean conditions — it is measuring something broken about CASE\n"+
						"  go: %s\n  sql: %s", c.name, goOut, c.sql)
				Expect(rejects(javaOut)).To(BeFalse(),
					"%s is a CONTROL and Java rejected it\n  java: %s\n  sql: %s", c.name, javaOut, c.sql)
				Expect(goOut).To(Equal(javaOut),
					"%s is a CONTROL and the engines disagree on it\n  java: %s\n  go: %s",
					c.name, javaOut, goOut)
			}
			if c.bothReject {
				controls++
				Expect(rejects(javaOut)).To(BeTrue(),
					"%s: Java now ACCEPTS a non-boolean CASE condition. Its BOOLEAN assert is what "+
						"Go's rejection is aligned with, so that alignment has become a divergence: "+
						"re-measure and decide which engine to follow.\n  java: %s\n  sql : %s",
					c.name, javaOut, c.sql)
				Expect(rejects(goOut)).To(BeTrue(),
					"%s: Go ACCEPTED a non-boolean CASE condition where Java rejects it. That is the "+
						"silent direction — the query runs, every row takes the ELSE branch, and "+
						"nothing says the condition was meaningless. walkCaseCondition must propagate "+
						"a DATATYPE_MISMATCH from the predicate walk instead of falling through to "+
						"the value walk.\n  go: %s\n  sql : %s", c.name, goOut, c.sql)
			}
		}

		fmt.Fprintf(GinkgoWriter, "\nDIVERGENT: %d of %d\n", len(divergent), len(cases))
		for _, d := range divergent {
			fmt.Fprintf(GinkgoWriter, "%s\n", d)
		}
		// The vacuity floor. Every assertion above is inside an `if`, so a
		// future edit that cleared the flags — or a `cases` slice that lost its
		// non-boolean arms — would leave this spec passing having asserted
		// nothing about the thing it is named for.
		Expect(controls).To(Equal(6),
			"the non-boolean population changed: %d arms were checked, not the 6 this file "+
				"measured against the live JVM. A green from a shrunken set says nothing", controls)
	})
})
