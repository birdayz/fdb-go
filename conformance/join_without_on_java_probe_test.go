//go:build bazelrunfiles

package conformance_test

// A JOIN whose ON clause is absent — measured against the live JVM.
//
// The grammar makes the join condition OPTIONAL for one arm and MANDATORY for
// another:
//
//	(INNER | CROSS)? JOIN tableSourceItem (ON expression | USING '(' … ')')?   #innerJoin
//	(LEFT | RIGHT | FULL) OUTER? JOIN tableSourceItem (ON … | USING …)         #outerJoin
//
// so `FROM a JOIN b` parses with a NULL ON, and `FROM a LEFT JOIN b` does not
// parse at all. fdb-relational's visitor dereferences `InnerJoinContext
// .expression()` unconditionally and NPEs on that null, so it cannot answer any
// conditionless join — `CROSS JOIN` included.
//
// GO ANSWERS THEM, DELIBERATELY, and that is what this file records. The rows
// are the correct cartesian product, the syntax is ordinary SQL, and nothing
// here touches the wire, so it is a read-side capability Java lacks rather than
// a divergence to repair. Go previously refused these to match Java's crash;
// that bought nothing and cost every user who writes CROSS JOIN.
//
// The distinction the file exists to hold is therefore NOT "do the engines
// agree" — three arms deliberately disagree — but WHICH WAY they disagree:
//
//	goExtends   Java cannot answer, Go can          intended, and floored at 3
//	javaOnly    Java answers, Go refuses            always a defect; no arm wants it
//
// That second class is why the measurement stays valuable after the policy
// change. `CROSS JOIN b ON p` was in it: the grammar puts CROSS and the
// optional condition in the same alternative, so it parses with a NON-null
// expression, Java's NPE cannot fire, and Java answers — while Go's gate,
// keyed on the CROSS token rather than on the absent condition, refused it.
//
// NATURAL JOIN and the unparseable `LEFT JOIN` with no ON are the both-refuse
// arms, and they carry weight: without them, a probe showing Go answering more
// than Java could equally be describing a parser that accepts anything.
//
// The comma-join and `ON 1 = 1` arms are the both-answer baseline — and their
// ROWS are compared, not just their success, since agreeing that there is no
// error is not agreeing on the result.
//
// EVERY ARM IS `SELECT COUNT(*)` WITH NO ORDER BY, and that is a measurement
// rather than a style choice. The first draft ordered by `a.id, b.id` and Java
// refused even the comma-join CONTROL with `UnableToPlanException: Cascades
// planner could not plan query` — the same shape seen on a disjunctive join
// with a trailing ORDER BY. An ORDER BY across two join legs is therefore its
// own Java limitation, and leaving it in would have made every row here a
// measurement of that instead of of the null ON.

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

// outcome is what a query DID in the two engines, as a single value, so each
// arm states its expectation instead of the file asserting one blanket rule.
// That matters here because the arms deliberately land in three different
// classes and "the engines agree" is the wrong assertion for one of them.
type outcome string

const (
	bothAnswer outcome = "both-answer"
	bothRefuse outcome = "both-refuse"
	// goExtends is the class this file exists to record: Java cannot answer the
	// query and Go can. It is not a divergence to repair — the rows are
	// correct, the syntax is ordinary SQL and nothing touches the wire, so it
	// is a read-side capability Java lacks.
	goExtends outcome = "go-extends"
	// javaOnly is the direction that IS a defect: Go refusing what Java
	// answers. No arm is expected to be in this class.
	javaOnly outcome = "java-only"
)

func classify(javaRejects, goRejects bool) outcome {
	switch {
	case !javaRejects && !goRejects:
		return bothAnswer
	case javaRejects && goRejects:
		return bothRefuse
	case javaRejects && !goRejects:
		return goExtends
	default:
		return javaOnly
	}
}

