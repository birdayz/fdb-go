//go:build bazelrunfiles

package conformance_test

// Does a primary-key intersection stay sound when one leg fixes a component of
// the primary key that the other leg only sorts?
//
// Neither engine did. Over PRIMARY KEY (pk1, pk2) with indexes ON (b, pk1) and
// ON (pk2), `WHERE b = 1 AND pk2 = 3` intersects two covering scans. The merged
// ordering carries pk2 as a constant (the (pk2) leg is equality-bound on it), so
// the only comparison key the enumeration offers is (pk1) — and Java's
// `isCompatibleComparisonKey` (AbstractDataAccessRule.java) accepts it because
// it subtracts the UNION of the legs' equality-bound values from the primary key.
// But pk2 = 3 is a constant of the (pk2) leg's stream ONLY: the (b, pk1) leg
// holds several records per pk1 that differ in pk2, so aligning the legs on
// (pk1) emits records the other leg never matched.
//
// MEASURED on Java 4.12.11.0, and asserted below so the classification cannot
// go stale: Java plans
//
//	COVERING(TI_PK2 [EQUALS …]) ∩ COVERING(TI_B_PK1 [EQUALS …]) COMPARE BY (_.PK1)
//
// and answers every pk2 = 3 record regardless of b — four rows, COUNT(*) = 4 —
// for a query whose answer is the single record (3, 3). Go used to build the
// same merge (emitting the (b, pk1) leg's rows instead, so four b = 1 records
// with the wrong pk2). Go's proof is now per leg
// (cascades/intersector_primary_key.go, comparisonKeyIdentifiesRecordInEveryLeg)
// and declines the merge; Java's answer is pinned as WRONG here, direction
// DivergenceJavaWrongRowsGoCorrect, booked in TODO.md section 9.
//
// The ORDER BY arms are the control. With a requested ordering Java plans the
// (b, pk1) covering scan with a residual filter on pk2 and answers correctly, so
// both engines agree there — which is what says the fixture and the harness are
// sound when the unordered arms disagree.

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

var _ = Describe("PkIntersectionLegBoundKeyJavaProbe", func() {
	It("measures both engines on an intersection whose legs bind different primary-key components", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("pkint_%s", uuid.New().String())
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

		const schema = "CREATE TABLE ti (pk1 BIGINT, pk2 BIGINT, b BIGINT, PRIMARY KEY (pk1, pk2)) " +
			"CREATE INDEX ti_b_pk1 ON ti (b, pk1) " +
			"CREATE INDEX ti_pk2 ON ti (pk2)"
		// Every pk1 in 0..3 has a pk2 = 3 record; the b = 1 records mostly sit
		// at another pk2. Only (3, 3) satisfies both predicates.
		setup := []string{
			"INSERT INTO ti (pk1,pk2,b) VALUES (0,2,1),(0,3,0),(1,4,1),(1,3,0),(2,0,1),(2,3,7),(3,3,1),(4,1,1)",
		}

		// multiset renders rows order-insensitively: the arms Java gets wrong
		// carry no ORDER BY, and a multiset still counts every extra row.
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
			name string
			sql  string
			// want is the SQL-correct answer, pinned absolutely for Go.
			want string
			// javaWrong records the measured Java outcome for this shape.
			javaWrong bool
		}
		arms := []arm{
			{
				name: "rows_unordered", javaWrong: true, want: "{[3 3 1]}",
				sql: "SELECT pk1, pk2, b FROM ti WHERE b = 1 AND pk2 = 3",
			},
			{
				name: "pk1_only_projection", javaWrong: true, want: "{[3]}",
				sql: "SELECT pk1 FROM ti WHERE b = 1 AND pk2 = 3",
			},
			{
				name: "count", javaWrong: true, want: "{[1]}",
				sql: "SELECT COUNT(*) FROM ti WHERE b = 1 AND pk2 = 3",
			},
			{
				name: "rows_ordered_pk1", want: "{[3 3 1]}",
				sql: "SELECT pk1, pk2, b FROM ti WHERE pk2 = 3 AND b = 1 ORDER BY pk1",
			},
			{
				name: "rows_ordered_pk1_pk2", want: "{[3 3 1]}",
				sql: "SELECT pk1, pk2, b FROM ti WHERE pk2 = 3 AND b = 1 ORDER BY pk1, pk2",
			},
		}

		type result struct {
			arm            arm
			javaOut, goOut string
		}
		results := make([]result, 0, len(arms))
		for _, a := range arms {
			r := result{
				arm:     a,
				javaOut: multiset(javaRunner.RunWithSetup(ctx, schema, setup, a.sql)),
				goOut:   multiset(goRunner.RunWithSetup(ctx, schema, setup, a.sql)),
			}
			results = append(results, r)
			mark := "  "
			if r.javaOut != r.goOut {
				mark = "!!"
			}
			fmt.Fprintf(GinkgoWriter, "%s %-22s java=%-40s go=%s\n", mark, a.name, r.javaOut, r.goOut)
		}

		var wrong, agreed int
		for _, r := range results {
			// Go is pinned to the SQL-correct answer on every arm, so the
			// comparison below can never be satisfied by both engines being
			// wrong the same way.
			Expect(r.goOut).To(Equal(r.arm.want),
				"%s: Go's rows changed. A merge that compares the legs on (pk1) alone is back "+
					"(the per-leg comparison-key proof regressed).\n  sql : %s", r.arm.name, r.arm.sql)
			if r.arm.javaWrong {
				wrong++
				Expect(r.javaOut).NotTo(Equal(r.arm.want),
					"%s: Java now answers this shape correctly, so the upstream defect this file "+
						"records is fixed: move the arm to the agreeing group, update the TODO.md "+
						"section 9 entry and the DIVERGENCES.md row.\n  java: %s\n  sql : %s",
					r.arm.name, r.javaOut, r.arm.sql)
			} else {
				agreed++
				Expect(r.javaOut).To(Equal(r.goOut),
					"%s: the control disagrees. With an ORDER BY Java plans the (b, pk1) covering "+
						"scan with a residual pk2 filter and has always answered correctly here; a "+
						"disagreement means the fixture or the harness moved, and the wrong-arm "+
						"readings above are not interpretable.\n  java: %s\n  go  : %s\n  sql : %s",
					r.arm.name, r.javaOut, r.goOut, r.arm.sql)
			}
		}
		// Vacuity guards: the divergence must still be measured by at least one
		// arm, and the control must still be comparing something.
		Expect(wrong).To(BeNumerically(">=", 3),
			"no arm records Java's wrong answer any more; this probe would pass while pinning nothing")
		Expect(agreed).To(BeNumerically(">=", 2),
			"the control group collapsed; the wrong-arm readings have no soundness check behind them")
		fmt.Fprintf(GinkgoWriter,
			"\nMEASURED DIVERGENCE: Java intersects on a comparison key that omits a primary-key "+
				"component fixed in one leg only and returns records the other leg never matched; "+
				"Go declines that merge. Direction: %s.\n", plandiff.DivergenceJavaWrongRowsGoCorrect)
	})
})
