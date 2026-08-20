//go:build bazelrunfiles

package conformance_test

// Does a CHAINED `USING` resolve, or is its column ambiguous?
//
// `a JOIN b USING (id) JOIN c USING (k)` asks a question the first join has
// already complicated. Java's resolveJoinUsingClause marks the RIGHT copy of a
// USING column hidden, so after `USING (id)` only `b.id` is hidden — `a.k` and
// `b.k` both remain visible on the left. The second `USING (k)` then resolves
// `k` against every left operator and can find two of them.
//
// Go instead qualifies the second USING against the PRIOR JOIN'S RIGHT alias,
// picks `b.k`, and answers. If Java raises AMBIGUOUS_COLUMN there, then a Go
// fixture asserting rows for that query pins a divergence as though it were the
// specification — the failure mode is not a wrong answer but a test that
// enshrines one.
//
// This is measured rather than reasoned about because the reasoning above is
// exactly the kind that has been wrong twice on this branch: the alias chain
// was assumed correct until it was mutated, and the mutation showed it was.
//
// THE FIXTURE MAKES THE TWO READINGS DISAGREE ON ROWS, so an engine that
// resolves rather than rejects still reveals WHICH column it chose: a.k and b.k
// differ at id=2, so `k` meaning a.k yields two rows and `k` meaning b.k yields
// one. A test that could not tell them apart would report agreement whichever
// column either engine picked.

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

