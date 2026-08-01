//go:build bazelrunfiles

package conformance_test

// Measures BOTH engines on integer-aggregate overflow when the operand comes
// from a JOIN LEG, and ASSERTS the two agree.
//
// It exists because the width decision behind SUM_I vs SUM_L was once
// unrecoverable through a join in Go — the operand's ordinal is stated in its
// own leg's layout while the width list was the merged, qualified join row —
// so `SELECT SUM(j1.x) FROM j1, j2 WHERE ...` over an INTEGER column silently
// answered in int64 where the SAME column of the SAME table without the join
// raised 22003. Java raises for both: NumericAggregationValue.encapsulate
// keys the operator map on the operand's static TypeCode
// (NumericAggregationValue.java:196-209), SUM_I accumulates with
// Math.addExact(int, int) (:629), and the reference's TypeCode survives a
// join because it is the column's own. That claim about Java was prose in a
// Go test until this probe measured it; a claim about another engine's
// behaviour is either measured or it is folklore.
//
// The probe asserts ACCEPT/REJECT agreement per shape, that every REJECT is
// an OVERFLOW rejection on both sides (a Java UnableToPlanException would
// also "reject" and must not be mistaken for the semantics under test), and
// that Go's rejections carry SQLSTATE 22003. Java's ArithmeticException has
// no SQLSTATE mapping in fdb-relational (its error enum has no 22003 member),
// so the states themselves are not compared — the message is the shared
// surface.

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

// sumOvfOutcome is the comparable shape of one engine's answer: whether it
// accepted, and if not, the engine's message. The message is compared against
// the expected LANE ("integer overflow" for SUM_I/AVG_I, "long overflow" for
// SUM_L) — not merely "some overflow" — so a rejection from the wrong lane, or
// a non-overflow rejection wearing this probe's expected outcome (a Java
// UnableToPlanException, a Go decline), cannot pass as agreement.
type sumOvfOutcome struct {
	accepted bool
	detail   string
}

func (o sumOvfOutcome) String() string {
	if o.accepted {
		return "ACCEPT"
	}
	return "REJECT(" + o.detail + ")"
}

// laneMatches reports whether a rejection carries the expected lane's verbatim
// Java message.
func (o sumOvfOutcome) laneMatches(wantMsg string) bool {
	return !o.accepted && strings.Contains(o.detail, wantMsg)
}

