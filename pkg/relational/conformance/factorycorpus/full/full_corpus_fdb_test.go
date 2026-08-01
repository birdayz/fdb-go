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
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"

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

	for _, f := range files {
		f := f
		t.Run(f.Header.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), factorycorpus.ScenarioTimeout)
			defer cancel()
			db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s",
				factorycorpus.DBPathFor(f), clusterFilePath, factorycorpus.SchemaName))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer db.Close()
			res, err := factorycorpus.RunFile(ctx, db, f)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.SetupError != nil || res.TestsFail > 0 {
				t.Fatalf("committed expectation no longer holds:\n%s", factorycorpus.Describe(f, res))
			}
			// A scenario that ran NOTHING passes every assertion above, because
			// zero failures out of zero tests is zero failures. The nightly
			// would then report the whole corpus green while executing none of
			// it — the loudest possible instrument failure wearing the quietest
			// possible output. The per-PR sample tier already floors this; the
			// full tier is the one that must not be able to go vacuous.
			if res.TestsRun == 0 {
				t.Fatalf("%s ran zero tests: a scenario with no executed test cannot fail, "+
					"so this file has been reporting green without exercising the engine", f.Path)
			}
		})
	}
}
