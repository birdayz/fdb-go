package docscheck

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// RFC-175 F2 — CLAUDE.md's comment bans, enforced as CI instead of prose.
// Code comments explain WHY, never WHEN or WHO: shift-tags and review
// attributions belong in git blame, PR descriptions, and shifts/*.md handovers,
// never in permanent source. The bans reached 29 files while they lived only in
// CLAUDE.md text; this gate keeps the count at zero.
//
// Scope is defined by PROPERTY, not enumeration (RFC-175 §5 B1/B2): IN is the
// tracked-file set (`git ls-files '*.go'` — test files included; a filesystem
// walk is the fallback when git is unavailable), OUT is only generated code,
// detected by Go's official marker convention — any leading comment line before
// the package clause matching the `// Code generated … DO NOT EDIT.` form —
// never by path patterns. Path lists under-exclude (generators whose output
// lands outside gen/, e.g. wire/types/*_generated.go and api/mocks_*.go) and
// directory lists under-include (a new top-level dir must be covered
// automatically), so paths appear below only as fallback-walk skips for trees
// that cannot contain tracked Go (the Java checkout, hidden dirs, Bazel
// symlinks).
//
// Only COMMENT text is scanned — never string literals or identifiers — so a
// test fixture or variable name containing a banned word cannot false-positive.

// bannedCommentPatterns are the comment-content bans from CLAUDE.md ("Never put
// shift tags in code comments"; review attribution is git-blame's job).
// Names match on WORD BOUNDARIES, case-insensitively: narrower forms
// (name-plus-colon, parenthesized name) let bare mentions ("per <name> #330",
// "<name> review") survive the sweep. The WHO-ban has no exemption for the
// review gates — that includes the Cascades-review authority: cite the ROLE
// ("the architectural review gate"), the artifact (an RFC, a paper title), or
// the invariant — never the person.
// Literature/file-path citations that legitimately carry a name (e.g. the
// Cascades-paper notes under docs/) belong on the allowlist with a
// justification, not as a pattern exemption.
//
// The GENERIC spellings are banned on exactly the same footing as the named
// ones, and for the same reason: "the nit both reviewers flagged" and "review
// round 7 found" attribute a comment to WHO raised it and WHEN in the process,
// which is precisely what a permanent source comment must not carry. CLAUDE.md
// names them explicitly ("no `per @claude`", "no `review round 2`"), so a list
// that matched only the proper nouns enforced a strictly narrower rule than the
// one it documents — the gap that let an unnamed-but-still-WHO attribution
// through.
//
// The word carries no exemption for describing a future READER either: a
// comment that says "this points a reviewer at the failing file" is talking
// about an audience, but it is one edit away from talking about a person, and
// the distinction is not expressible as a pattern. Say "reader" — it is what is
// meant, and it stays true after the review is over. The one place the word is
// unavoidable is the prose of this gate itself, which has to name what it bans;
// those lines sit on hygieneAllowlist.
var bannedCommentPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(day|night|swing)shift-[0-9]+`),
	regexp.MustCompile(`(?i)\bcodex\b`),
	regexp.MustCompile(`(?i)\btorvalds\b`),
	regexp.MustCompile(`(?i)\bgraefe\b`),
	regexp.MustCompile(`audit #[0-9]+`),
	// Generic review attribution: the role standing in for the person.
	regexp.MustCompile(`(?i)\breviewers?\b`),
	// The review-bot handle — a name that happens not to look like one.
	regexp.MustCompile(`(?i)@claude\b`),
	// Review-cycle labels ("review round 2", "Round-12 reviewer", "round-4
	// review-found"): WHEN in the process, which rots exactly like a shift tag.
	regexp.MustCompile(`(?i)\breview(er)?s?[-\s]+round\b|\bround[-\s]*[0-9]+[-\s]+review`),
}

