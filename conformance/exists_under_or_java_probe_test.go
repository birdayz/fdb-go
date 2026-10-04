//go:build bazelrunfiles

package conformance_test

// Does an EXISTS inside a disjunction of a join's condition still filter?
//
// Java does not. The disjunction stays in the lower select over c, correlated
// to the existential that ranges over d one level up, and when that select is
// matched to an index, ExistentialValuePredicate.computeCompensationFunction
// returns noCompensationNeeded because the existential is not one of the
// matched select's quantifiers. The predicate simply disappears: the ON form
// keeps (2, 51), whose a has no d row; the WHERE forms keep only `c.id > 100`
// and return nothing. Go reapplies the predicate as an ordinary residual over
// the outer row (select_subsumption_predicates.go,
// selectSubsumptionExistentialPredicateCompensation). Direction
// DivergenceJavaWrongRowsGoCorrect; TODO.md section 9 books the upstream report.
//
// The control is the conjunctive EXISTS, which both engines answer correctly.

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

var _ = Describe("ExistsUnderOrJavaProbe", func() {
	It("measures both engines on an EXISTS inside a disjunction of a join's condition", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("exor_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		const schema = "CREATE TABLE a (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE c (id BIGINT, a_id BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE d (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX c_a_id ON c (a_id)"
		// Only a = 1 has a d row; no c.id exceeds 100, so the answer is (1, 50).
		setup := []string{
			"INSERT INTO a VALUES (1), (2)",
			"INSERT INTO c VALUES (50, 1), (51, 2)",
			"INSERT INTO d VALUES (1), (51)",
		}
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

		type arm struct {
			name, sql string
			javaWrong bool
		}
		const want = "{[1 50]}"
		arms := []arm{
			{
				name: "on_disjunction", javaWrong: true,
				sql: "SELECT a.id, c.id FROM a JOIN c ON (c.a_id = a.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)) OR c.id > 100",
			},
			{
				name: "where_disjunction", javaWrong: true,
				sql: "SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id WHERE EXISTS (SELECT 1 FROM d WHERE d.id = a.id) OR c.id > 100",
			},
			{
				name: "comma_join_disjunction", javaWrong: true,
				sql: "SELECT a.id, c.id FROM a, c WHERE c.a_id = a.id AND (EXISTS (SELECT 1 FROM d WHERE d.id = a.id) OR c.id > 100)",
			},
			{
				name: "conjunction_control",
				sql:  "SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)",
			},
		}
		var wrong, agreed int
		for _, a := range arms {
			javaOut := multiset(javaRunner.RunWithSetup(ctx, schema, setup, a.sql))
			goOut := multiset(goRunner.RunWithSetup(ctx, schema, setup, a.sql))
			fmt.Fprintf(GinkgoWriter, "%-24s java=%-28s go=%s\n", a.name, javaOut, goOut)
			Expect(goOut).To(Equal(want),
				"%s: Go's rows changed; an existential predicate is being dropped again.\n  sql: %s", a.name, a.sql)
			if a.javaWrong {
				wrong++
				Expect(javaOut).NotTo(Equal(want),
					"%s: Java now answers this shape correctly, so the upstream defect is fixed: move the "+
						"arm to the control and update the TODO.md section 9 and DIVERGENCES.md entries.\n  java: %s\n  sql: %s",
					a.name, javaOut, a.sql)
			} else {
				agreed++
				Expect(javaOut).To(Equal(goOut),
					"%s: the control disagrees, so the fixture or harness moved.\n  java: %s\n  go: %s", a.name, javaOut, goOut)
			}
		}
		Expect(wrong).To(BeNumerically(">=", 3), "no arm records Java's wrong answer any more")
		Expect(agreed).To(BeNumerically(">=", 1), "the control collapsed")
		fmt.Fprintf(GinkgoWriter, "\nMEASURED DIVERGENCE: Java drops an existential predicate that a matched lower select "+
			"carries for an outer existential; Go reapplies it. Direction: %s.\n", plandiff.DivergenceJavaWrongRowsGoCorrect)
	})
})
