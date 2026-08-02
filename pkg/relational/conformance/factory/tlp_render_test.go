package factory_test

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestTLPRenderingsPlan is the load-bearing premise of the whole TLP oracle:
// every rendering of the ternary partition must PLAN, or the branch that
// cannot plan silently removes itself from the oracle.
//
// Planning needs no FDB, so this costs seconds and runs in the default tier —
// which is the point. Without it, a grammar or translation change that stops
// accepting one branch does not fail: the factory just starts skipping every
// candidate with "engine error", reports a clean run, and commits nothing.
//
// It also pins the other two renderings. `NOT (p)` composed as a conjunct
// beside a comma join's equality and an appended EXISTS depends on NOT binding
// tighter than AND; a precedence mistake there would not fail to plan, it
// would plan a DIFFERENT query and report the engine as wrong.
func TestTLPRenderingsPlan(t *testing.T) {
	t.Parallel()

	// Seeds are planned concurrently. Each rendering is an independent planner
	// call — the entry point's package state was removed so parallel tests
	// could share it — and planning ~1600 of them serially under -race put this
	// target at the edge of its Bazel budget, where box variance alone decided
	// whether the lane was red.
	perLabel := map[string]int{}
	failures := map[string]int{}
	var samples []string
	var mu sync.Mutex

	workers := min(runtime.GOMAXPROCS(0), 4)
	seeds := make(chan uint64)
	go func() {
		defer close(seeds)
		for seed := uint64(1); seed <= 40; seed++ {
			seeds <- seed
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seed := range seeds {
				for _, cand := range factory.Candidates(seed) {
					ddl := cand.Case.DDL()
					for i, q := range cand.TLPQueries() {
						sqlText := cand.Case.SQL(q, cand.Projection)
						label := factory.TLPLabels[i]
						// A Fatal here would exit only this goroutine and let
						// the run report a pass on renderings never checked.
						missingIsNull := i == 3 && !strings.Contains(sqlText, ") IS NULL")
						_, planErr := embedded.PlanPhysicalForTest(sqlText, ddl, nil)

						mu.Lock()
						perLabel[label]++
						if missingIsNull {
							t.Errorf("seed %d %s: the p-is-null rendering lacks `) IS NULL`: %s",
								seed, cand.Name(), sqlText)
						}
						if planErr != nil {
							failures[label]++
							if len(samples) < 3 {
								samples = append(samples,
									fmt.Sprintf("seed %d %s: %v\n  SQL: %s", seed, label, planErr, sqlText))
							}
						}
						mu.Unlock()
					}
				}
			}
		}()
	}
	wg.Wait()

	labels := make([]string, 0, len(perLabel))
	for l := range perLabel {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	for _, l := range labels {
		t.Logf("%-11s planned %4d, failed %4d", l, perLabel[l], failures[l])
	}

	if perLabel["p-is-null"] == 0 {
		t.Fatal("zero `p IS NULL` renderings were produced — the partition's third branch does not exist")
	}
	total := 0
	for _, n := range failures {
		total += n
	}
	if total > 0 {
		t.Fatalf("%d TLP renderings failed to plan; the partition oracle cannot run on a rendering the planner rejects. Samples:\n%s",
			total, strings.Join(samples, "\n"))
	}
}
