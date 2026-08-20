//go:build bazelrunfiles

package conformance_test

// Does a record that satisfies a disjunctive join condition through MORE THAN
// ONE disjunct come back once?
//
// Go said no. A disjunction is planned as a union of one access path per DNF
// term, so a record satisfying two terms is produced by two legs; those legs
// are covering scans of DIFFERENT indexes, whose rows for that one record
// differ — (ua, uid) from one, (ub, uid) from the other — and the cross-leg
// dedup was keyed on the ROW rather than on the primary key, so it collapsed
// neither. The repair keys it on the primary key, which is what Java's rule set
// does (its LogicalDistinctExpression IS a primary-key dedup; Go's is a
// full-row one, and the two node sets do not line up by name).
//
// So the fix is PARITY, and this file is the direct evidence: over a fixture
// where u=13 satisfies both disjuncts, Java and Go return the same three rows.
//
// WHAT THE PROBE CORRECTED. It was first written asserting that Java simply
// cannot plan a disjunctive join — the three projection shapes all came back
// UnableToPlanException, and that reading was written into this header as
// settled. The `_no_orderby` arms, added only to say WHICH clause defeated the
// planner, refuted it: Java plans the same join perfectly well and answers the
// same rows. What Java refuses is a disjunctive join carrying a trailing
// ORDER BY:
//
//	SELECT t.id, u.uid FROM t LEFT JOIN u ON u.ua = t.a OR u.ub = t.b
//	  java: [[1 11] [1 12] [1 13]]     go: same three rows
//	... the same query + ORDER BY t.id, u.uid
//	  java: UnableToPlanException      go: same three rows, ordered
//
// That is a narrow Java planner limitation Go does not share
// (DivergenceJavaErrorsGoCorrect), and it is worth far less than the thing it
// was hiding: the arms that DO compare are the ones that pin the dedup.
//
// ORDER is not compared where the query does not ask for one. Without an
// ORDER BY the engines are free to emit the same multiset in different orders
// and they DO — Java gave 11,12,13 and Go 11,13,12 — so an ordered string
// compare there would report a divergence that is not one. The rows are
// compared as a multiset, which still catches a duplicate (a multiset counts
// 13 twice) and still catches a dropped row.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("OrUnionDedupJavaProbe", func() {
	It("measures both engines on a disjunctive join that matches through several legs", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("orunion_%s", uuid.New().String())
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

		const schema = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE u (uid BIGINT, ua BIGINT, ub BIGINT, PRIMARY KEY (uid)) " +
			"CREATE INDEX u_ua ON u (ua) " +
			"CREATE INDEX u_ub ON u (ub)"
		// u=13 satisfies BOTH disjuncts against the single t row; 11 and 12
		// satisfy one each; 14 satisfies neither. So a correct answer is three
		// rows and a correct COUNT is 3 — four would be 13 surviving twice.
		setup := []string{
			"INSERT INTO t (id,a,b) VALUES (1,1,2)",
			"INSERT INTO u (uid,ua,ub) VALUES (11,1,9),(12,7,2),(13,1,2),(14,8,8)",
		}

		// multiset renders rows order-insensitively for the queries that do not
		// ask for an order. Sorting the per-row renderings is enough here: every
		// cell is a scalar, so a row's rendering determines it, and equal
		// multisets sort to equal sequences.
		multiset := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			parts := make([]string, 0, len(r.Rows.Rows))
			for _, row := range r.Rows.Rows {
				parts = append(parts, fmt.Sprint(row))
			}
			sort.Strings(parts)
			return "{" + strings.Join(parts, " ") + "}"
		}
		ordered := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}

		type arm struct {
			name string
			sql  string
			// want is the pinned Go answer, in the same rendering the arm is
			// compared with — so a pass means Go is right in absolute terms and
			// not merely equal to whatever Java did.
			want string
			// javaRefuses records the measured Java outcome for this shape.
			javaRefuses bool
		}
		arms := []arm{
			// --- disjunctive JOIN, no trailing ORDER BY: Java plans these.
			{
				name: "left_join_or", want: "{[1 11] [1 12] [1 13]}",
				sql: "SELECT t.id, u.uid FROM t LEFT JOIN u ON u.ua = t.a OR u.ub = t.b",
			},
			{
				name: "inner_join_or", want: "{[1 11] [1 12] [1 13]}",
				sql: "SELECT t.id, u.uid FROM t JOIN u ON u.ua = t.a OR u.ub = t.b",
			},
			{
				name: "comma_join_or", want: "{[1 11] [1 12] [1 13]}",
				sql: "SELECT t.id, u.uid FROM t, u WHERE u.ua = t.a OR u.ub = t.b",
			},
			// --- the same joins under COUNT(*). 3, never 4.
			{
				name: "count_left_join_or", want: "{[3]}",
				sql: "SELECT COUNT(*) FROM t LEFT JOIN u ON u.ua = t.a OR u.ub = t.b",
			},
			{
				name: "count_inner_join_or", want: "{[3]}",
				sql: "SELECT COUNT(*) FROM t JOIN u ON u.ua = t.a OR u.ub = t.b",
			},
			// --- single-table disjunction over the same two indexes: the union
			// shape without a join around it. Java plans this one WITH ORDER BY.
			{
				name: "single_table_or", want: "{[11] [12] [13]}",
				sql: "SELECT uid FROM u WHERE ua = 1 OR ub = 2 ORDER BY uid",
			},
			{
				name: "count_single_table_or", want: "{[3]}",
				sql: "SELECT COUNT(*) FROM u WHERE ua = 1 OR ub = 2",
			},
			// --- disjunctive JOIN + trailing ORDER BY: the one shape Java
			// refuses. Compared ordered, because these DO ask for an order.
			{
				name: "left_join_or_ordered", javaRefuses: true, want: "[[1 11] [1 12] [1 13]]",
				sql: "SELECT t.id, u.uid FROM t LEFT JOIN u ON u.ua = t.a OR u.ub = t.b " +
					"ORDER BY t.id, u.uid",
			},
			{
				name: "inner_join_or_ordered", javaRefuses: true, want: "[[1 11] [1 12] [1 13]]",
				sql: "SELECT t.id, u.uid FROM t JOIN u ON u.ua = t.a OR u.ub = t.b " +
					"ORDER BY t.id, u.uid",
			},
			{
				name: "comma_join_or_ordered", javaRefuses: true, want: "[[1 11] [1 12] [1 13]]",
				sql: "SELECT t.id, u.uid FROM t, u WHERE u.ua = t.a OR u.ub = t.b " +
					"ORDER BY t.id, u.uid",
			},
		}

		// Measure every arm FIRST and print the whole table, so one failing
		// expectation cannot hide the rest of the measurement.
		type result struct {
			arm              arm
			javaOut, goOut   string
			javaErr, matched bool
		}
		results := make([]result, 0, len(arms))
		for _, a := range arms {
			jr := javaRunner.RunWithSetup(ctx, schema, setup, a.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, a.sql)
			r := result{arm: a, javaErr: jr.Err != nil}
			if a.javaRefuses {
				r.javaOut, r.goOut = ordered(jr), ordered(gr)
			} else {
				r.javaOut, r.goOut = multiset(jr), multiset(gr)
			}
			r.matched = r.javaOut == r.goOut
			results = append(results, r)
			mark := "  "
			if !r.matched {
				mark = "!!"
			}
			fmt.Fprintf(GinkgoWriter, "%s %-24s java=%-34s go=%s\n", mark, a.name, r.javaOut, r.goOut)
		}

		var compared, refused int
		for _, r := range results {
			if r.arm.javaRefuses {
				refused++
				Expect(r.javaOut).To(ContainSubstring("UnableToPlan"),
					"%s: Java now PLANS a disjunctive join carrying an ORDER BY. That is the one "+
						"limitation this file records as Java-only, so the header is now stale: move "+
						"this arm to the compared group and rewrite the paragraph.\n  java: %s\n  sql : %s",
					r.arm.name, r.javaOut, r.arm.sql)
			} else {
				compared++
				Expect(r.goOut).To(Equal(r.javaOut),
					"%s: the engines disagree on a shape they both plan. Before the cross-leg dedup "+
						"was re-keyed on the primary key Go returned the doubly-matching record TWICE; "+
						"a disagreement now means either that regressed or the dedup grew aggressive "+
						"enough to drop a row.\n  java: %s\n  go  : %s\n  sql : %s",
					r.arm.name, r.javaOut, r.goOut, r.arm.sql)
			}
			// Every arm is ALSO pinned absolutely, so two engines wrong the same
			// way could not satisfy a comparison together, and so the arms Java
			// refuses are still pinned at all.
			Expect(r.goOut).To(Equal(r.arm.want),
				"%s: Go's rows changed. u=13 satisfies BOTH disjuncts, so seeing it twice is the "+
					"row-keyed dedup returning.\n  sql : %s", r.arm.name, r.arm.sql)
		}

		// Vacuity guards, in both directions. If Java started planning the
		// ordered form, `refused` would be 0 and this file would assert nothing
		// about the limitation it records; if Java stopped planning any of them,
		// `compared` would be 0 and the parity claim — the whole point — would
		// be resting on nothing.
		Expect(compared).To(BeNumerically(">=", 7),
			"the compared group has collapsed: the dedup fix is no longer pinned against Java by "+
				"any shape, so this probe would pass while proving nothing")
		Expect(refused).To(BeNumerically(">=", 3),
			"no shape is refused by Java any more, so the DivergenceJavaErrorsGoCorrect direction "+
				"recorded in this file's header no longer holds and the header must be rewritten")
		fmt.Fprintf(GinkgoWriter,
			"\nMEASURED at %d arms: %d compared against Java (dedup is PARITY), %d refused by Java "+
				"(disjunctive join + ORDER BY only). Direction for the refused ones: %s.\n",
			len(arms), compared, refused, plandiff.DivergenceJavaErrorsGoCorrect)
	})
})
