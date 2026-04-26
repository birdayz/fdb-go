package plandiff

// Tests for goSQLRunner — both the stub fallback (no cluster file)
// and the real-FDB smoke path. The stub fallback runs in any
// environment; the smoke path skips when Docker / FDB is unavailable.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
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

// TestGoSQLRunner_SeedRunCorpus_Values drives every SeedRunCorpus
// entry through the real Go embedded engine and asserts that the
// row-VALUES match the entry's Expected (Java-side baseline).
//
// Column metadata (names + types) is checked separately in
// TestGoSQLRunner_SeedRunCorpus_ColumnGaps — there are documented
// Java↔Go divergences (identifier-case, synthetic projection names,
// qualifier stripping, ResultSetMetaData type-name inference) that
// are real conformance gaps in the Go embedded engine, not bugs in
// the runner. Splitting the assertions surfaces semantic-VALUE
// regressions immediately while keeping the metadata gaps visible
// without failing the build.
//
// See CLAUDE.md "Java↔Go conformance gotchas" for the full list.
func TestGoSQLRunner_SeedRunCorpus_Values(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	for _, q := range SeedRunCorpus() {
		q := q
		t.Run(q.Name, func(t *testing.T) {
			t.Parallel()
			got := r.RunWithSetup(context.Background(), q.SchemaTemplate, q.SetupSqls, q.Query)
			if got.Err != nil {
				// Distinguish "Go engine doesn't support this feature
				// yet" (skip with note — surfaces the gap) from "Go
				// engine should work but errored" (real failure).
				if isGoFeatureGap(got.Err) {
					t.Skipf("Go-engine feature gap: %v", got.Err)
				}
				t.Fatalf("Go engine failed: %v\nQuery: %s", got.Err, q.Query)
			}
			if len(got.Rows.Rows) != len(q.Expected.Rows) {
				t.Fatalf("row count mismatch: got %d, want %d\ngot rows: %v\nwant rows: %v",
					len(got.Rows.Rows), len(q.Expected.Rows),
					got.Rows.Rows, q.Expected.Rows)
			}
			for i := range got.Rows.Rows {
				gotRow := got.Rows.Rows[i]
				wantRow := q.Expected.Rows[i]
				if len(gotRow) != len(wantRow) {
					t.Fatalf("row %d width mismatch: got %d, want %d", i, len(gotRow), len(wantRow))
				}
				for j := range gotRow {
					if gotRow[j] != wantRow[j] {
						t.Errorf("row %d col %d: got %v (%T), want %v (%T)",
							i, j, gotRow[j], gotRow[j], wantRow[j], wantRow[j])
					}
				}
			}
		})
	}
}

// TestGoSQLRunner_SeedRunCorpus_ColumnGaps logs (does NOT fail on)
// column-metadata divergences between the Go embedded engine and
// the Java reference. Each entry's failure is one of the documented
// gaps in CLAUDE.md "Java↔Go conformance gotchas":
//
//   - Identifier case: Go preserves "id", Java uppercases to "ID".
//   - Anonymous projections: Go uses raw expression text
//     ("COUNT(*)", "id+10"), Java uses synthetic "_0", "_1".
//   - Qualified columns: Go returns "t.id" / "u.name", Java
//     strips qualifiers.
//   - Type-name inference: the Go runner infers from value shapes
//     because the embedded driver doesn't expose JDBC type names;
//     DOUBLE values come through as float64 and infer to "BIGINT".
//   - Empty result set: no row = no inferred type ("ID|" vs
//     "ID|BIGINT").
//
// This subtest is intentionally a t.Logf-only audit. When the Go
// engine catches up on a gap, the corresponding entry will start
// agreeing. When all entries agree, fold the assertion into
// _Values and delete this audit.
func TestGoSQLRunner_SeedRunCorpus_ColumnGaps(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	mismatches := 0
	for _, q := range SeedRunCorpus() {
		got := r.RunWithSetup(context.Background(), q.SchemaTemplate, q.SetupSqls, q.Query)
		if got.Err != nil {
			continue
		}
		if !columnsMatch(got.Rows.Columns, q.Expected.Columns) {
			mismatches++
			t.Logf("[%s] columns differ:\n  got:  %v\n  want: %v",
				q.Name, got.Rows.Columns, q.Expected.Columns)
		}
	}
	t.Logf("column-metadata gaps: %d / %d entries", mismatches, len(SeedRunCorpus()))
}

// isGoFeatureGap recognises errors that mean "the Go embedded engine
// doesn't support this feature yet" (vs. an unexpected runtime error).
// Today's gaps:
//   - "unsupported column type": DDL parser-level rejection.
//   - "unsupported DataType code": metadata-builder-level rejection
//     for types whose DDL is accepted but proto-mapping isn't wired
//     (e.g. UUID after swingshift-52's ddl.go fix — the parser accepts
//     UUID, the proto-builder doesn't yet map it to a field type).
//
// Extend as new gap categories emerge. Each entry points at a tracked
// TODO.md item so closing it removes the skip path.
func isGoFeatureGap(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unsupported column type") ||
		strings.Contains(s, "unsupported DataType code")
}

func columnsMatch(a, b []Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
