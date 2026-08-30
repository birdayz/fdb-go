package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// progressEvery is how often a sweep prints a progress line, in CASES.
const progressEvery = 500

// huntBudget is the wall-clock a sweep gives itself before stopping cleanly and
// reporting what it actually covered. HUNT_BUDGET overrides it.
//
// A seed COUNT is the wrong bound for a sweep and this was learned by paying
// for it twice. Both times the range was sized from a throughput measured on an
// idle box; both times other work was running by the time it mattered. The
// first run spent 1h57m and died on the Go test timeout having printed nothing
// at all. The second, after the progress fix, still died at 90 minutes — but at
// 2000 of 9000 seeds, so the number that mattered (what got swept) was
// recoverable from the log rather than from the exit status.
//
// A budget makes the seed count an UPPER bound, which is what it always was in
// practice, and makes the run's own report the authority on coverage. It also
// removes the failure mode where a panic-on-timeout is indistinguishable in the
// exit code from a sweep that found a real defect.
//
// This is the same shape as the nightly's ROWDIFF_BUDGET, which was dismissed
// here as "the nightly's semantics" before two timeouts demonstrated it is
// simply how a long sweep has to end.
func huntBudget() time.Duration {
	if v := os.Getenv("HUNT_BUDGET"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 20 * time.Minute
}

// classifyExcluded names why the factory's TLP eligibility filter refuses a
// query, or "" when it would be accepted. Multi-label: a query can be refused
// for several reasons at once, and a single-label switch undercounts every
// class behind the one it tests first.
func classifyExcluded(q rowdiff.Query) []string {
	var out []string
	if q.Agg != nil {
		out = append(out, "aggregate")
	}
	if q.Union != nil {
		out = append(out, "union")
	}
	if q.Derived != nil {
		out = append(out, "derived-table")
	}
	if q.Distinct {
		out = append(out, "distinct")
	}
	if q.Limit > 0 {
		out = append(out, "limit")
	}
	if q.Offset > 0 {
		out = append(out, "offset")
	}
	if q.Where == nil {
		out = append(out, "no-where")
	}
	return out
}

// TestFDB_ExcludedShapeHunt is a DENSE Oracle-M differential aimed at exactly
// the query shapes the committed factory corpus can never contain.
//
// The factory blesses with ternary-logic partitioning, and TLP cannot
// partition an aggregate, a UNION, a derived table, a DISTINCT or a LIMIT — so
// factory.Candidates drops all of them before any oracle runs. Measured over
// seeds 1..4000, that is 52.3% of every query the generator emits, and the
// resulting corpus has ZERO scenarios covering GROUP BY, UNION, subqueries in
// FROM, DISTINCT or LIMIT.
//
// Oracle M (rowdiff.OracleRows) has no such restriction — it evaluates all of
// those shapes against an in-memory reference. This test keeps only the
// excluded queries and runs them, so a fixed time budget buys roughly twice
// the density on the least-covered surface than an unfiltered sweep does.
func TestFDB_ExcludedShapeHunt(t *testing.T) {
	t.Parallel()
	byClass := runShapeHunt(t, "EXCL", true,
		uint64(envInt("EXCL_SEED_START", 1)), uint64(envInt("EXCL_SEEDS", 40)), envInt("EXCL_WORKERS", 4))

	// The coverage floors live HERE, in the test that owns the intent, and not
	// inside the helper keyed on the helper's own onlyExcluded parameter.
	//
	// That earlier arrangement produced a guard that could not fire: flipping
	// the flag simply selected the other branch's floors, which passed, so a
	// mutation flipping FullShapeHunt's filter back ON was GREEN. A guard
	// selected by the thing it is meant to detect is not a guard. Asserting
	// from the caller means each test states what its own run must have
	// covered, and a flipped flag reddens the test whose claim became false.
	for _, want := range []string{"aggregate", "union", "derived-table", "distinct", "limit"} {
		if byClass[want] == 0 {
			t.Errorf("EXCL VACUOUS: class %q never appeared — this hunt exists to cover it", want)
		}
	}
	if n := byClass["tlp-eligible"]; n != 0 {
		t.Errorf("EXCL swept %d TLP-eligible queries — the filter is off, so this run is no longer "+
			"the DENSE sweep of the excluded shapes it claims to be", n)
	}
}

// TestFDB_FullShapeHunt is the same harness with the shape filter OFF.
//
// It exists for THROUGHPUT, not for a different oracle. rowdiff's own
// runRowdiffSweep walks its range with one closure per seed, sequentially, and
// measured 2326 seeds in 3006s -- 0.8 seeds/s. This harness drives the SAME
// rowdiff.RunCase across N workers and measured 5.4 seeds/s at 6 workers on the
// same box, so a fixed hunting budget walks roughly six times the range.
//
// It is deliberately NOT a replacement for the nightly sweep. That loop carries
// a wall-clock budget, a coverage floor and an INFRA circuit breaker, all
// written against measured failures (a wedged container that cost five hours,
// and a sweep that could only end by timeout). This one has none of that and is
// an interactive instrument.
func TestFDB_FullShapeHunt(t *testing.T) {
	t.Parallel()
	byClass := runShapeHunt(t, "FULLSHAPE", false,
		uint64(envInt("FULL_SEED_START", 1)), uint64(envInt("FULL_SEEDS", 25)), envInt("FULL_WORKERS", 4))

	// This sweep's whole point is that it ALSO walks the shapes the factory
	// accepts, so a zero here means the filter was left on and the run silently
	// duplicates the excluded-shape hunt.
	if byClass["tlp-eligible"] == 0 {
		t.Error("FULLSHAPE VACUOUS: no TLP-eligible query was swept — the shape filter is on, so " +
			"this run duplicates TestFDB_ExcludedShapeHunt rather than extending it")
	}
	// And it must still reach the excluded classes, or it is not a FULL sweep.
	for _, want := range []string{"aggregate", "union", "distinct", "limit"} {
		if byClass[want] == 0 {
			t.Errorf("FULLSHAPE VACUOUS: class %q never appeared in an unfiltered sweep", want)
		}
	}
}

// TestFDB_NestedShapeHunt sweeps the NESTED generator axis, which nothing else
// sweeps against a live engine.
//
// rowdiff.GenerateNested builds cases whose table carries a STRUCT column and
// whose projections and predicates name dotted paths. Measured by grep over
// pkg/: its only non-test caller is factory/nested.go, reached solely through
// cmd/factory-run's `-nested` flag, and nightly-factory.yml never passes it.
// The rowdiff nightly runs RunSeed, which calls the FLAT Generate. So the
// nested axis has 35 committed corpus files acting as frozen pins and NO
// ongoing differential at all: a new nested defect is caught only if one of
// those already-frozen scenarios happens to cover it.
//
// Same oracle and same harness as the flat sweeps — only the generator differs.
func TestFDB_NestedShapeHunt(t *testing.T) {
	t.Parallel()
	byClass := runShapeHuntGen(t, "NESTED", false, rowdiff.GenerateNested,
		uint64(envInt("NEST_SEED_START", 1)), uint64(envInt("NEST_SEEDS", 25)), envInt("NEST_WORKERS", 4))

	// A nested sweep that swept no query at all would report the same clean
	// green as a correct engine, and the nested generator is the one most
	// likely to produce an empty case (a struct column the shape builder
	// declines to project).
	total := 0
	for _, v := range byClass {
		total += v
	}
	if total == 0 {
		t.Error("NESTED VACUOUS: the nested generator produced no queries to sweep")
	}
}

// runShapeHunt sweeps a seed range against Oracle M and returns the per-class
// census of what it actually swept.
//
// onlyExcluded keeps just the queries factory.Candidates refuses; false sweeps
// every generated query. The census is RETURNED rather than asserted here on
// purpose — see the comment in TestFDB_ExcludedShapeHunt for the guard that
// could not fire when these floors lived inside this function.
func runShapeHunt(t *testing.T, tag string, onlyExcluded bool, start, count uint64, workers int) map[string]int {
	t.Helper()
	return runShapeHuntGen(t, tag, onlyExcluded, rowdiff.Generate, start, count, workers)
}

// runShapeHuntGen is runShapeHunt with the case generator injected, so the flat
// and nested axes share one harness rather than one being a copy of the other
// that drifts.
func runShapeHuntGen(
	t *testing.T, tag string, onlyExcluded bool,
	gen func(uint64) *rowdiff.Case,
	start, count uint64, workers int,
) map[string]int {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	began := time.Now()
	var (
		mu         sync.Mutex
		mismatches []*rowdiff.Mismatch
		executed   int
		cases      int
		byClass    = map[string]int{}
		byArm      = map[int]int{}
		infraErrs  int
	)

	seeds := make(chan uint64)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			// DRAIN on every exit path. A worker that returns early on a
			// setup failure stops reading `seeds`, and if every worker does
			// so the producer blocks on an unbuffered send until the Go test
			// timeout kills the process -- which is precisely the
			// panic-instead-of-report ending the wall-clock budget was added
			// to eliminate. Draining turns a dead worker into a fast, honest
			// `walked=N` report instead of a hang.
			defer func() {
				for range seeds {
				}
				wg.Done()
			}()
			setupDB, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+clusterFilePath+"&schema=CATALOG")
			if err != nil {
				t.Errorf("worker %d: open sys: %v", w, err)
				return
			}
			defer setupDB.Close()
			ctx := context.Background()
			// Namespaced by TAG as well as worker: the two entry points are
			// both t.Parallel(), so a shared database path would have one
			// hunt dropping the other's schemas mid-sweep.
			dbPath := fmt.Sprintf("/%s_%d", tag, w)
			if _, err := setupDB.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
				t.Errorf("worker %d: create database: %v", w, err)
				return
			}
			defer setupDB.ExecContext(ctx, "DROP DATABASE "+dbPath) //nolint:errcheck

			for seed := range seeds {
				c := gen(seed)
				var kept []rowdiff.Query
				local := map[string]int{}
				for _, q := range c.Queries {
					cls := classifyExcluded(q)
					if onlyExcluded && len(cls) == 0 {
						continue
					}
					kept = append(kept, q)
					if len(cls) == 0 {
						// Swept by the full-shape hunt only. Counted under its
						// own name so the class census still sums to the
						// queries actually run — an unlabelled query would
						// make the ledger quietly disagree with `executed`.
						local["tlp-eligible"]++
						continue
					}
					for _, k := range cls {
						local[k]++
					}
				}
				if len(kept) == 0 {
					continue
				}
				c.Queries = kept

				// Two arms per case, and the second is the point.
				//
				// scanLimit 0 runs each query unpaged. scanLimit 2 pins a
				// connection with OptExecutionScannedRowsLimit so every query
				// breaks and resumes through the continuation machinery while
				// still returning the complete answer.
				//
				// The CROSS is what is new here. Aggregates, DISTINCT, UNION
				// and LIMIT all carry state that a page boundary can drop — a
				// running aggregate, a dedup set, a window offset — and this
				// repo has already paid for two of those (see
				// agg_continuation_straddle_fdb_test.go and
				// distinct_crosspage_fdb_test.go, both written to pin a
				// cross-page defect after the fact). The committed corpus
				// contains none of these shapes at all, so nothing sweeps them
				// against a reference under paging.
				for _, scanLimit := range []int{0, 2} {
					id := fmt.Sprintf("%s%d_%d_s%d", tag, w, seed, scanLimit)
					r := rowdiff.RunCase(ctx, setupDB, dbPath, clusterFilePath, c, id, scanLimit)

					mu.Lock()
					cases++
					executed += r.Executed
					byArm[scanLimit] += r.Executed
					// Report a MISMATCH the moment it is found, and a progress
					// line periodically. A hunt that prints only its summary is
					// all-or-nothing: a 60000-seed run that is killed, wedged or
					// still in flight is indistinguishable from one that found
					// nothing, and its whole hour is unrecoverable. Measured the
					// hard way — a 67-minute run of this test produced not one
					// line of output while it worked.
					for _, m := range r.Mismatches {
						fmt.Printf("%s MISMATCH seed=%d scanLimit=%d\n%s\n", tag, seed, scanLimit, m.String())
					}
					// Every 500 cases, with elapsed and rate. The interval is
					// tuned for a LOCAL grind, where the operator is sizing the
					// next run from the last one's throughput: a report every
					// 2000 cases told you nothing for the first several minutes
					// of a sweep, which is exactly when you want to know whether
					// the range you picked will finish.
					if cases%progressEvery == 0 {
						el := time.Since(began)
						fmt.Printf("%s progress cases=%d executed=%d mismatches=%d elapsed=%s rate=%.1f seeds/s\n",
							tag, cases, executed, len(mismatches)+len(r.Mismatches),
							el.Round(time.Second), float64(cases)/2/el.Seconds())
					}
					mismatches = append(mismatches, r.Mismatches...)
					if r.InfraErr != nil {
						infraErrs++
					}
					mu.Unlock()
				}
				mu.Lock()
				for k, v := range local {
					byClass[k] += v
				}
				mu.Unlock()
			}
		}(w)
	}
	budget := huntBudget()
	deadline := began.Add(budget)
	var walked uint64
	for s := start; s < start+count; s++ {
		if time.Now().After(deadline) {
			fmt.Printf("%s BUDGET EXHAUSTED after %s: walked %d of %d seeds. This is a NORMAL end, "+
				"not a failure — the seed count is an upper bound and the clock is what sizes the run. "+
				"Coverage below is what was actually swept.\n",
				tag, budget, walked, count)
			break
		}
		seeds <- s
		walked++
	}
	close(seeds)
	wg.Wait()

	fmt.Printf("%s seeds=%d..%d walked=%d cases=%d executed=%d infra-errs=%d mismatches=%d\n",
		tag, start, start+count-1, walked, cases, executed, infraErrs, len(mismatches))
	var keys []string
	for k := range byClass {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%s --- query classes swept ---\n", tag)
	for _, k := range keys {
		fmt.Printf("%s   %-16s %6d\n", tag, k, byClass[k])
	}

	// Vacuity guards. A hunt over an empty population reports the same green
	// as a clean engine, and this one filters its own input, so an over-strict
	// filter would silently sweep nothing.
	if cases == 0 {
		t.Fatalf("%s VACUOUS: no seed produced a query to sweep — the filter matched nothing", tag)
	}
	if executed == 0 {
		t.Fatalf("%s VACUOUS: zero query executions compared", tag)
	}
	// Each arm is floored SEPARATELY. Summing them lets one arm go to zero
	// while the total stays healthy — which is exactly how the paged arm, the
	// one this hunt was extended for, would silently stop running.
	for _, arm := range []int{0, 2} {
		if byArm[arm] == 0 {
			t.Fatalf("%s VACUOUS: arm scanLimit=%d executed zero queries", tag, arm)
		}
	}
	fmt.Printf("%s arms: unpaged=%d paged(scanLimit=2)=%d\n", tag, byArm[0], byArm[2])

	for i, m := range mismatches {
		fmt.Printf("%s MISMATCH %d:\n%s\n", tag, i, m.String())
	}
	if len(mismatches) > 0 {
		t.Fatalf("%s: %d Oracle-M mismatches", tag, len(mismatches))
	}
	return byClass
}