var _ = Describe("JoinWithoutOnJavaProbe", func() {
	It("measures both engines on every spelling of a JOIN with no ON clause", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("joinnoon_%s", uuid.New().String())
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

		const schema = "CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE b (id BIGINT, w BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO a (id,v) VALUES (1,10),(2,20)",
			"INSERT INTO b (id,w) VALUES (1,100),(2,200)",
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
			// want is the outcome measured against the live JVM. Stating it per
			// arm is what lets three different classes coexist in one table.
			want outcome
			// wantsTableName marks an arm where accept/reject agreement is too
			// weak: both engines refuse, and WHICH refusal they give is the
			// measurement. Both messages must name the offending table.
			wantsTableName bool
		}{
			{
				name: "comma join", want: bothAnswer,
				sql: "SELECT COUNT(*) FROM a, b",
			},
			{
				name: "JOIN with ON 1 = 1", want: bothAnswer,
				sql: "SELECT COUNT(*) FROM a JOIN b ON 1 = 1",
			},
			// --- the conditionless population: Java NPEs, Go answers.
			{
				want: goExtends, name: "CROSS JOIN",
				sql: "SELECT COUNT(*) FROM a CROSS JOIN b",
			},
			{
				want: goExtends, name: "bare JOIN, no ON",
				sql: "SELECT COUNT(*) FROM a JOIN b",
			},
			{
				want: goExtends, name: "INNER JOIN, no ON",
				sql: "SELECT COUNT(*) FROM a INNER JOIN b",
			},
			{
				// USING is the third alternative of the same optional group. It
				// supplies a join condition without an `expression`, so a
				// visitor that reads `expression()` unconditionally sees the
				// same null here as it does with no clause at all.
				want: bothAnswer, name: "JOIN USING",
				sql: "SELECT COUNT(*) FROM a JOIN b USING (id)",
			},
			{
				want: bothRefuse, name: "NATURAL JOIN",
				sql: "SELECT COUNT(*) FROM a NATURAL JOIN b",
			},
			{
				// CROSS JOIN *WITH* an ON — the arm that separates the two
				// candidate readings of Go's gate. The grammar puts CROSS and
				// the optional join condition in the same alternative, so
				// `CROSS JOIN b ON p` parses with a NON-null expression and
				// Java's NPE cannot fire, so Java answers it. Go refused it
				// while its gate keyed on the CROSS token — the javaOnly class,
				// and the one direction that is always a defect.
				want: bothAnswer, name: "CROSS JOIN with ON",
				sql: "SELECT COUNT(*) FROM a CROSS JOIN b ON 1 = 1",
			},
			{
				// ERROR PRECEDENCE. Two things are wrong with
				// `a JOIN missing_table` — an unknown table and a missing
				// condition — and which one an engine reports is observable.
				// Java visits `tableSourceItem` BEFORE dereferencing the absent
				// expression, so the unknown table wins and the NPE never
				// happens; a gate that rejects the missing condition first
				// turns a precise "no such table" into a generic 0A000 and
				// diverges on a query BOTH engines refuse.
				//
				// Accept/reject agreement is not enough here, so this arm is
				// the one place rows are not the measurement: bothReject is set
				// AND the messages are compared for the table name.
				want: bothRefuse, name: "no ON *and* an unknown table",
				sql:            "SELECT COUNT(*) FROM a JOIN missing_table",
				wantsTableName: true,
			},
			{
				// The grammar's MANDATORY-ON arm, as the boundary control on the
				// other side: an outer join with no ON is not a null expression,
				// it is not a parse at all. Both engines share this grammar, so
				// both must refuse it — and if one does not, the two are running
				// different grammars, which would make every row above suspect.
				want: bothRefuse, name: "LEFT JOIN, no ON (unparseable)",
				sql: "SELECT COUNT(*) FROM a LEFT JOIN b",
			},
		}

		var mismatched []string
		byKind := map[outcome]int{}
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))

			got := classify(rejects(javaOut), rejects(goOut))
			mark := "  "
			if got != c.want {
				mark = "!!"
				mismatched = append(mismatched, fmt.Sprintf(
					"%s\n    want %s, got %s\n    java: %s\n    go  : %s\n    sql : %s",
					c.name, c.want, got, javaOut, goOut, c.sql))
			}
			byKind[c.want]++
			fmt.Fprintf(GinkgoWriter, "%s %-34s %-12s java=%-58s go=%s\n",
				mark, c.name, got, javaOut, goOut)

			// Where both engines ANSWER, the ROWS must match too — agreeing that
			// there is no error is not agreeing on the result.
			if c.want == bothAnswer && got == bothAnswer && javaOut != goOut {
				mismatched = append(mismatched, fmt.Sprintf(
					"%s: both engines answered but with DIFFERENT rows\n"+
						"    java: %s\n    go  : %s\n    sql : %s", c.name, javaOut, goOut, c.sql))
			}
			// Where both REFUSE an unknown table, both must NAME it — otherwise
			// the two faults are being reported in a different order.
			if c.wantsTableName {
				for _, e := range []struct{ name, out string }{{"java", javaOut}, {"go", goOut}} {
					if !strings.Contains(strings.ToUpper(e.out), "MISSING_TABLE") {
						mismatched = append(mismatched, fmt.Sprintf(
							"%s: %s refused it WITHOUT naming the missing table, so source "+
								"resolution is not running first\n    %s: %s\n    sql : %s",
							c.name, e.name, e.name, e.out, c.sql))
					}
				}
			}
		}

		Expect(mismatched).To(BeEmpty(),
			"%d of %d arms did not land on their expected outcome:\n\n%s",
			len(mismatched), len(cases), strings.Join(mismatched, "\n"))

		// One floor per outcome class, because a green from a shrunken set says
		// nothing. goExtends is the one that matters most: it is the class this
		// file exists to record, and it is also the class that silently empties
		// if someone "restores conformance" by refusing these queries again.
		Expect(byKind[bothAnswer]).To(Equal(4),
			"%d both-answer arms, not 4 — without them a goExtends arm cannot be told "+
				"apart from an engine that is simply broken", byKind[bothAnswer])
		Expect(byKind[goExtends]).To(Equal(3),
			"%d goExtends arms, not 3 — these are the deliberate extensions (CROSS JOIN, "+
				"bare JOIN, INNER JOIN, each with no condition). If this reaches zero, Go has "+
				"gone back to refusing queries it answers correctly", byKind[goExtends])
		Expect(byKind[bothRefuse]).To(Equal(3),
			"%d both-refuse arms, not 3 — NATURAL JOIN, the unparseable LEFT JOIN and the "+
				"unknown table are what show the extension is deliberate rather than a parser "+
				"that accepts anything", byKind[bothRefuse])
	})
})
