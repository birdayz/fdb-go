package plandiff

// Tests for goSQLRunner — both the stub fallback (no cluster file)
// and the real-FDB smoke path. The stub fallback runs in any
// environment; the smoke path skips when Docker / FDB is unavailable.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// goSQLClusterFilePath is set by TestMain when an FDB testcontainer is
// available. Empty value means "skip integration tests".
var goSQLClusterFilePath string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// No Docker — run non-FDB tests only.
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ClusterFile: %v\n", err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "fdb-plandiff-*.cluster")
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(clusterContent); err != nil {
		fmt.Fprintf(os.Stderr, "WriteString: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	goSQLClusterFilePath = tmp.Name()

	os.Exit(m.Run())
}

// TestGoSQLRunner_NoClusterFileFallback pins the no-FDB contract:
// NewGoSQLRunner("") returns the stub runner (NewGoRunner) so callers
// in CI without FDB still get a deterministic ErrGoUnimplemented.
func TestGoSQLRunner_NoClusterFileFallback(t *testing.T) {
	t.Parallel()
	r := NewGoSQLRunner("")
	got := r.Run(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if !errors.Is(got.Err, ErrGoUnimplemented) {
		t.Fatalf("expected ErrGoUnimplemented, got %v", got.Err)
	}

	sr := NewGoSQLSetupRunner("")
	got = sr.RunWithSetup(context.Background(), "", nil, "SELECT 1")
	if !errors.Is(got.Err, ErrGoUnimplemented) {
		t.Fatalf("expected ErrGoUnimplemented from RunWithSetup, got %v", got.Err)
	}
}

// TestGoSQLRunner_HappyPath drives a simple INSERT-then-SELECT flow
// through the real Go embedded engine: ephemeral schema lifecycle +
// setup + query, then asserts the RowSet shape and values match the
// per-entry expectation. Skips when no FDB is available.
func TestGoSQLRunner_HappyPath(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE T (id BIGINT, name STRING, PRIMARY KEY (id))",
		[]string{
			"INSERT INTO T VALUES (1, 'alice'), (2, 'bob')",
		},
		"SELECT id, name FROM T ORDER BY id",
	)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Columns) != 2 {
		t.Fatalf("Columns: got %d, want 2 (%+v)", len(got.Rows.Columns), got.Rows.Columns)
	}
	if len(got.Rows.Rows) != 2 {
		t.Fatalf("Rows: got %d, want 2 (%+v)", len(got.Rows.Rows), got.Rows.Rows)
	}
	// Row 0: {1, "alice"} — coerced to {float64(1), "alice"} for
	// JSON-compatible comparison with the Java side.
	if got.Rows.Rows[0][0] != float64(1) {
		t.Fatalf("Rows[0][0]: got %v (%T), want 1", got.Rows.Rows[0][0], got.Rows.Rows[0][0])
	}
	if got.Rows.Rows[0][1] != "alice" {
		t.Fatalf("Rows[0][1]: got %v, want alice", got.Rows.Rows[0][1])
	}
	if got.Rows.Rows[1][0] != float64(2) {
		t.Fatalf("Rows[1][0]: got %v, want 2", got.Rows.Rows[1][0])
	}
	if got.Rows.Rows[1][1] != "bob" {
		t.Fatalf("Rows[1][1]: got %v, want bob", got.Rows.Rows[1][1])
	}
}

// TestGoSQLRunner_ArrayCellIsNormalised pins the coercion against the shape the
// DRIVER actually produces, not the shape the coercion's unit tests assume.
//
// The unit tests in struct_cell_coercion_test.go drive every arm with fakes,
// and a fake proves the arm works — not that the arm is the one real results
// take. That distinction went wrong once already here: the coercion was first
// written with an api.Array arm only, and materializeDriverValue hands an ARRAY
// column back as []any, so the arm real queries needed did not exist and every
// array cell fell to the pass-through default with its elements unnormalised.
// An array of BIGINTs stayed []any{int64…}, which can never equal Java's
// []any{float64…} — a permanent false divergence on every array-valued column,
// reported as a semantic disagreement.
//
// So this asserts the END-TO-END shape: a real ARRAY column, through the real
// runner, must arrive as a []any of float64 — the JSON-decoded form the Java
// side produces — and not as int64s, a pointer, or anything else.
func TestGoSQLRunner_ArrayCellIsNormalised(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE T (id BIGINT, xs BIGINT ARRAY, PRIMARY KEY (id))",
		[]string{"INSERT INTO T VALUES (1, [10, 20])"},
		"SELECT xs FROM T WHERE id = 1",
	)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Rows) != 1 || len(got.Rows.Rows[0]) != 1 {
		t.Fatalf("row shape: got %+v, want one row of one cell", got.Rows.Rows)
	}
	cell := got.Rows.Rows[0][0]
	arr, ok := cell.([]any)
	if !ok {
		t.Fatalf("an ARRAY cell arrived as %T, not []any. Whatever the driver now returns, the "+
			"coercion needs an arm for it — the pass-through default compares it as a raw Go "+
			"value, which no Java rendering can equal.\n  cell: %#v", cell, cell)
	}
	want := []any{float64(10), float64(20)}
	if !reflect.DeepEqual(arr, want) {
		t.Fatalf("an ARRAY cell was not normalised element-wise\n  got  %#v\n  want %#v\n"+
			"  (int64 elements can never equal Java's JSON-decoded float64s, so every "+
			"array-valued column would read as a permanent divergence)", arr, want)
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

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
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

// TestGoSQLRunner_NullPassThrough pins NULL handling: a SELECT on a
// NULL column produces nil in the resulting RowSet (matches Java's
// JSON-NULL encoding).
func TestGoSQLRunner_NullPassThrough(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE T (id BIGINT, name STRING, PRIMARY KEY (id))",
		[]string{
			"INSERT INTO T VALUES (1, 'alice'), (2, NULL)",
		},
		"SELECT id, name FROM T ORDER BY id",
	)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Rows) != 2 {
		t.Fatalf("Rows: got %d, want 2 (%+v)", len(got.Rows.Rows), got.Rows.Rows)
	}
	if got.Rows.Rows[1][1] != nil {
		t.Fatalf("Rows[1][1]: got %v (%T), want nil", got.Rows.Rows[1][1], got.Rows.Rows[1][1])
	}
}

func TestGoSQLRunner_BytesINList(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE lb (a BIGINT, b BYTES, PRIMARY KEY (a))",
		[]string{
			"INSERT INTO lb VALUES (1, X'deadbeef'), (2, X'cafe'), (3, null)",
		},
		"SELECT a FROM lb WHERE b IN (X'cafe', X'deadbeef') ORDER BY a",
	)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Rows) != 2 {
		t.Fatalf("Rows: got %d, want 2 (%+v)", len(got.Rows.Rows), got.Rows.Rows)
	}
	if got.Rows.Rows[0][0] != float64(1) {
		t.Fatalf("Row[0]: got %v (%T), want 1", got.Rows.Rows[0][0], got.Rows.Rows[0][0])
	}
	if got.Rows.Rows[1][0] != float64(2) {
		t.Fatalf("Row[1]: got %v (%T), want 2", got.Rows.Rows[1][0], got.Rows.Rows[1][0])
	}
}
