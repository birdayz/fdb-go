package factorycorpus_test

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

var clusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// In CI a container failure must be FATAL. A silent all-skip would
		// leave the corpus gate green while executing nothing, which is the
		// exact state a committed corpus exists to make impossible.
		if os.Getenv("CI") != "" {
			fmt.Fprintf(os.Stderr, "FATAL: FDB container startup failed in CI — the factory corpus would silently skip: %v\n", err)
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
	tmp, err := os.CreateTemp("", "fdb-factorycorpus-*.cluster")
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

// TestFDB_FactoryCorpusSample is the per-PR tier's slice of the committed
// corpus (RFC-201 §7): a deterministic, feature-vector-stratified sample, so
// every query dimension the corpus covers appears in every PR run.
//
// The FULL corpus runs in the sibling `full` package, which is `manual` and
// tagged out of the default suite. That split is the point: the whole corpus
// grows toward six figures and cannot live in a gate that must stay fast,
// while a gate that runs NOTHING from the corpus lets a batch land broken and
// stay broken until the nightly.
func TestFDB_FactoryCorpusSample(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	files, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	sample := factorycorpus.StratifiedSample(files, factorycorpus.SampleSize)
	if len(sample) == 0 {
		t.Fatal("the stratified sample is empty — this gate would pass over nothing")
	}

	dims := map[string]bool{}
	for _, f := range sample {
		dims[f.Header.FeatureVector] = true
	}
	t.Logf("running %d of %d committed scenarios, covering %d feature vectors", len(sample), len(files), len(dims))

	for _, f := range sample {
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
			if res.TestsRun == 0 {
				t.Fatalf("%s ran zero tests", f.Path)
			}
		})
	}
}
