//go:build bazelrunfiles

package conformance_test

// Measures BOTH engines on a trailing `ORDER BY` after `… UNION ALL SELECT …`
// and ASSERTS the divergence that is currently real: Java orders the RIGHT LEG
// ONLY, Go orders the COMBINED result (the SQL-standard reading).
//
// The watch-list entry for this divergence was HALF-pinned: Go's side had a
// yamsql pin, but the claim about Java rested on the prose record of a live
// probe that no longer existed as a committed test. A claim about another
// engine's behaviour is either measured on every run or it is folklore — and
// this list has had folklore inverted by measurement before, so the Java half
// gets the same standing the Go half already had.
//
// Java's shape comes from QueryVisitor.visitSetQuery visiting each leg
// independently: every leg keeps its own ORDER BY, and the trailing one binds
// to the last simpleTable rather than to the set operation. Go lifts it to the
// union output. Both engines answer; the ROWS differ in ORDER, which is why a
// row-set comparison that ignores order would see nothing.
//
// WHY THIS ASSERTS SUBSEQUENCES RATHER THAN EXACT SEQUENCES: Java's UNION ALL
// does not concatenate its legs in a fixed order. Measured directly — the same
// unordered union returned [1 6 2 5] on one run and [2 1 6 5] on another, the
// second interleaving the legs — so any pin naming a whole expected sequence
// for Java is a flake waiting to happen, and would eventually be "fixed" by
// loosening the very assertion that carries the finding. What IS stable is each
// leg's own relative order, because each leg is a primary-key scan. So the
// divergence is asserted where it actually lives: within one leg.
//
// WHICH FIXTURES MAY CARRY THE "JAVA DID NOT SORT THE UNION" CLAIM: not all of
// them, and the difference is structural rather than a matter of taste. That
// claim is asserted by checking Java's sequence is not the combined sort — but
// under a right-leg-only sort the emitted sequence is SOME interleaving of the
// left leg's natural order with the right leg's sorted order, and for certain
// fixtures one of those legal interleavings IS the combined sort. On such a
// fixture the check can fire on a completely unchanged Java. Concretely: with
// legs [1,6] and [2,5] under `ORDER BY id ASC`, the interleaving 1,2,5,6 is
// both legal for right-leg-only AND equal to the global ascending sort.
//
// A fixture is therefore only allowed to carry the claim when NO legal
// interleaving can equal the combined sort. That holds exactly when the LEFT
// leg's natural order contradicts the requested direction, because the left
// leg's relative order is preserved under every interleaving:
//
//   - `ORDER BY id DESC` over PK legs: left natural is ascending [1,6] while a
//     globally descending sequence needs [6,1]. Impossible — claim allowed.
//   - `ORDER BY id ASC` over PK legs: left natural is already ascending, so an
//     ascending interleaving exists. Claim NOT allowed on this shape, and no
//     choice of values fixes it — a PK scan is ascending by construction.
//   - `ORDER BY v ASC` over a NON-key column whose natural (PK) order is
//     descending in v: the left leg emits [60,10], so no ascending
//     interleaving exists. Claim allowed — this is how the ASC direction keeps
//     real coverage of the divergence rather than being dropped.
//
// Red means the divergence CHANGED: either Java started sorting the combined
// result (the entry can be retired) or Go stopped (a Go regression). Either way
// the watch-list entry must be re-read, not the assertion relaxed.

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

// unionOrderIDs flattens a single-column RunResult to the value sequence IN THE
// ORDER THE ENGINE RETURNED IT. Order is the whole subject here, so nothing may
// sort or canonicalise on the way through.
func unionOrderIDs(r plandiff.RunResult) []float64 {
	ids := make([]float64, 0, len(r.Rows.Rows))
	for _, row := range r.Rows.Rows {
		Expect(row).To(HaveLen(1), "probe expects single-column rows")
		f, ok := row[0].(float64)
		Expect(ok).To(BeTrue(), fmt.Sprintf("non-numeric value %#v", row[0]))
		ids = append(ids, f)
	}
	return ids
}

// unionSubseq returns the members of ids that belong to one leg, in the order
// the engine emitted them. Leg membership is by VALUE because every fixture
// gives its two legs disjoint values.
func unionSubseq(ids []float64, leg map[float64]bool) []float64 {
	out := make([]float64, 0, len(ids))
	for _, v := range ids {
		if leg[v] {
			out = append(out, v)
		}
	}
	return out
}

