package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

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
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	start := uint64(envInt("EXCL_SEED_START", 1))
	count := uint64(envInt("EXCL_SEEDS", 40))
	workers := envInt("EXCL_WORKERS", 4)

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
			defer wg.Done()
			setupDB, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+clusterFilePath+"&schema=CATALOG")
			if err != nil {
				t.Errorf("worker %d: open sys: %v", w, err)
				return
			}
			defer setupDB.Close()
			ctx := context.Background()
			dbPath := fmt.Sprintf("/EXCL_%d", w)
			if _, err := setupDB.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
				t.Errorf("worker %d: create database: %v", w, err)
				return
			}
			defer setupDB.ExecContext(ctx, "DROP DATABASE "+dbPath) //nolint:errcheck

			for seed := range seeds {
				c := rowdiff.Generate(seed)
				var kept []rowdiff.Query
				local := map[string]int{}
				for _, q := range c.Queries {
					cls := classifyExcluded(q)
					if len(cls) == 0 {
						continue
					}
					kept = append(kept, q)
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
					id := fmt.Sprintf("x%d_%d_s%d", w, seed, scanLimit)
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
						fmt.Printf("EXCL MISMATCH seed=%d scanLimit=%d\n%s\n", seed, scanLimit, m.String())
					}
					if cases%2000 == 0 {
						fmt.Printf("EXCL progress cases=%d executed=%d mismatches=%d\n",
							cases, executed, len(mismatches)+len(r.Mismatches))
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
	for s := start; s < start+count; s++ {
		seeds <- s
	}
	close(seeds)
	wg.Wait()

	fmt.Printf("EXCL seeds=%d..%d cases=%d executed=%d infra-errs=%d mismatches=%d\n",
		start, start+count-1, cases, executed, infraErrs, len(mismatches))
	var keys []string
	for k := range byClass {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println("EXCL --- excluded query classes swept ---")
	for _, k := range keys {
		fmt.Printf("EXCL   %-16s %6d\n", k, byClass[k])
	}

	// Vacuity guards. A hunt over an empty population reports the same green
	// as a clean engine, and this one filters its own input, so an over-strict
	// filter would silently sweep nothing.
	if cases == 0 {
		t.Fatal("EXCL VACUOUS: no seed produced an excluded query — the filter matched nothing")
	}
	if executed == 0 {
		t.Fatal("EXCL VACUOUS: zero query executions compared")
	}
	// Each arm is floored SEPARATELY. Summing them lets one arm go to zero
	// while the total stays healthy — which is exactly how the paged arm, the
	// one this hunt was extended for, would silently stop running.
	for _, arm := range []int{0, 2} {
		if byArm[arm] == 0 {
			t.Fatalf("EXCL VACUOUS: arm scanLimit=%d executed zero queries", arm)
		}
	}
	fmt.Printf("EXCL arms: unpaged=%d paged(scanLimit=2)=%d\n", byArm[0], byArm[2])
	for _, want := range []string{"aggregate", "union", "derived-table", "distinct", "limit"} {
		if byClass[want] == 0 {
			t.Fatalf("EXCL VACUOUS: class %q never appeared — this hunt exists to cover it", want)
		}
	}

	for i, m := range mismatches {
		fmt.Printf("EXCL MISMATCH %d:\n%s\n", i, m.String())
	}
	if len(mismatches) > 0 {
		t.Fatalf("EXCL: %d Oracle-M mismatches on shapes the factory corpus cannot contain", len(mismatches))
	}
}
