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

// TestFeatureMatrixOutcomesMatchCoverage pins that the two generated inventories
// walk the same corpus: the outcome totals FEATURE_MATRIX.md reports must equal
// SQL_COVERAGE.md's, over the same scenario count. The per-case classification is
// structurally shared (both fold through classifyTests), so what remains worth
// asserting is that neither generator silently walks a different file set — a glob
// or tolerant-parse change in one would show up here as a total mismatch.
func TestFeatureMatrixOutcomesMatchCoverage(t *testing.T) {
	t.Parallel()

	scenarios, err := yamsql.ParseFeatureScenarios("testdata")
	if err != nil {
		t.Fatalf("parse feature scenarios: %v", err)
	}
	rep, err := yamsql.ParseCoverage("testdata")
	if err != nil {
		t.Fatalf("parse coverage: %v", err)
	}

	var grand yamsql.CoverageBucket
	for _, s := range scenarios {
		grand.Add(s.Outcomes)
	}
	if grand != rep.Total {
		t.Errorf("feature-matrix outcome totals %+v != SQL_COVERAGE totals %+v — the two docs "+
			"must inventory the same corpus", grand, rep.Total)
	}
	if len(scenarios) != rep.Scenarios {
		t.Errorf("feature-matrix counted %d scenarios, coverage counted %d", len(scenarios), rep.Scenarios)
	}
}

// TestFeatureMatrixDistinguishesRejections exercises the split on a concrete case:
// `x IN (SELECT ...)` is rejected by both engines, and the corpus scenarios that pin
// that rejection must report those cases as unsupported-feature pins. Before the
// outcome split these two scenarios contributed 22 cases to a bare "Cases" column
// that read as 22 cases of working IN-subquery support.
func TestFeatureMatrixDistinguishesRejections(t *testing.T) {
	t.Parallel()

	scenarios, err := yamsql.ParseFeatureScenarios("testdata")
	if err != nil {
		t.Fatalf("parse feature scenarios: %v", err)
	}
	// Both scenarios pin IN-subquery rejection plus a handful of supported
	// EXISTS/join rewrites; every non-supported case in them is an
	// unsupported-feature pin, never an unclassified case.
	for _, name := range []string{"subquery_in", "in_subquery_decomposition"} {
		var found bool
		for _, s := range scenarios {
			if s.Name != name {
				continue
			}
			found = true
			if s.Outcomes.Unsupported == 0 {
				t.Errorf("scenario %s: expected unsupported-feature pins (IN-subquery is rejected), got none", name)
			}
			if s.Outcomes.ErrorPath != 0 {
				t.Errorf("scenario %s: %d cases classified error-path; the IN-subquery rejections use 0AF00 "+
					"and must land in the unsupported bucket", name, s.Outcomes.ErrorPath)
			}
		}
		if !found {
			t.Errorf("scenario %s missing from the corpus inventory", name)
		}
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