var _ = Describe("SumOverflowJoinLegJavaProbe", func() {
	It("measures both engines on integer SUM/AVG overflow through a join leg", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("sumovf_%s", uuid.New().String())
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

		schema := "CREATE TABLE J1 (id BIGINT, x INTEGER, PRIMARY KEY (id)) " +
			"CREATE TABLE JN (id BIGINT, x INTEGER, PRIMARY KEY (id)) " +
			"CREATE TABLE JB (id BIGINT, y BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE JE (id BIGINT, x INTEGER, PRIMARY KEY (id)) " +
			"CREATE TABLE JU (id BIGINT, x INTEGER, PRIMARY KEY (id)) " +
			"CREATE TABLE J2 (id2 BIGINT, y BIGINT, PRIMARY KEY (id2))"
		setup := []string{
			"INSERT INTO J1 VALUES (1, CAST(2000000000 AS INTEGER)), (2, CAST(2000000000 AS INTEGER))",
			"INSERT INTO JN VALUES (1, CAST(-2000000000 AS INTEGER)), (2, CAST(-2000000000 AS INTEGER))",
			"INSERT INTO JB VALUES (1, 9223372036854775807), (2, 1)",
			"INSERT INTO JE VALUES (1, CAST(2000000000 AS INTEGER)), (2, CAST(147483647 AS INTEGER))",
			"INSERT INTO JU VALUES (1, CAST(1 AS INTEGER)), (2, CAST(2 AS INTEGER))",
			"INSERT INTO J2 VALUES (1, 1), (2, 1)",
		}

		classify := func(r plandiff.RunResult) sumOvfOutcome {
			if r.Err == nil {
				return sumOvfOutcome{accepted: true}
			}
			var je *plandiff.JavaError
			if errors.As(r.Err, &je) {
				return sumOvfOutcome{detail: je.SQLState + " " + je.Message}
			}
			var ge *api.Error
			if errors.As(r.Err, &ge) {
				return sumOvfOutcome{detail: string(ge.Code) + " " + ge.Message}
			}
			return sumOvfOutcome{detail: "?:" + r.Err.Error()}
		}

		type probe struct {
			name, sql string
			// wantMsg "" means both engines must ACCEPT; otherwise both must
			// REJECT with this lane's verbatim Java message.
			wantMsg string
		}
		const intLane = "integer overflow"
		const longLane = "long overflow"
		probes := []probe{
			// The shape under test: an INTEGER operand read off a join leg.
			{"sum_int_join_overflow", "SELECT SUM(J1.x) FROM J1, J2 WHERE J1.id = J2.id2", intLane},
			// Control: the SAME column of the SAME table without the join. If
			// this disagrees while the join case agrees, the harness or the
			// standalone lane broke — not the leg path.
			{"sum_int_alone_overflow", "SELECT SUM(x) FROM J1", intLane},
			// The negative direction of the int32 check.
			{"sum_int_join_negative_overflow", "SELECT SUM(JN.x) FROM JN, J2 WHERE JN.id = J2.id2", intLane},
			// Exactly MaxInt32 — inside the domain, must answer.
			{"sum_int_join_exact_boundary", "SELECT SUM(JE.x) FROM JE, J2 WHERE JE.id = J2.id2", ""},
			// The SUM_L lane through the same join shape.
			{"sum_bigint_join_overflow", "SELECT SUM(JB.y) FROM JB, J2 WHERE JB.id = J2.id2", longLane},
			// AVG_I shares the int-checked accumulation.
			{"avg_int_join_overflow", "SELECT AVG(J1.x) FROM J1, J2 WHERE J1.id = J2.id2", intLane},
			// COUNT never consults the width.
			{"count_int_join_control", "SELECT COUNT(J1.x) FROM J1, J2 WHERE J1.id = J2.id2", ""},
			// DERIVED-TABLE input: the operand is the derived table's output
			// column, not a base-table reference. In Java the derived column's
			// TypeCode is still INT (the projection flows the column's own
			// type), so SUM_I fires through the wrapper too.
			{"sum_int_derived_overflow", "SELECT SUM(D.x) FROM (SELECT x FROM J1) AS D", intLane},
			{"sum_int_derived_exact_boundary", "SELECT SUM(D.x) FROM (SELECT x FROM JE) AS D", ""},
			// UNION ALL input: the aggregate input's row is the union's merged
			// output layout. Java types the union output by the branches' common
			// type — INT ∪ INT stays INT — so SUM_I fires over a union too.
			{"sum_int_union_all_overflow", "SELECT SUM(U.x) FROM (SELECT x FROM J1 UNION ALL SELECT x FROM J1) AS U", intLane},
			{"sum_int_union_all_in_range", "SELECT SUM(U.x) FROM (SELECT x FROM JU UNION ALL SELECT x FROM JU) AS U", ""},
			// The SUM_L control through a derived wrapper: BIGINT must keep the
			// long lane even when the int32 width flows through derived typing.
			{"sum_bigint_derived_overflow", "SELECT SUM(D.y) FROM (SELECT y FROM JB) AS D", longLane},
		}

		var problems []string
		for _, p := range probes {
			java := classify(javaRunner.RunWithSetup(ctx, schema, setup, p.sql))
			goSide := classify(goRunner.RunWithSetup(ctx, schema, setup, p.sql))
			mark := "  "
			switch {
			case java.accepted != goSide.accepted:
				mark = "!!"
				problems = append(problems,
					fmt.Sprintf("%s: engines disagree: java=%s go=%s\n    %s", p.name, java, goSide, p.sql))
			case p.wantMsg == "" && !java.accepted:
				mark = "!!"
				problems = append(problems, fmt.Sprintf(
					"%s: both engines rejected but the probe expects an answer — the shape stopped measuring what it was built for: java=%s go=%s\n    %s",
					p.name, java, goSide, p.sql))
			case p.wantMsg != "" && java.accepted:
				mark = "!!"
				problems = append(problems, fmt.Sprintf(
					"%s: both engines answered but the probe expects a %q rejection — the shape stopped measuring what it was built for\n    %s",
					p.name, p.wantMsg, p.sql))
			case p.wantMsg != "" && (!java.laneMatches(p.wantMsg) || !goSide.laneMatches(p.wantMsg)):
				// A rejection outside the expected lane on EITHER side is a
				// different phenomenon (Java UnableToPlan, a Go decline, or the
				// wrong accumulator width) wearing this probe's expected
				// outcome.
				mark = "!!"
				problems = append(problems,
					fmt.Sprintf("%s: rejected, but not with the %q lane message on both sides: java=%s go=%s\n    %s",
						p.name, p.wantMsg, java, goSide, p.sql))
			}
			fmt.Fprintf(GinkgoWriter, "%s %-32s java=%-40s go=%-40s %s\n",
				mark, p.name, java, goSide, p.sql)
		}

		Expect(problems).To(BeEmpty(), "integer-aggregate overflow through a join leg diverges between the engines.\n"+
			"Java's SUM_I/AVG_I raise on int32 overflow regardless of the join; Go must do the same.\n"+
			strings.Join(problems, "\n"))
	})
})
