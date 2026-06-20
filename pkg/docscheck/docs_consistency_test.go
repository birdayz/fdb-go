package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// RFC-131 — documentation & source-of-truth drift guards. These run as ordinary unit
// tests (no Docker, no cgo) so every CI / pre-commit fails if a "living" doc drifts from
// the MODULE.bazel-pinned versions or reintroduces a known contradiction.
//
// File location: repoRoot walks up from the working directory to the dir holding
// MODULE.bazel. Under `go test` the cwd is this package's dir; under Bazel the doc files
// are `data` deps staged at the runfiles root (also the test's cwd), so the same walk-up
// finds them. No t.Skip: a missing MODULE.bazel is a hard failure, not a skip.

func repoRoot(t *testing.T) string {
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
			t.Fatalf("MODULE.bazel not found walking up from the working dir; under Bazel the doc files must be `data` deps of this test target")
		}
		dir = parent
	}
}

func readDoc(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

var (
	// fourPartVersion matches a Java-style x.y.z.w version (the record-layer scheme).
	fourPartVersion = regexp.MustCompile(`\b\d+\.\d+\.\d+\.\d+\b`)
	// recordLayerPin extracts the fdb-record-layer-core version from MODULE.bazel.
	recordLayerPin = regexp.MustCompile(`fdb-record-layer-core:(\d+\.\d+\.\d+\.\d+)`)
)

// javaTarget is the single source of truth: the fdb-record-layer-core pin in MODULE.bazel.
func javaTarget(t *testing.T, root string) string {
	t.Helper()
	m := recordLayerPin.FindStringSubmatch(readDoc(t, root, "MODULE.bazel"))
	if m == nil {
		t.Fatalf("could not parse fdb-record-layer-core version from MODULE.bazel")
	}
	return m[1]
}

// livingDocs are the docs that must always reflect current truth.
var livingDocs = []string{"README.md", "PRODUCTION_READINESS.md", "TODO.md", "DIVERGENCES.md"}

// TestLivingDocsCiteCurrentJavaTarget: any 4-part (record-layer-style) version string in a
// living doc must equal the MODULE.bazel pin. Anchored to the pin (not a stale-string
// blocklist), so it catches 4.2.6.0, 4.3.x, or any future drift — and a MODULE.bazel bump
// that forgets a living doc goes red.
func TestLivingDocsCiteCurrentJavaTarget(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	want := javaTarget(t, root)
	for _, doc := range livingDocs {
		body := readDoc(t, root, doc)
		for _, v := range fourPartVersion.FindAllString(body, -1) {
			if v != want {
				t.Errorf("%s cites 4-part version %q, but MODULE.bazel pins fdb-record-layer-core %q — living docs must track the pin (archived snapshots belong under docs/archive/)", doc, v, want)
			}
		}
	}
}

// TestREADMENoEscapeHatchContradiction: the README must not deny a "drop-in escape hatch"
// while documenting the `-tags libfdbc` one (the exact RFC-131 contradiction).
func TestREADMENoEscapeHatchContradiction(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	readme := readDoc(t, root, "README.md")
	documentsLibfdbc := strings.Contains(readme, "libfdbc")
	deniesEscapeHatch := regexp.MustCompile(`(?i)no\s+(drop-in\s+)?escape hatch`).MatchString(readme)
	if documentsLibfdbc && deniesEscapeHatch {
		t.Errorf("README documents the `-tags libfdbc` escape hatch but also denies it (matches /no (drop-in )?escape hatch/) — the RFC-131 contradiction reappeared")
	}
}

// TestStaleReportsArchived: the six 2026-03-09 audit snapshots must live under
// docs/archive/ (not reports/), with the archive's superseded-snapshot header present.
func TestStaleReportsArchived(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	const archive = "docs/archive/reports-2026-03-09"
	reports := []string{
		"behavior_compat_audit.md", "conformance_coverage.md", "feature_completeness.md",
		"go_style_audit.md", "subspace_wire_compat.md", "wire_compat_audit.md",
	}
	for _, f := range reports {
		if _, err := os.Stat(filepath.Join(root, "reports", f)); err == nil {
			t.Errorf("reports/%s is a stale 2026-03-09 snapshot — it must be archived under %s/", f, archive)
		}
		if _, err := os.Stat(filepath.Join(root, archive, f)); err != nil {
			t.Errorf("%s/%s missing (archive incomplete): %v", archive, f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, archive, "README.md")); err != nil {
		t.Errorf("%s/README.md (superseded-snapshot header) missing: %v", archive, err)
	}
}