// hygieneAllowlist exempts individual offenses from the gate. Entries are
// matched as substrings of the reported offense string ("rel/path.go:123: the
// comment line"), so either a "path.go:123" prefix or a distinctive fragment of
// the comment works. Every addition needs review sign-off in the PR that adds
// it, with the reason the line is a legitimate exception rather than an
// attribution to sweep.
var hygieneAllowlist = []string{
	// The academic citation of the Cascades framework paper (notes under
	// docs/) — a literature source the planner implements, not review
	// attribution.
	"Graefe 1995",

	// The four lines of this gate's own prose that QUOTE a banned form in
	// order to define it. A ban that cannot state what it bans is undocumented,
	// and a paraphrase ("the role used as an agent") is exactly the vagueness
	// that let the generic spellings ship in the first place — the concrete
	// string is the load-bearing part. Scoped to the quoting line, never to the
	// file, so a real attribution landing here is still caught.
	`the nit both reviewers flagged" and "review`,
	"names them explicitly (\"no `per @claude`\"",
	`comment that says "this points a reviewer at the failing file"`,
	`Review-cycle labels ("review round 2"`,
}

// generatedMarker is Go's official generated-file convention
// (https://go.dev/s/generatedcode), used here as a fast path on the file
// header; ast.IsGenerated on the parsed file is the authority. The marker only
// counts BEFORE the package clause (protobuf output puts a license header
// first, so it is NOT necessarily line 1 — gen/*.pb.go carry it at line 15+).
var (
	generatedMarker = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)
	packageClause   = regexp.MustCompile(`(?m)^package\s`)
)

// sourceTreeRoot locates the REAL repository checkout. Under `go test` that is
// repoRoot's MODULE.bazel walk-up. Under Bazel the walk-up lands on the staged
// runfiles copy — which contains only declared data deps, not the source tree —
// so the staged MODULE.bazel symlink is resolved back to the workspace file it
// points at. TestSourceCommentHygiene verifies the resolved tree is the real
// one (it must contain this very test file) and fails loudly otherwise; a
// silently-empty scan would be a green gate guarding nothing.
func sourceTreeRoot(t *testing.T) string {
	t.Helper()
	staged := filepath.Join(repoRoot(t), "MODULE.bazel")
	resolved, err := filepath.EvalSymlinks(staged)
	if err != nil {
		t.Fatalf("resolving MODULE.bazel to the workspace file: %v", err)
	}
	return filepath.Dir(resolved)
}

// trackedGoFiles enumerates the scan set: `git ls-files -z -- '*.go'` (the
// RFC-175 §5 B1/B2 scope — exactly the tracked set, so untracked local scratch
// never false-positives). When git is unavailable (minimal CI image, sandbox
// without git), it falls back to a filesystem walk — a SUPERSET of the tracked
// set, so the gate can only get stricter, never quieter.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go").Output()
	if err == nil && len(out) > 0 {
		var files []string
		for _, rel := range bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0}) {
			if len(rel) > 0 {
				files = append(files, string(rel))
			}
		}
		return files
	}
	t.Logf("git ls-files unavailable (%v) — falling back to a filesystem walk (superset of tracked)", err)
	var files []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Fallback-walk skips only — trees that cannot contain tracked Go of
			// this repo. Hidden dirs cover .git and nested worktrees; bazel-* are
			// the convenience symlinks (also caught below as symlinks);
			// fdb-record-layer is the untracked Java checkout.
			if path != root && (strings.HasPrefix(name, ".") ||
				strings.HasPrefix(name, "bazel-") ||
				name == "fdb-record-layer" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		files = append(files, rel)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", root, walkErr)
	}
	return files
}

