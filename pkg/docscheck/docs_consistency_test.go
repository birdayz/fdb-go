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
	// foundationDBPin extracts the FDB C++ client version from MODULE.bazel
	// (bazel_dep(name = "foundationdb", version = "7.3.75")).
	foundationDBPin = regexp.MustCompile(`name\s*=\s*"foundationdb"\s*,\s*version\s*=\s*"(\d+\.\d+\.\d+)"`)
	// fdbThreePart matches an FDB-style 7.x.y version. The only 7.x.y version in the docs is
	// the FDB C++ client, so any 7.x.y that isn't the pin is FDB drift.
	fdbThreePart = regexp.MustCompile(`\b7\.\d+\.\d+\b`)
	// goPin extracts the Go toolchain major.minor from go.mod (go 1.26.4 -> 1.26).
	goPin = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)
	// goCited matches a "Go x.y" reference, tolerating markdown bold/backticks/table separators
	// and line wraps between "Go" and the version: "Go 1.26", "Go **1.26.x**", a wrapped
	// "Go\n  **1.26.x**", and a table row "| **Go** | **1.26.4** |" all capture 1.26. The char
	// class is restricted to `*`/backtick/`|`/whitespace, so prose like "Go to 1.2.3" (letters in
	// between) does NOT match — no false positive.
	goCited = regexp.MustCompile("(?i)\\bGo\\b[*`|\\s]{0,12}?(\\d+\\.\\d+)")
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
var livingDocs = []string{
	"README.md", "PRODUCTION_READINESS.md", "TODO.md", "DIVERGENCES.md",
	"CHANGELOG.md", "RELEASE.md",
}

func pin(t *testing.T, root string, re *regexp.Regexp, src, what string) string {
	t.Helper()
	m := re.FindStringSubmatch(readDoc(t, root, src))
	if m == nil {
		t.Fatalf("could not parse %s version from %s", what, src)
	}
	return m[1]
}

// unreleasedSection returns the CHANGELOG text from "## [Unreleased]" up to the next "## ["
// release heading (or EOF). Only this section must cite the CURRENT pins / carry the
// Compatibility block; a tagged release entry freezes its own versions and must NOT be
// force-rewritten when MODULE.bazel/go.mod later bump (codex #330).
func unreleasedSection(changelog string) string {
	const marker = "## [Unreleased]"
	i := strings.Index(changelog, marker)
	if i < 0 {
		return ""
	}
	rest := changelog[i+len(marker):]
	if j := strings.Index(rest, "\n## ["); j >= 0 {
		return rest[:j]
	}
	return rest
}

// versionScanBody is the portion of a living doc the version anchors check. For CHANGELOG.md
// that's only the Unreleased section (historical entries snapshot their own versions); every
// other living doc is scanned whole.
func versionScanBody(t *testing.T, root, doc string) string {
	t.Helper()
	body := readDoc(t, root, doc)
	if doc == "CHANGELOG.md" {
		return unreleasedSection(body)
	}
	return body
}

// TestLivingDocsCiteCurrentJavaTarget: any 4-part (record-layer-style) version string in a
// living doc must equal the MODULE.bazel pin. Anchored to the pin (not a stale-string
// blocklist), so it catches 4.2.6.0, 4.3.x, or any future drift — and a MODULE.bazel bump
// that forgets a living doc goes red.
func TestLivingDocsCiteCurrentJavaTarget(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	want := javaTarget(t, root)
	for _, doc := range livingDocs {
		body := versionScanBody(t, root, doc)
		for _, v := range fourPartVersion.FindAllString(body, -1) {
			if v != want {
				t.Errorf("%s cites 4-part version %q, but MODULE.bazel pins fdb-record-layer-core %q — living docs must track the pin (archived snapshots belong under docs/archive/)", doc, v, want)
			}
		}
	}
}

// TestLivingDocsCiteCurrentFDBVersion: any FDB-style 7.x.y version in a living doc must equal the
// foundationdb pin in MODULE.bazel (the only 7.x.y in the docs is the FDB C++ client). Closes the
// gap that the 4-part Java anchor can't catch a stale 3-part FDB version (RFC-132 / Torvalds).
func TestLivingDocsCiteCurrentFDBVersion(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	want := pin(t, root, foundationDBPin, "MODULE.bazel", "foundationdb")
	for _, doc := range livingDocs {
		body := versionScanBody(t, root, doc)
		for _, v := range fdbThreePart.FindAllString(body, -1) {
			if v != want {
				t.Errorf("%s cites FDB version %q, but MODULE.bazel pins foundationdb %q — living docs must track the pin", doc, v, want)
			}
		}
	}
}

// TestLivingDocsCiteCurrentGoVersion: any "Go x.y" reference in a living doc must share the go.mod
// toolchain major.minor (Go 1.26.x and Go 1.26.4 both pass; Go 1.25 fails). Closes the Go-version
// half of the gap (RFC-132 / Torvalds).
func TestLivingDocsCiteCurrentGoVersion(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	want := pin(t, root, goPin, "go.mod", "go toolchain")
	for _, doc := range livingDocs {
		body := versionScanBody(t, root, doc)
		for _, m := range goCited.FindAllStringSubmatch(body, -1) {
			if m[1] != want {
				t.Errorf("%s cites Go %q, but go.mod pins Go %q.x — living docs must track the toolchain pin", doc, m[1], want)
			}
		}
	}
}

// TestReleaseDocsExistAndCompat: the release machinery exists and the changelog can't silently drop
// the four compatibility notes the audit requires (RFC-132).
func TestReleaseDocsExistAndCompat(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	changelog := readDoc(t, root, "CHANGELOG.md")
	if !strings.Contains(changelog, "## [Unreleased]") {
		t.Errorf("CHANGELOG.md is missing an `## [Unreleased]` section")
	}
	// The Compatibility block must live INSIDE the Unreleased section — not satisfied by an old
	// release entry's heading (codex #330). A freshly-opened Unreleased without it must go red.
	if !strings.Contains(unreleasedSection(changelog), "### Compatibility") {
		t.Errorf("CHANGELOG.md `## [Unreleased]` is missing its `### Compatibility` block (wire/SQL/option/version notes)")
	}
	if rel := readDoc(t, root, "RELEASE.md"); len(strings.TrimSpace(rel)) == 0 {
		t.Errorf("RELEASE.md is empty")
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
