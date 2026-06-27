package yamsql_test

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

// TestFeatureMatrixUpToDate is the anti-rot guard for FEATURE_MATRIX.md: it
// regenerates the matrix from the testdata corpus and fails if the committed
// file differs. This is what makes the matrix "generated" rather than a
// hand-maintained doc that silently goes stale (P1.7). No Docker needed.
//
// File location mirrors pkg/docscheck: testdata is read via the package-relative
// glob (staged into runfiles by `data`), and FEATURE_MATRIX.md is found by
// walking up to MODULE.bazel (staged as a `data` dep at the runfiles root).
func TestFeatureMatrixUpToDate(t *testing.T) {
	t.Parallel()

	got, err := yamsql.GenerateFeatureMatrix("testdata")
	if err != nil {
		t.Fatalf("generate feature matrix: %v", err)
	}

	root := repoRootForMatrix(t)
	want, err := os.ReadFile(filepath.Join(root, "FEATURE_MATRIX.md"))
	if err != nil {
		t.Fatalf("read FEATURE_MATRIX.md: %v", err)
	}

	if string(want) != got {
		t.Fatalf("FEATURE_MATRIX.md is stale — run `just feature-matrix` and commit the result "+
			"(committed %d bytes, regenerated %d bytes)", len(want), len(got))
	}
}

// repoRootForMatrix walks up from the working dir to the directory holding
// MODULE.bazel. Under `go test` the cwd is this package's dir; under Bazel the
// doc + MODULE.bazel are `data` deps staged at the runfiles root (the test cwd's
// ancestor), so the same walk finds them.
func repoRootForMatrix(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "MODULE.bazel")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("MODULE.bazel not found walking up from %q; under Bazel, MODULE.bazel and "+
				"FEATURE_MATRIX.md must be `data` deps of yamsql_test", dir)
		}
		dir = parent
	}
}
