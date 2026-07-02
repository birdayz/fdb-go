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
// Code comments explain WHY, never WHEN or WHO: shift-tags and reviewer
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
// shift tags in code comments"; reviewer attribution is git-blame's job).
var bannedCommentPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(day|night|swing)shift-[0-9]+`),
	regexp.MustCompile(`\(codex\b`),
	regexp.MustCompile(`codex:`),
	regexp.MustCompile(`\(Torvalds`),
	regexp.MustCompile(`audit #[0-9]+`),
}

// hygieneAllowlist exempts individual offenses from the gate. Entries are
// matched as substrings of the reported offense string ("rel/path.go:123: the
// comment line"), so either a "path.go:123" prefix or a distinctive fragment of
// the comment works. EMPTY at birth — every addition needs review sign-off in
// the PR that adds it, with the reason the line is a legitimate exception
// rather than an attribution to sweep.
var hygieneAllowlist = []string{}

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
