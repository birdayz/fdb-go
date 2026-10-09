// Package full runs the entire committed factory corpus in `just test-full`
// and non-race PR CI. The fast local lane retains the loader/census gates.
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
	"path/filepath"
	"sort"
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

// The corpus runs as fullShards top-level tests, one per Bazel shard
// (rules_go shards by top-level test function; shard_count in BUILD.bazel
// must equal fullShards). One process replaying all ~8000 scenarios was the
// suite's longest pole: 873 s on CI, most of it after every other target had
// finished, with the other cores idle. Each shard is its own process with its
// own container and loads only its own files.
const fullShards = 8

func TestFDB_FactoryCorpusFull0(t *testing.T) { runShard(t, 0) }
func TestFDB_FactoryCorpusFull1(t *testing.T) { runShard(t, 1) }
func TestFDB_FactoryCorpusFull2(t *testing.T) { runShard(t, 2) }
func TestFDB_FactoryCorpusFull3(t *testing.T) { runShard(t, 3) }
func TestFDB_FactoryCorpusFull4(t *testing.T) { runShard(t, 4) }
func TestFDB_FactoryCorpusFull5(t *testing.T) { runShard(t, 5) }
func TestFDB_FactoryCorpusFull6(t *testing.T) { runShard(t, 6) }
func TestFDB_FactoryCorpusFull7(t *testing.T) { runShard(t, 7) }

// shardFiles assigns the corpus files to fullShards shards, largest file
// first onto the lightest shard (by bytes, a proxy for scenario count), and
// returns shard's files. Every file lands in exactly one shard;
// TestFDB_FactoryCorpusShardsCoverTheCorpus pins that.
func shardFiles(t *testing.T, shard int) []string {
	t.Helper()
	assign := assignShards(t)
	var out []string
	for path, s := range assign {
		if s == shard {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func assignShards(t *testing.T) map[string]int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(corpusDir, "*.yamsql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob %s: %v (%d files): an empty corpus passes vacuously", corpusDir, err, len(paths))
	}
	type file struct {
		path string
		size int64
	}
	files := make([]file, len(paths))
	for i, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		files[i] = file{p, st.Size()}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].size != files[j].size {
			return files[i].size > files[j].size
		}
		return files[i].path < files[j].path
	})
	var load [fullShards]int64
	assign := make(map[string]int, len(files))
	for _, f := range files {
		best := 0
		for s := 1; s < fullShards; s++ {
			if load[s] < load[best] {
				best = s
			}
		}
		assign[f.path] = best
		load[best] += f.size
	}
	return assign
}

// TestFDB_FactoryCorpusShardsCoverTheCorpus pins the partition: every
// committed file belongs to exactly one shard, so the shards together run the
// whole corpus once.
func TestFDB_FactoryCorpusShardsCoverTheCorpus(t *testing.T) {
	corpus, err := filepath.Glob(filepath.Join(corpusDir, "*.yamsql"))
	if err != nil || len(corpus) == 0 {
		t.Fatalf("glob %s: %v (%d files)", corpusDir, err, len(corpus))
	}
	seen := map[string]int{}
	for s := 0; s < fullShards; s++ {
		for _, f := range shardFiles(t, s) {
			if prev, dup := seen[f]; dup {
				t.Fatalf("%s is in shard %d and shard %d", f, prev, s)
			}
			seen[f] = s
		}
	}
	for _, f := range corpus {
		if _, ok := seen[f]; !ok {
			t.Fatalf("%s is in no shard: the shards would not run it", f)
		}
	}
}

// runShard executes every committed scenario of one shard.
func runShard(t *testing.T, shard int) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	var files []*factorycorpus.Scenario
	for _, path := range shardFiles(t, shard) {
		f, err := factorycorpus.Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		files = append(files, f.Scenarios...)
	}
	if len(files) == 0 {
		t.Fatalf("shard %d of %d has no scenarios: it would pass vacuously", shard, fullShards)
	}
	t.Logf("executing shard %d of %d: %d scenarios", shard, fullShards, len(files))
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