// unionSorted is the multiset of values, canonicalised — used only to assert
// the engines returned the SAME ROWS, never to assert order.
func unionSorted(ids []float64) []float64 {
	out := append([]float64(nil), ids...)
	sort.Float64s(out)
	return out
}

func unionLegSet(vs ...float64) map[float64]bool {
	m := make(map[float64]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

var _ = Describe("UnionTrailingOrderByJavaProbe", func() {
	It("measures both engines on a trailing ORDER BY over UNION ALL", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("unionoby_%s", uuid.New().String())
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

		// UOA/UOB order by the PRIMARY KEY, so each leg's natural order is
		// ascending. UOC/UOD order by a NON-key column stored against the PK in
		// descending order, which is what lets the ASC direction carry the
		// not-the-combined-sort claim (see the header). Both legs of UOC/UOD
		// name the column `v` because Java binds the trailing ORDER BY to the
		// RIGHT leg — a right leg whose column were named `w` would raise 42703
		// there, and the probe would be measuring an error rather than an order.
		schema := "CREATE TABLE UOA (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE UOB (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE UOC (id BIGINT, v BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE UOD (id BIGINT, v BIGINT, PRIMARY KEY (id)) " +
			// Only the RIGHT leg is indexed on v. Java cannot plan an ORDER BY
			// over an unindexed non-key column at all (UnableToPlanException),
			// so without this index the fixture measures a planner limitation
			// instead of an ordering. Indexing the LEFT leg too would be worse
			// than useless: it could turn that leg's scan into a v-ascending
			// one, and the fixture's whole distinguishing power rests on the
			// left leg emitting 60 before 10.
			"CREATE INDEX uod_v ON UOD (v)"
		setup := []string{
			"INSERT INTO UOA VALUES (1), (6)",
			"INSERT INTO UOB VALUES (2), (5)",
			"INSERT INTO UOC VALUES (1, 60), (2, 10)",
			"INSERT INTO UOD VALUES (1, 20), (2, 50)",
		}
		pkLeft, pkRight := unionLegSet(1, 6), unionLegSet(2, 5)
		vLeft, vRight := unionLegSet(60, 10), unionLegSet(20, 50)

		type probe struct {
			name, sql string
			// wantGo is Go's FULL sequence: Go sorts the combined result, so
			// its output is deterministic and pinned exactly.
			wantGo []float64
			// wantJavaLeft/wantJavaRight are Java's PER-LEG subsequences — the
			// interleave-robust form of "only the right leg was sorted".
			wantJavaLeft, wantJavaRight []float64
			leftLeg, rightLeg           map[float64]bool
			// distinguishing marks a fixture on which NO legal right-leg-only
			// interleaving equals the combined sort, so "Java's sequence is not
			// the combined sort" is a sound claim. False means the fixture
			// cannot carry that claim — never that the claim was inconvenient.
			distinguishing bool
			why            string
		}
		probes := []probe{
			{
				name:           "trailing_order_by_desc",
				sql:            "SELECT id FROM UOA UNION ALL SELECT id FROM UOB ORDER BY id DESC",
				wantGo:         []float64{6, 5, 2, 1},
				wantJavaLeft:   []float64{1, 6},
				wantJavaRight:  []float64{5, 2},
				leftLeg:        pkLeft,
				rightLeg:       pkRight,
				distinguishing: true,
				why:            "the divergence under test: Java's LEFT leg stays natural-ascending while its RIGHT leg is DESC — the sort never reached the set operation; Go's whole result is DESC. Distinguishing because a descending interleaving would need the left leg to emit 6 before 1",
			},
			{
				name:           "trailing_order_by_asc_nonkey",
				sql:            "SELECT v FROM UOC UNION ALL SELECT v FROM UOD ORDER BY v ASC",
				wantGo:         []float64{10, 20, 50, 60},
				wantJavaLeft:   []float64{60, 10},
				wantJavaRight:  []float64{20, 50},
				leftLeg:        vLeft,
				rightLeg:       vRight,
				distinguishing: true,
				why:            "the ASC direction with real coverage: ordering on a NON-key column whose natural order descends, so Java's left leg emits 60 before 10 and no ascending interleaving exists",
			},
			{
				name:           "trailing_order_by_asc_pk",
				sql:            "SELECT id FROM UOA UNION ALL SELECT id FROM UOB ORDER BY id ASC",
				wantGo:         []float64{1, 2, 5, 6},
				wantJavaLeft:   []float64{1, 6},
				wantJavaRight:  []float64{2, 5},
				leftLeg:        pkLeft,
				rightLeg:       pkRight,
				distinguishing: false,
				why:            "ASC over PK legs: kept for Go's exact combined-sort sequence, but it CANNOT carry the not-the-combined-sort claim — both legs are naturally ascending, so 1,2,5,6 is a legal right-leg-only interleaving and the claim would fire on an unchanged Java",
			},
			{
				name:           "no_order_by_control",
				sql:            "SELECT id FROM UOA UNION ALL SELECT id FROM UOB",
				wantGo:         []float64{1, 6, 2, 5},
				wantJavaLeft:   []float64{1, 6},
				wantJavaRight:  []float64{2, 5},
				leftLeg:        pkLeft,
				rightLeg:       pkRight,
				distinguishing: false,
				why:            "control: with no trailing ORDER BY each leg is natural on BOTH engines — if this reds, the data or the harness moved, not the sort. Java's LEG INTERLEAVE is deliberately not asserted; it was measured to vary run to run",
			},
			{
				name:           "single_leg_desc_control",
				sql:            "SELECT id FROM UOA ORDER BY id DESC",
				wantGo:         []float64{6, 1},
				wantJavaLeft:   []float64{6, 1},
				wantJavaRight:  []float64{},
				leftLeg:        pkLeft,
				rightLeg:       pkRight,
				distinguishing: false,
				why:            "control: ORDER BY DESC on ONE leg is honoured identically by both engines — isolates the divergence to the SET-operation binding rather than to sorting itself",
			},
		}

		var problems []string
		for _, p := range probes {
			java := javaRunner.RunWithSetup(ctx, schema, setup, p.sql)
			goSide := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			if java.Err != nil || goSide.Err != nil {
				problems = append(problems, fmt.Sprintf(
					"%s: an engine failed where the probe expects rows (java_err=%v go_err=%v)\n    %s",
					p.name, java.Err, goSide.Err, p.sql))
				continue
			}
			javaIDs := unionOrderIDs(java)
			goIDs := unionOrderIDs(goSide)
			mark := "  "
			fail := func(f string, a ...any) {
				mark = "!!"
				problems = append(problems, fmt.Sprintf("%s: %s (%s)\n    %s",
					p.name, fmt.Sprintf(f, a...), p.why, p.sql))
			}

			// Same rows on both sides, before any order claim: an order
			// divergence is only meaningful over identical multisets.
			wantRows := unionSorted(p.wantGo)
			if fmt.Sprint(unionSorted(javaIDs)) != fmt.Sprint(wantRows) ||
				fmt.Sprint(unionSorted(goIDs)) != fmt.Sprint(wantRows) {
				fail("engines no longer return the same rows: java=%v go=%v want multiset %v",
					javaIDs, goIDs, wantRows)
			}
			// Go: the combined sort, exactly.
			if fmt.Sprint(goIDs) != fmt.Sprint(p.wantGo) {
				fail("GO order changed: got %v want %v", goIDs, p.wantGo)
			}
			// Java: per-leg, interleave-robust.
			if got := unionSubseq(javaIDs, p.leftLeg); fmt.Sprint(got) != fmt.Sprint(p.wantJavaLeft) {
				fail("JAVA left-leg order changed: got %v want %v", got, p.wantJavaLeft)
			}
			if got := unionSubseq(javaIDs, p.rightLeg); fmt.Sprint(got) != fmt.Sprint(p.wantJavaRight) {
				fail("JAVA right-leg order changed: got %v want %v", got, p.wantJavaRight)
			}
			// The divergence itself, stated directly — only where the fixture
			// makes it sound. Without this a future Java that sorted the
			// combined result would still satisfy every per-leg claim above
			// (each leg's subsequence is sorted under a global sort too) and
			// the entry would silently stop being true.
			if p.distinguishing && fmt.Sprint(javaIDs) == fmt.Sprint(p.wantGo) {
				fail("JAVA now returns the COMBINED sort %v — the divergence is GONE and the watch-list entry can be retired", javaIDs)
			}
			fmt.Fprintf(GinkgoWriter, "%s %-28s java=%-18v go=%-18v %s\n",
				mark, p.name, javaIDs, goIDs, p.sql)
		}

		Expect(problems).To(BeEmpty(),
			"the UNION ALL trailing-ORDER-BY divergence is no longer what the watch-list records.\n"+
				"Re-read the watch-list entry against this measurement before touching the assertion.\n"+
				strings.Join(problems, "\n"))
	})
})
