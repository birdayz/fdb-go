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
	// (bazel_dep(name = "foundationdb", version = "7.3.77")).
	foundationDBPin = regexp.MustCompile(`name\s*=\s*"foundationdb"\s*,\s*version\s*=\s*"(\d+\.\d+\.\d+)"`)
	// fdbContextCited matches a 3-part version that FOLLOWS an FDB-client keyword on the same line
	// (within 30 non-digit chars). This anchors on context, not the major number, so it catches FDB
	// drift across ANY major bump (a stale 7.x left after an 8.x bump still sits beside an FDB
	// keyword) without a numeric range that would hit Bazel 9.x. Case-sensitive "FDB" dodges the
	// lowercase "fdb-record-layer-core" (the Java artifact); the \n in the negated class keeps the
	// next table row's Bazel version out of reach.
	fdbContextCited = regexp.MustCompile(`(?:FoundationDB|foundationdb|libfdb_c|FDB)[^0-9\n]{0,30}?(\d+\.\d+\.\d+)`)
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

var changelogHeading = regexp.MustCompile(`(?m)^## \[[^\]]*\]`)

// unreleasedSection returns the CHANGELOG text from "## [Unreleased]" up to the next "## ["
// release heading (or EOF) — the Unreleased entry's body. Used to require the Compatibility
// block INSIDE Unreleased (not satisfied by a tagged entry's heading).
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

// currentChangelog returns the changelog PREAMBLE + the Unreleased section — everything up to
// the first TAGGED release heading. The preamble (which carries a current Java-target claim) and
// Unreleased must track the live pins; only tagged entries freeze their own versions and are
// excluded, so a later pin bump doesn't force a history rewrite.
func currentChangelog(changelog string) string {
	for _, loc := range changelogHeading.FindAllStringIndex(changelog, -1) {
		if changelog[loc[0]:loc[1]] != "## [Unreleased]" {
			return changelog[:loc[0]] // first tagged entry → cut before it
		}
	}
	return changelog // no tagged release yet
}