// isGeneratedFile reports whether the file carries the generated-code marker.
// src is the raw file content; f is its parse result (nil when parsing was
// skipped). The fast path scans only the header — the bytes before the first
// package clause — so a file that merely QUOTES the marker in a later comment
// is not excluded; it can only under-truncate (a "package" line inside an
// earlier string is impossible before the real clause in valid Go, and any
// mis-truncation shrinks the window), in which case ast.IsGenerated decides.
func isGeneratedFile(src []byte, f *ast.File) bool {
	head := src
	if loc := packageClause.FindIndex(head); loc != nil {
		head = head[:loc[0]]
	}
	if generatedMarker.Match(head) {
		return true
	}
	return f != nil && ast.IsGenerated(f)
}

// TestSourceCommentHygiene scans every tracked Go file in the repository and
// fails, with file:line, on any comment line matching a banned pattern.
func TestSourceCommentHygiene(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	var scanned int
	var sawSelf bool
	var offenses []string

	for _, rel := range trackedGoFiles(t, root) {
		src, readErr := os.ReadFile(filepath.Join(root, rel))
		if readErr != nil {
			t.Errorf("read %s: %v", rel, readErr)
			continue
		}
		if isGeneratedFile(src, nil) {
			continue
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if parseErr != nil {
			t.Errorf("parse %s: %v (every non-generated tracked .go file must parse)", rel, parseErr)
			continue
		}
		if isGeneratedFile(src, f) {
			continue
		}
		scanned++
		if rel == "pkg/docscheck/source_hygiene_test.go" {
			sawSelf = true
		}

		for _, cg := range f.Comments {
			for _, c := range cg.List {
				base := fset.Position(c.Slash).Line
				for i, line := range strings.Split(c.Text, "\n") {
					for _, re := range bannedCommentPatterns {
						if !re.MatchString(line) {
							continue
						}
						offense := rel + ":" + strconv.Itoa(base+i) + ": " + strings.TrimSpace(line)
						if allowlisted(offense) {
							continue
						}
						offenses = append(offenses, offense)
						break // one report per comment line, even if several patterns hit
					}
				}
			}
		}
	}

	// Anti-vacuity: if the scan did not see the real source tree (e.g. a
	// runfiles staging change broke sourceTreeRoot), the gate must go red, not
	// silently guard an empty set.
	if !sawSelf || scanned < 1000 {
		t.Fatalf("hygiene scan saw %d Go files (sawSelf=%v) under %s — that is not the real source tree; fix sourceTreeRoot/runfiles staging", scanned, sawSelf, root)
	}

	for _, o := range offenses {
		t.Errorf("banned comment content: %s", o)
	}
	if len(offenses) > 0 {
		t.Errorf("%d offending comment lines. Comments explain WHY, never WHEN/WHO: drop the shift-tag or reviewer attribution, keep the reasoning (CLAUDE.md; RFC-175 B1/B2). Genuinely legitimate lines go on hygieneAllowlist with review sign-off.", len(offenses))
	}
}

// TestBannedCommentPatterns_GenericAttribution pins the half of the ban that is
// about the ROLE rather than the name. It exists because the pattern list once
// documented these forms and did not match them: every proper noun was covered,
// so an attribution that named nobody read as clean and shipped. A gate whose
// prose is stricter than its patterns is worse than no gate, because it is cited
// as coverage.
//
// The rejected cases are the real spellings that were live in the tree, not
// invented ones. The accepted cases are the load-bearing half: "review" in an
// innocuous sense (a verb, a noun for the activity, a compound like
// "re-reviewed") must stay writable, or the ban stops being about WHO and starts
// being about a substring — at which point it gets weakened or ignored.
func TestBannedCommentPatterns_GenericAttribution(t *testing.T) {
	t.Parallel()

	rejected := []string{
		// The exact line that motivated this: WHO, with nobody named.
		"// TestInJoinLimit pins the nit both reviewers flagged with the concat fix",
		"// (reported by reviewer).",
		"// the FDB-C reviewer's ask: after the",
		"// Regression (RFC-174 Slice 0 bug 5, reviewer G2): read-only commands must",
		"// pins reviewer PUSHBACK 1 (a): a half-open poll",
		"// The REVIEWER flagged this as CRITICAL #1",
		// The review-bot handle, which CLAUDE.md names explicitly.
		"// UID's two little-endian uint64 halves copied verbatim above (@claude #303).",
		"// @claude caught in PR #214: SemanticEqualsUnderAliasMap intercepted",
		// Review-cycle labels — WHEN in the process.
		"// the original value. Caught by reviewer round 7.",
		"// plan-time-resolved ordinal accessors (review round-2 on PR #446): two",
		"// review round 12 found that an EXISTS that is NOT in a directly-handled position",
		"// 3.14 -> TRUE. Round-12 reviewer flagged the missing values.NullableDouble",
		"// read len(stmts.AllStatement()) before nil-checking. Round-2 review",
		"// pins the two round-4 review-found silent-wrong",
	}
	for _, line := range rejected {
		if !matchesBanned(line) {
			t.Errorf("comment line was ACCEPTED but carries a generic attribution: %q", line)
		}
	}

	accepted := []string{
		// "review" as the activity, not as a person who performed it.
		"// Reviewed against libfdb_c 7.3.77 — the C++ source is the spec.",
		"// The architectural review gate requires an ordered-variant enumeration here.",
		"// This is re-reviewed on every metadata-version bump.",
		"// Under review semantics the cursor must not advance past the limit.",
		// The word the audience-facing comments should use instead.
		"// The diff key points a reader at the file and index that changed.",
		"// so a reader can see the old and new primary keys side by side",
		// Rounds that are not review rounds.
		"// spfreshRefineRound retries the pass twice; round 2 sees the sealed set.",
		"// Each round of the fixpoint re-fires the rule until nothing changes.",
	}
	for _, line := range accepted {
		if matchesBanned(line) {
			t.Errorf("innocuous comment line was REJECTED — the ban is on WHO, not on the "+
				"substring \"review\": %q", line)
		}
	}
}

// matchesBanned reports whether a single comment line trips any banned pattern.
// It is the same per-line decision TestSourceCommentHygiene makes, factored out
// so the pattern set can be asserted directly instead of only through a
// whole-tree scan (which can only ever prove the tree is currently clean, never
// that a pattern would catch anything).
func matchesBanned(line string) bool {
	for _, re := range bannedCommentPatterns {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func allowlisted(offense string) bool {
	for _, a := range hygieneAllowlist {
		if a != "" && strings.Contains(offense, a) {
			return true
		}
	}
	return false
}

// TestIsGeneratedFileMarkerPlacement pins the generated-file detection the gate
// relies on: the marker counts wherever it appears before the package clause
// (protobuf output puts a license header first — the marker sits at line 15+),
// and a file merely MENTIONING generated code after the package clause is NOT
// excluded. Detection is by the marker property, never by path.
func TestIsGeneratedFileMarkerPlacement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"marker on line 1", "// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage p\n", true},
		{
			"marker after license header",
			"// Copyright 2026 The Authors.\n//\n// Licensed under the Apache License, Version 2.0.\n// See LICENSE for details.\n\n// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage p\n",
			true,
		},
		{"no marker", "// Package p is handwritten.\npackage p\n", false},
		{"marker text only after package clause", "package p\n\n// This helper consumes files whose head reads\n// Code generated by protoc-gen-go. DO NOT EDIT.\nvar X int\n", false},
	}
	for _, tc := range cases {
		src := []byte(tc.src)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, tc.name+".go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		// The authority path (parsed file present) must agree with want; the
		// fast path (head scan, nil file) may only ever say true when the
		// authority also says true.
		if got := isGeneratedFile(src, f); got != tc.want {
			t.Errorf("%s: isGeneratedFile with AST = %v, want %v", tc.name, got, tc.want)
		}
		if fastOnly := isGeneratedFile(src, nil); fastOnly && !tc.want && !ast.IsGenerated(f) {
			t.Errorf("%s: head-scan fast path excluded a file the marker authority does not", tc.name)
		}
	}
}
