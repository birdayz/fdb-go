// Package full runs the ENTIRE committed factory corpus as ordinary suite
// content — it is in every `just test` and every CI run (owner ruling
// 2026-08-01, recorded in rfcs/201-layered-test-corpus.md §7).
//
// It is a separate package from the corpus's loader/census gates so that
// those stay a pure no-FDB target; this target is the one that pays for a
// container and executes every committed scenario. The nightly corpus job
// re-runs the same target on master as the scheduled heartbeat.
//
// Run it alone:
//
//	bazelisk test //pkg/relational/conformance/factorycorpus/full:full_test \
//	  --test_output=streamed --nocache_test_results
package full_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"

	// The corpus runner opens database/sql connections against the "fdbsql"
	// driver; the import registers it in this test binary.
	_ "fdb.dev/pkg/relational/sqldriver"
)

// corpusDir is the parent package's testdata. The corpus lives there, with the
// loader and the census, because it is the corpus package's content — this
// package is only a second execution target over it.
const corpusDir = "../testdata"

var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		if os.Getenv("CI") != "" {
			fmt.Fprintf(os.Stderr, "FATAL: FDB container startup failed in CI — the full factory corpus would silently skip: %v\n", err)
			os.Exit(1)
		}
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster file: %v\n", err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "fdb-factorycorpus-full-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "write cluster file: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	clusterFilePath = tmp.Name()

	os.Exit(m.Run())
}

// TestFDB_FactoryCorpusFull executes every committed scenario.
func TestFDB_FactoryCorpusFull(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	files, err := factorycorpus.LoadDir(corpusDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	t.Logf("executing the full committed corpus: %d scenarios", len(files))

	// A scenario's own failure text is printed where it happens, buried in one
	// subtest among thousands. The summary below is what makes a red legible:
	// it is written to stderr, unbuffered and last, after every parallel
	// subtest has reported, so the scenario names land at the end of the stream
	// no matter how much framing preceded them. Bounded by construction — see
	// factorycorpus.FailureSummary.
	var mu sync.Mutex
	var failed []factorycorpus.ScenarioFailure
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if s := factorycorpus.FailureSummary(failed, len(files), 0); s != "" {
			fmt.Fprint(os.Stderr, "\n"+s)
		}
	})

	for _, f := range files {
		f := f
		t.Run(f.Header.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), factorycorpus.ScenarioTimeout)
			defer cancel()
			res := factorycorpus.RunScenario(ctx, clusterFilePath, f)
			// CheckResult owns the vacuous-pass floor too: a scenario that ran
			// NOTHING passes every row assertion, because zero failures out of
			// zero tests is zero failures — the loudest possible instrument
			// failure wearing the quietest possible output. It demands every
			// committed stanza actually asserted.
			if err := factorycorpus.CheckResult(f, res); err != nil {
				mu.Lock()
				failed = append(failed, factorycorpus.ScenarioFailure{
					Name: f.Header.Name, Path: f.Path,
					Seed: f.Header.Seed, Date: f.Header.Date, Err: err,
				})
				mu.Unlock()
				t.Fatal(err)
			}
		})
	}
}
