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
// parse at all. Go's `extractJoinClause` rejects `CROSS JOIN` explicitly,
// citing Java: fdb-relational's visitor dereferences `InnerJoinContext
// .expression()` unconditionally and NPEs when it is null. But that gate keys
// on the CROSS TOKEN, while the null it exists to avoid comes from the ABSENT
// ON — and `FROM a JOIN b` produces the identical null without the token.
//
// So the gate covers one spelling of the shape and the other three — bare
// `JOIN`, `INNER JOIN`, and `USING` — go unmeasured. This file measures all of
// them rather than reasoning about which ones inherit the rejection.
//
// NATURAL JOIN rides along because it is the fourth member of the joinPart
// production and Go reaches it only through `extractJoinClause`'s default arm,
// i.e. by having no case for it. Whether that lands on the same answer Java
// gives is exactly the kind of thing "it falls through to the default" does not
// establish.
//
// THE DIRECTION IS THE POINT. Go rejecting what Java accepts is a reach gap and
// the user sees it. Go ACCEPTING what Java rejects is silent: the query runs,
// returns a cartesian product, and the two engines disagree about what a
// four-row answer means. This file's whole reason for existing is that the
// CROSS-JOIN gate was written to prevent the second kind for one spelling.
//
// The comma-join and explicit-`ON 1 = 1` arms are the controls: both engines
// must accept them and agree, or the fixture is measuring something broken
// about joins in general and the null-ON rows below cannot be read.
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
			// control marks the arms both engines must ACCEPT and agree on,
			// without which nothing else here is interpretable.
			control bool
			// nullOn marks the arms that reach the joinPart production with no
			// ON expression — the population this file exists to measure. The
			// vacuity floor counts them.
			nullOn bool
			// wantsTableName marks an arm where accept/reject agreement is too
			// weak: both engines refuse, and WHICH refusal they give is the
			// measurement. Both messages must name the offending table.
			wantsTableName bool
		}{
			{
				name: "comma join", control: true,
				sql: "SELECT COUNT(*) FROM a, b",
			},
			{
				name: "JOIN with ON 1 = 1", control: true,
				sql: "SELECT COUNT(*) FROM a JOIN b ON 1 = 1",
			},
			// --- the null-ON population.
			{
				nullOn: true, name: "CROSS JOIN",
				sql: "SELECT COUNT(*) FROM a CROSS JOIN b",
			},
			{
				nullOn: true, name: "bare JOIN, no ON",
				sql: "SELECT COUNT(*) FROM a JOIN b",
			},
			{
				nullOn: true, name: "INNER JOIN, no ON",
				sql: "SELECT COUNT(*) FROM a INNER JOIN b",
			},
			{
				// USING is the third alternative of the same optional group. It
				// supplies a join condition without an `expression`, so a
				// visitor that reads `expression()` unconditionally sees the
				// same null here as it does with no clause at all.
				nullOn: true, name: "JOIN USING",
				sql: "SELECT COUNT(*) FROM a JOIN b USING (id)",
			},
			{
				nullOn: true, name: "NATURAL JOIN",
				sql: "SELECT COUNT(*) FROM a NATURAL JOIN b",
			},
			{
				// CROSS JOIN *WITH* an ON — the arm that separates the two
				// candidate readings of Go's gate. The grammar puts CROSS and
				// the optional join condition in the same alternative, so
				// `CROSS JOIN b ON p` parses with a NON-null expression and
				// Java's NPE cannot fire. If Go's rejection is really about the
				// null, it must let this through; if it is about the CROSS
				// token, it refuses a query Java answers.
				//
				// Not part of the null-ON census — it is the opposite of one —
				// but it must still AGREE, which is what mustAgree marks.
				name: "CROSS JOIN with ON",
				sql:  "SELECT COUNT(*) FROM a CROSS JOIN b ON 1 = 1",
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
				nullOn: true, name: "no ON *and* an unknown table",
				sql:            "SELECT COUNT(*) FROM a JOIN missing_table",
				wantsTableName: true,
			},
			{
				// The grammar's MANDATORY-ON arm, as the boundary control on the
				// other side: an outer join with no ON is not a null expression,
				// it is not a parse at all. Both engines share this grammar, so
				// both must refuse it — and if one does not, the two are running
				// different grammars, which would make every row above suspect.
				nullOn: true, name: "LEFT JOIN, no ON (unparseable)",
				sql: "SELECT COUNT(*) FROM a LEFT JOIN b",
			},
		}

		var divergent, disagreed []string
		var nullOnArms, controls, named int
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			mark := "  "
			if rejects(javaOut) != rejects(goOut) {
				mark = "!!"
				divergent = append(divergent, fmt.Sprintf(
					"%s (javaRejects=%v goRejects=%v)\n    java: %s\n    go  : %s\n    sql : %s",
					c.name, rejects(javaOut), rejects(goOut), javaOut, goOut, c.sql))
			}
			fmt.Fprintf(GinkgoWriter, "%s %-32s java=%-64s go=%s\n", mark, c.name, javaOut, goOut)

			if c.control {
				controls++
				Expect(rejects(goOut)).To(BeFalse(),
					"%s is a CONTROL and Go rejected it, so this fixture is not measuring "+
						"the null-ON population — it is measuring something broken about joins\n"+
						"  go: %s\n  sql: %s", c.name, goOut, c.sql)
				Expect(rejects(javaOut)).To(BeFalse(),
					"%s is a CONTROL and Java rejected it\n  java: %s\n  sql: %s",
					c.name, javaOut, c.sql)
				Expect(goOut).To(Equal(javaOut),
					"%s is a CONTROL and the engines disagree on it, so no null-ON row below "+
						"can be read as a finding about null ON\n  java: %s\n  go: %s",
					c.name, javaOut, goOut)
			}
			if c.nullOn {
				nullOnArms++
			}
			if !c.control {
				// The ACCEPT/REJECT decision is the whole measurement. Rows are
				// not compared here: where both engines accept, the comma-join
				// control already pins the cartesian answer, and where both
				// reject, the messages cannot be shared (Java's is an NPE naming
				// a parser-internal class, Go's is a clean 0A000).
				//
				// Collected rather than asserted in the loop: a Gomega failure
				// aborts the spec, so asserting here would report the FIRST
				// disagreeing spelling and leave the rest unmeasured — and the
				// question this file asks is which spellings diverge, not
				// whether any does.
				if rejects(goOut) != rejects(javaOut) {
					disagreed = append(disagreed, fmt.Sprintf(
						"%s\n    java: %s\n    go  : %s\n    sql : %s",
						c.name, javaOut, goOut, c.sql))
				}
				if c.wantsTableName {
					named++
					for engine, out := range map[string]string{"java": javaOut, "go": goOut} {
						if !strings.Contains(strings.ToUpper(out), "MISSING_TABLE") {
							disagreed = append(disagreed, fmt.Sprintf(
								"%s: %s refused it WITHOUT naming the missing table, so the two "+
									"faults are being reported in a different ORDER than the other "+
									"engine\n    %s: %s\n    sql : %s",
								c.name, engine, engine, out, c.sql))
						}
					}
				}
			}
		}

		fmt.Fprintf(GinkgoWriter, "\nDIVERGENT: %d of %d\n", len(divergent), len(cases))
		for _, d := range divergent {
			fmt.Fprintf(GinkgoWriter, "%s\n", d)
		}
		Expect(disagreed).To(BeEmpty(),
			"the engines DISAGREE on whether a join with no ON clause is a legal query, "+
				"for %d of the %d null-ON spellings measured. Go accepting what Java refuses is the "+
				"silent direction — the query runs, returns a cartesian product, and nothing "+
				"says the two engines read it differently. Go's gate in extractJoinClause "+
				"keys on the CROSS TOKEN; the null it guards against comes from the ABSENT "+
				"ON, so it must key on that instead.\n\n%s",
			len(disagreed), nullOnArms, strings.Join(disagreed, "\n"))

		// Two floors, because there are two ways to be green having measured
		// nothing: no null-ON arm left in the slice, and no control left to make
		// the null-ON rows interpretable.
		Expect(named).To(Equal(1),
			"%d error-precedence arms ran, not 1 — without one, nothing checks that a "+
				"conditionless join over an UNKNOWN TABLE reports the same fault first in "+
				"both engines", named)
		Expect(nullOnArms).To(Equal(7),
			"the null-ON population changed: %d arms were measured, not the 7 this file "+
				"ran against the live JVM. A green from a shrunken set says nothing about "+
				"the spellings that were dropped", nullOnArms)
		Expect(controls).To(Equal(2),
			"%d controls ran, not 2 — without a join shape both engines accept and agree on, "+
				"a null-ON arm rejected by both proves nothing about null ON", controls)
	})
})
