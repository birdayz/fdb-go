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
// divergence is asserted where it actually lives: within one leg. Under a
// trailing `ORDER BY id DESC` Java's RIGHT-leg values come back descending
// while its LEFT-leg values stay in natural ascending order — that is exactly
// "only the right leg was sorted" — whereas Go's full sequence is the combined
// sort and is deterministic.
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

// unionOrderIDs flattens a single-column RunResult to the id sequence IN THE
// ORDER THE ENGINE RETURNED IT. Order is the whole subject here, so nothing may
// sort or canonicalise on the way through.
func unionOrderIDs(r plandiff.RunResult) []float64 {
	ids := make([]float64, 0, len(r.Rows.Rows))
	for _, row := range r.Rows.Rows {
		Expect(row).To(HaveLen(1), "probe expects single-column rows")
		f, ok := row[0].(float64)
		Expect(ok).To(BeTrue(), fmt.Sprintf("non-numeric id %#v", row[0]))
		ids = append(ids, f)
	}
	return ids
}

// unionSubseq returns the members of ids that belong to one leg, in the order
// the engine emitted them. Leg membership is by VALUE because the two legs hold
// disjoint values by construction.
func unionSubseq(ids []float64, leg map[float64]bool) []float64 {
	out := make([]float64, 0, len(ids))
	for _, v := range ids {
		if leg[v] {
			out = append(out, v)
		}
	}
	return out
}

// unionSorted is the multiset of ids, canonicalised — used only to assert the
// engines returned the SAME ROWS, never to assert order.
func unionSorted(ids []float64) []float64 {
	out := append([]float64(nil), ids...)
	sort.Float64s(out)
	return out
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

		// Values are chosen so the two readings are DISTINGUISHABLE: under a
		// combined sort the legs interleave (6,5,2,1 descending), under a
		// right-leg-only sort each leg keeps its own order. The legs are also
		// DISJOINT in value so a row can be attributed to its leg. Equal or
		// non-overlapping-but-monotone ranges would let both readings produce
		// the same sequence and the probe would measure nothing.
		leftLeg := map[float64]bool{1: true, 6: true}
		rightLeg := map[float64]bool{2: true, 5: true}
		schema := "CREATE TABLE UOA (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE UOB (id BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO UOA VALUES (1), (6)",
			"INSERT INTO UOB VALUES (2), (5)",
		}
		allRows := []float64{1, 2, 5, 6}

		type probe struct {
			name, sql string
			// wantGo is Go's FULL sequence: Go sorts the combined result, so
			// its output is deterministic and pinned exactly.
			wantGo []float64
			// wantJavaLeft/wantJavaRight are Java's PER-LEG subsequences — the
			// interleave-robust form of "only the right leg was sorted".
			wantJavaLeft, wantJavaRight []float64
			why                         string
		}
		probes := []probe{
			{
				name:          "trailing_order_by_desc",
				sql:           "SELECT id FROM UOA UNION ALL SELECT id FROM UOB ORDER BY id DESC",
				wantGo:        []float64{6, 5, 2, 1},
				wantJavaLeft:  []float64{1, 6},
				wantJavaRight: []float64{5, 2},
				why:           "the divergence under test: Java's LEFT leg stays natural-ascending while its RIGHT leg is DESC — the sort never reached the set operation; Go's whole result is DESC",
			},
			{
				name:          "trailing_order_by_asc",
				sql:           "SELECT id FROM UOA UNION ALL SELECT id FROM UOB ORDER BY id ASC",
				wantGo:        []float64{1, 2, 5, 6},
				wantJavaLeft:  []float64{1, 6},
				wantJavaRight: []float64{2, 5},
				why:           "same divergence in the ASC direction; here the per-leg orders coincide with natural order, so ONLY Go's combined sequence distinguishes the readings — ASC must not be mistaken for agreement",
			},
			{
				name:          "no_order_by_control",
				sql:           "SELECT id FROM UOA UNION ALL SELECT id FROM UOB",
				wantGo:        []float64{1, 6, 2, 5},
				wantJavaLeft:  []float64{1, 6},
				wantJavaRight: []float64{2, 5},
				why:           "control: with no trailing ORDER BY each leg is natural on BOTH engines — if this reds, the data or the harness moved, not the sort. Java's LEG INTERLEAVE is deliberately not asserted here; it was measured to vary run to run",
			},
			{
				name:          "single_leg_desc_control",
				sql:           "SELECT id FROM UOA ORDER BY id DESC",
				wantGo:        []float64{6, 1},
				wantJavaLeft:  []float64{6, 1},
				wantJavaRight: []float64{},
				why:           "control: ORDER BY DESC on ONE leg is honoured identically by both engines — isolates the divergence to the SET-operation binding rather than to sorting itself",
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
			if p.name != "single_leg_desc_control" {
				if fmt.Sprint(unionSorted(javaIDs)) != fmt.Sprint(allRows) ||
					fmt.Sprint(unionSorted(goIDs)) != fmt.Sprint(allRows) {
					fail("engines no longer return the same rows: java=%v go=%v", javaIDs, goIDs)
				}
			}
			// Go: the combined sort, exactly.
			if fmt.Sprint(goIDs) != fmt.Sprint(p.wantGo) {
				fail("GO order changed: got %v want %v", goIDs, p.wantGo)
			}
			// Java: per-leg, interleave-robust.
			if got := unionSubseq(javaIDs, leftLeg); fmt.Sprint(got) != fmt.Sprint(p.wantJavaLeft) {
				fail("JAVA left-leg order changed: got %v want %v", got, p.wantJavaLeft)
			}
			if got := unionSubseq(javaIDs, rightLeg); fmt.Sprint(got) != fmt.Sprint(p.wantJavaRight) {
				fail("JAVA right-leg order changed: got %v want %v", got, p.wantJavaRight)
			}
			// The divergence itself, stated directly: for the sorted union
			// shapes Java's sequence must NOT be the combined sort. Without
			// this a future Java that sorted the combined result would still
			// satisfy every per-leg claim above (each leg's subsequence is
			// sorted under a global sort too) and the entry would silently
			// stop being true.
			if strings.HasPrefix(p.name, "trailing_order_by") &&
				fmt.Sprint(javaIDs) == fmt.Sprint(p.wantGo) {
				fail("JAVA now returns the COMBINED sort %v — the divergence is GONE and the watch-list entry can be retired", javaIDs)
			}
			fmt.Fprintf(GinkgoWriter, "%s %-26s java=%-16v go=%-16v %s\n",
				mark, p.name, javaIDs, goIDs, p.sql)
		}

		Expect(problems).To(BeEmpty(),
			"the UNION ALL trailing-ORDER-BY divergence is no longer what the watch-list records.\n"+
				"Re-read the watch-list entry against this measurement before touching the assertion.\n"+
				strings.Join(problems, "\n"))
	})
})