// versionScanBody is the portion of a living doc the version anchors check. For CHANGELOG.md
// that's the preamble + Unreleased (tagged release entries snapshot their own versions); every
// other living doc is scanned whole.
func versionScanBody(t *testing.T, root, doc string) string {
	t.Helper()
	body := readDoc(t, root, doc)
	if doc == "CHANGELOG.md" {
		return currentChangelog(body)
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
// gap that the 4-part Java anchor can't catch a stale 3-part FDB version (RFC-132).
func TestLivingDocsCiteCurrentFDBVersion(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	want := pin(t, root, foundationDBPin, "MODULE.bazel", "foundationdb")
	for _, doc := range livingDocs {
		body := versionScanBody(t, root, doc)
		for _, m := range fdbContextCited.FindAllStringSubmatchIndex(body, -1) {
			v := body[m[2]:m[3]]
			// Skip a 4-part version's 3-part prefix (e.g. the Java 4.11.1.0 — its own anchor
			// handles it): only when the capture is followed by '.<digit>' (a genuine fourth
			// part), NOT a bare trailing period like "...7.3.77." ending a sentence.
			if m[3]+1 < len(body) && body[m[3]] == '.' && body[m[3]+1] >= '0' && body[m[3]+1] <= '9' {
				continue
			}
			if v != want {
				t.Errorf("%s cites FDB version %q next to an FDB keyword, but MODULE.bazel pins foundationdb %q — living docs must track the pin", doc, v, want)
			}
		}
	}
}

// TestLivingDocsCiteCurrentGoVersion: any "Go x.y" reference in a living doc must share the go.mod
// toolchain major.minor (Go 1.26.x and Go 1.26.4 both pass; Go 1.25 fails). Closes the Go-version
// half of the gap (RFC-132).
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
	// release entry's heading. A freshly-opened Unreleased without it must go red.
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

// readmeSQLGapsHeading is the README's gap-list heading — the boundary between the
// supported-SQL bullet list and the known-gap list. The full sentence (not the bare
// "Not yet supported") is the delimiter so a cross-reference to the gap list from
// inside the supported list cannot be mistaken for the heading itself.
const readmeSQLGapsHeading = "Not yet supported in the SQL engine:"

// readmeSQLSections splits the README's SQL summary into (supported list, gap list).
// Both lists mention rejected features — one to point at the gap list, the other to
// explain the gap — so a whole-file substring check cannot tell a claim of support
// from a documented limitation; the section boundary can.
func readmeSQLSections(t *testing.T, readme string) (supported, gaps string) {
	t.Helper()
	start := strings.Index(readme, "Supported SQL")
	if start < 0 {
		t.Fatalf("README.md has no `Supported SQL` section — the SQL surface summary was renamed or removed")
	}
	rest := readme[start:]
	end := strings.Index(rest, readmeSQLGapsHeading)
	if end < 0 {
		t.Fatalf("README.md has no %q heading following `Supported SQL`", readmeSQLGapsHeading)
	}
	return rest[:end], rest[end:]
}

var (
	// inSubqueryMention matches any way the README names the feature: the literal
	// `IN (SELECT`/`NOT IN (SELECT` (case-insensitive, tolerating inner spaces) or
	// the compound noun `IN-subquery` / `IN subqueries`. The compound arm is
	// case-SENSITIVE on the SQL keyword so ordinary prose ("described in
	// subqueries below") cannot trip it.
	inSubqueryMention = regexp.MustCompile(`(?i)\bIN\s*\(\s*SELECT`)
	inSubqueryNoun    = regexp.MustCompile(`\bIN[-\s]subquer`)
	// negatedClaim is the explicit-negation vocabulary a supported-list bullet must
	// use if it mentions the feature at all. Deliberately a small closed set: the
	// contract is "say `not supported` / `unsupported` / `rejected`", not "write
	// any sentence a parser might read as negative".
	negatedClaim = regexp.MustCompile(`(?i)\b(?:not\s+supported|unsupported|rejected|no\s+support)\b`)
	// markdownEmphasis is stripped before the negation check so `is **not**
	// supported` reads as `is not supported`.
	markdownEmphasis = regexp.MustCompile("[*`]")
	// bulletStart marks a markdown list item; a bullet plus its continuation lines
	// is the unit a claim is judged in.
	bulletStart = regexp.MustCompile(`(?m)^\s*[-*]\s`)
)

// markdownBlocks splits a section into the units a claim is judged in: every
// blank-line-separated paragraph, with a bullet list further split into one block
// per list item (a `- ` line plus its continuation lines). Judging per block rather
// than per sentence is what lets a mention and its negation be matched up —
// sentence splitting is unusable here because `IN (SELECT ...)` contains literal
// periods. Nothing in the section is dropped, so a claim cannot hide in surrounding
// prose or ride along inside a neighbouring bullet's negation.
func markdownBlocks(section string) []string {
	var out []string
	for _, para := range strings.Split(section, "\n\n") {
		locs := bulletStart.FindAllStringIndex(para, -1)
		if len(locs) == 0 {
			out = append(out, para)
			continue
		}
		if pre := para[:locs[0][0]]; strings.TrimSpace(pre) != "" {
			out = append(out, pre)
		}
		for i, loc := range locs {
			end := len(para)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			out = append(out, para[loc[0]:end])
		}
	}
	return out
}

// mentionsInSubquery reports whether text names the IN-subquery feature in any form.
func mentionsInSubquery(text string) bool {
	return inSubqueryMention.MatchString(text) || inSubqueryNoun.MatchString(text)
}

// TestREADMEDoesNotAdvertiseInSubquery: `x IN (SELECT ...)` is rejected everywhere by
// BOTH engines — Java's ExpressionVisitor.visitInPredicate asserts
// `inList().queryExpressionBody() == null` (UNSUPPORTED_QUERY), and Go returns 0AF00;
// the yamsql corpus pins the rejection in every shape. The README once listed it in
// the supported-SQL bullet list while its own gap list documented the DML half as
// rejected, so the document contradicted itself and sent users at a guaranteed 0AF00.
//
// The check is on the CLAIM, not a literal string: a supported-list bullet may name
// the feature only if the same bullet explicitly negates it, so both re-advertising
// it verbatim and rewording it ("EXISTS / NOT EXISTS, IN-subqueries, and correlated
// scalar subqueries") are caught, while a correct negation passes. The negation
// vocabulary is a small closed set (negatedClaim) — the gate is biased toward
// catching a re-advertisement, so a bullet that negates the feature in words outside
// that set fails and must be reworded rather than silently accepted.
func TestREADMEDoesNotAdvertiseInSubquery(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	supported, gaps := readmeSQLSections(t, readDoc(t, root, "README.md"))

	for _, block := range markdownBlocks(supported) {
		plain := markdownEmphasis.ReplaceAllString(block, "")
		if !mentionsInSubquery(plain) || negatedClaim.MatchString(plain) {
			continue
		}
		t.Errorf("README's `Supported SQL` section names the IN-subquery without negating it, but "+
			"both engines reject `x IN (SELECT ...)` (Go: 0AF00; Java: UNSUPPORTED_QUERY \"IN "+
			"predicate does not support nested SELECT\") and the yamsql corpus pins the rejection in "+
			"every shape. Offending block:\n%s", strings.TrimRight(block, "\n"))
	}

	if !mentionsInSubquery(gaps) {
		t.Errorf("README's gap list no longer documents the `IN (SELECT ...)` limitation — it is " +
			"still rejected by both engines, so dropping the entry hides a real gap")
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
