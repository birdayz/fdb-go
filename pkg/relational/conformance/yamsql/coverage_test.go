package yamsql_test

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

// TestSQLCoverageUpToDate is the anti-rot guard for SQL_COVERAGE.md (Ledger B):
// it regenerates the measured coverage report from the corpus and fails if the
// committed file differs. This is what makes the "% supported" a measurement
// rather than a hand-typed number that silently goes stale (the SQL_CONFORMANCE.md
// "115/115" failure mode). No Docker needed — same idiom as
// TestFeatureMatrixUpToDate (repoRootForMatrix lives in featurematrix_test.go).
func TestSQLCoverageUpToDate(t *testing.T) {
	t.Parallel()

	got, err := yamsql.GenerateCoverageReport("testdata")
	if err != nil {
		t.Fatalf("generate coverage report: %v", err)
	}

	root := repoRootForMatrix(t)
	want, err := os.ReadFile(filepath.Join(root, "SQL_COVERAGE.md"))
	if err != nil {
		t.Fatalf("read SQL_COVERAGE.md: %v", err)
	}

	if string(want) != got {
		t.Fatalf("SQL_COVERAGE.md is stale — run `just sql-coverage` and commit the result "+
			"(committed %d bytes, regenerated %d bytes)", len(want), len(got))
	}
}
