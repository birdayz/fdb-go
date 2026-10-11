package seedcorpus

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/plandiff"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// goSQLClusterFilePath is set by TestMain when an FDB testcontainer is
// available. Empty value means "skip integration tests".
var goSQLClusterFilePath string

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

// runTests returns the exit code so its deferred cleanup runs before os.Exit.
func runTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// No Docker — run non-FDB tests only.
		return m.Run()
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	path, err := container.ClusterFilePath(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFilePath: %v\n", err)
		return 1
	}
	defer os.Remove(path)
	goSQLClusterFilePath = path

	return m.Run()
}

// TestSeedCorpus_GoEngineSucceedsOnAll asserts the naive Go generator
// produces a non-empty plan tree for every seed query without error.
// A failure here means a corpus entry parses but the planner refuses
// it — the harness loses a regression sentinel.
func TestSeedCorpus_GoEngineSucceedsOnAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng := plandiff.NewGoEngine()
	corpus := SeedCorpus()
	for _, q := range corpus {
		q := q
		t.Run(q.Name, func(t *testing.T) {
			t.Parallel()
			r := eng.Plan(ctx, q)
			if r.Err != nil {
				t.Fatalf("Go engine errored on %s: %v\n  sql: %s", q.Name, r.Err, q.SQL)
			}
			if r.Tree == "" {
				t.Fatalf("Go engine produced empty tree for %s\n  sql: %s", q.Name, q.SQL)
			}
			if r.Hash == "" {
				t.Fatalf("Go engine produced empty hash for %s", q.Name)
			}
		})
	}
}

// TestRun_AllJavaUnimplemented pins that Run with the stub Java engine
// classifies every case as StatusJavaUnimplemented (not StatusGoError /
// StatusBothError). This lets a CI gate that wants to be Go-only
// tolerant filter on Status. The single Total==JavaUnimplemented check
// covers both the "all Java unimpl" and "no Go errors" properties —
// if Total matches JavaUnimplemented, no other status (including
// GoError) can be non-zero.
func TestRun_AllJavaUnimplemented(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	report := plandiff.Run(ctx, SeedCorpus(), plandiff.NewGoEngine(), plandiff.NewJavaEngine())
	if report.Summary.Total != len(SeedCorpus()) {
		t.Fatalf("Total: got %d, want %d", report.Summary.Total, len(SeedCorpus()))
	}
	if report.Summary.JavaUnimplemented != report.Summary.Total {
		t.Fatalf("expected every case to be JAVA_UNIMPL, got %d/%d (go-err %d, both-err %d, diverge %d)",
			report.Summary.JavaUnimplemented, report.Summary.Total,
			report.Summary.GoError, report.Summary.BothError, report.Summary.Diverge)
	}
}

// TestGoSQLRunner_SeedRunCorpus is a Go-side runner smoke test: every
// SeedRunCorpus entry is driven through the embedded engine, and must not
// panic or hit a Go-engine type gap. Cross-engine byte-equivalence with Java
// is asserted in `conformance/run_sql_conformance_test.go` (the only place
// that has both the Java conformance server AND the FDB testcontainer in
// scope). Splitting the responsibilities keeps this package's tests running
// without a Java dependency.
//
// Query errors are tolerated: the corpus carries negative entries that Java
// rejects and Go rejects too, so a non-nil error is not by itself a failure.
// What is NOT tolerated is a Go-engine TYPE gap (isGoFeatureGap) — a DDL or
// metadata-builder rejection means the corpus declares a column type the Go
// engine cannot represent, and that must fail loudly rather than pass quietly.
func TestGoSQLRunner_SeedRunCorpus(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := plandiff.NewGoSQLSetupRunner(goSQLClusterFilePath)
	for _, q := range SeedRunCorpus() {
		q := q
		t.Run(q.Name, func(t *testing.T) {
			t.Parallel()
			// Go-runner-only smoke test: confirms each entry runs
			// through Go's embedded engine without panic. Whether Go's
			// behaviour matches Java is asserted in the cross-engine
			// conformance test (run_sql_conformance_test.go). Ordinary
			// query errors are tolerated — negative entries that Java
			// rejects also error in Go after alignment work.
			got := r.RunWithSetup(context.Background(), q.SchemaTemplate, q.SetupSqls, q.Query)
			if got.Err != nil && isGoFeatureGap(got.Err) {
				t.Fatalf("Go-engine type gap: %v — the corpus declares a column type the Go "+
					"engine cannot represent; close the gap rather than tolerating it", got.Err)
			}
		})
	}
}

// isGoFeatureGap recognises errors that mean "the Go embedded engine cannot
// represent this column type" (vs. an ordinary query rejection). Patterns:
//   - "unsupported column type": DDL parser-level rejection.
//   - "unsupported DataType code": metadata-builder-level rejection
//     for types whose DDL is accepted but proto-mapping isn't wired.
//
// No SeedRunCorpus entry hits this path today — every entry's schema uses a
// type the engine models end-to-end. It is a gate, not a tolerance: a corpus
// entry that introduces a not-yet-modelled type names the gap in the failure
// message instead of surfacing as an opaque strict-equivalence mismatch
// downstream.
func isGoFeatureGap(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unsupported column type") ||
		strings.Contains(s, "unsupported DataType code")
}