var _ = Describe("JoinUsingChainJavaProbe", func() {
	It("measures both engines on a chained USING whose column exists on both left sources", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("usingchain_%s", uuid.New().String())
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

		const schema = "CREATE TABLE a (id BIGINT, k BIGINT, j BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE b (id BIGINT, k BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE c (id BIGINT, k BIGINT, j BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO a (id,k,j) VALUES (1,10,5),(2,20,6),(3,30,7)",
			"INSERT INTO b (id,k) VALUES (1,10),(2,99),(3,30)",
			"INSERT INTO c (id,k,j) VALUES (1,10,5),(2,20,9),(3,77,9)",
		}

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
			// control marks arms both engines must accept and agree on.
			control bool
			// chained marks the arms this file exists to measure.
			chained bool
		}{
			{
				name: "single USING", control: true,
				sql: "SELECT COUNT(*) FROM a JOIN b USING (id)",
			},
			{
				name: "multi-column USING", control: true,
				sql: "SELECT COUNT(*) FROM a JOIN b USING (id, k)",
			},
			{
				// The chain whose second USING column is present on BOTH left
				// sources — the shape where Java's hidden-column rule and Go's
				// prior-right-alias rule can disagree.
				chained: true, name: "chained USING, second column on both left sources",
				sql: "SELECT a.id FROM a JOIN b USING (id) JOIN c USING (id, k) ORDER BY a.id",
			},
			{
				// The same chain where the FIRST join already equates k, so both
				// readings coincide. If the engines agree here and differ above,
				// the difference is about ambiguity and not about USING chains.
				// It also explains the rule: the first `USING (id, k)` HIDES the
				// right copy b.k, leaving only a.k visible, so the second USING
				// has exactly one candidate. Hiding is what makes one chain legal
				// and the other ambiguous.
				chained: true, name: "chained USING, first join already equates the column",
				sql: "SELECT a.id FROM a JOIN b USING (id, k) JOIN c USING (id, k) ORDER BY a.id",
			},
			{
				// THE FAR-LEFT CASE, measured now so the repair is designed once
				// rather than discovered twice. `j` exists ONLY on `a`, so the
				// second USING's unique candidate is the FIRST source, not the
				// prior join's right side. A rule that qualifies by the prior
				// right alias looks for b.j and cannot find it; Java resolves
				// against all visible left operators and finds a.j.
				chained: true, name: "chained USING whose column is only on the FIRST source",
				sql: "SELECT a.id FROM a JOIN b USING (id) JOIN c USING (j) ORDER BY a.id",
			},
		}

		var disagreed []string
		var chainedArms, controls int
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			mark := "  "
			if javaOut != goOut {
				mark = "!!"
			}
			fmt.Fprintf(GinkgoWriter, "%s %-52s java=%-40s go=%s\n", mark, c.name, javaOut, goOut)

			if c.control {
				controls++
				Expect(rejects(goOut)).To(BeFalse(),
					"%s is a CONTROL and Go rejected it\n  go: %s\n  sql: %s", c.name, goOut, c.sql)
				Expect(rejects(javaOut)).To(BeFalse(),
					"%s is a CONTROL and Java rejected it\n  java: %s\n  sql: %s", c.name, javaOut, c.sql)
				Expect(goOut).To(Equal(javaOut),
					"%s is a CONTROL and the engines disagree, so nothing below is readable\n"+
						"  java: %s\n  go: %s", c.name, javaOut, goOut)
			}
			if c.chained {
				chainedArms++
				// Full output, not just accept/reject: an engine that RESOLVES
				// rather than rejects still has to be shown choosing the same
				// column, and the fixture is built so the two choices differ.
				if javaOut != goOut {
					disagreed = append(disagreed, fmt.Sprintf(
						"%s\n    java: %s\n    go  : %s\n    sql : %s",
						c.name, javaOut, goOut, c.sql))
				}
			}
		}

		// TWO MEASURED DIVERGENCES, PINNED AS THEY STAND rather than asserted
		// away. Both come from ONE root cause: Go qualifies a chained USING by
		// the PRIOR JOIN'S RIGHT alias, while Java resolves the column against
		// every visible left operator with the earlier USING's right copy
		// hidden. That makes Go wrong in both directions at once —
		//
		//	USING (id) … USING (id, k)   java AMBIGUOUS_COLUMN   go picks b.k
		//	USING (id) … USING (j)       java answers            go 42703
		//
		// — and it means NO Java-agreeing query can exercise Go's rule: the
		// only chain the engines agree on is the one where the first USING
		// hides the column, and there the rule is unobservable. So the yamsql
		// scenario cannot pin this, and this probe is where it lives.
		//
		// The repair is a semantic USING resolution (unique visible left owner;
		// 42702 on two, 42703 on none), replacing the syntactic prior-alias
		// rule. It changes column-resolution semantics for every USING query,
		// so it is designed and reviewed before it is written rather than
		// patched in from here. See TODO.md.
		//
		// The assertion is written so the pin FAILS WHEN THE DIVERGENCE
		// CHANGES, in either direction — repaired, or grown to more shapes.
		Expect(len(disagreed)).To(Equal(2),
			"the chained-USING divergence changed: %d of %d arms disagree, not the 2 measured "+
				"against the live JVM. If the count DROPPED, the semantic USING resolution has "+
				"landed and this pin must become an equality assertion; if it GREW, a third "+
				"shape has started diverging and needs measuring.\n\n%s",
			len(disagreed), chainedArms, strings.Join(disagreed, "\n"))
		// And the divergence must stay the SHAPE that was measured — a count of
		// two says nothing about which two.
		Expect(strings.Join(disagreed, "\n")).To(ContainSubstring("Ambiguous reference"),
			"the ambiguous-chain arm no longer diverges by Java raising AMBIGUOUS_COLUMN, so "+
				"the pinned pair is not the pair that was measured:\n\n%s",
			strings.Join(disagreed, "\n"))
		Expect(strings.Join(disagreed, "\n")).To(ContainSubstring("42703"),
			"the far-left-column arm no longer diverges by Go raising 42703, so the pinned "+
				"pair is not the pair that was measured:\n\n%s", strings.Join(disagreed, "\n"))
		Expect(chainedArms).To(Equal(3),
			"%d chained arms ran, not 3 — a green from a shrunken set says nothing about "+
				"the shape this file is named for", chainedArms)
		Expect(controls).To(Equal(2),
			"%d controls ran, not 2 — without an agreed USING baseline, a chained "+
				"disagreement cannot be attributed to the chaining", controls)
	})
})
