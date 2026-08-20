package docscheck

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
// automatically), so paths appear below only as fallback-walk skips, and only
// for trees that are unbounded or foreign rather than merely hidden (the Java
// checkout, the object store, other agents' worktrees, Bazel symlinks) — see
// fallbackWalkSkippedTrees. A hidden directory is NOT excluded for being hidden:
// .github and .claude/skills are tracked.
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
	//
	// `rounds?` and not `round`: the PLURAL is the form that appears when the
	// attribution is a narrative rather than a label ("successive review rounds
	// then found"), and it is the form that shipped past this gate. `\bround\b`
	// rejected it outright — the trailing `s` is a word character, so the
	// boundary never matched — which made the singular-only pattern read as
	// coverage of a class it half-covered.
	regexp.MustCompile(`(?i)\breview(er)?s?[-\s]+rounds?\b|\bround[-\s]*[0-9]+[-\s]+review`),
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
	// The plural arm's own rationale, which has to name the form it added.
	`attribution is a narrative rather than a label ("successive review rounds`,
	// The flowed-scan rationale, reported as the whole flowed group because the
	// quoted form is itself wrapped. Scoped to a fragment only that block
	// contains.
	`is the same attribution as "review rounds", and the per-line scan above cannot see it`,
	// The accepted-cases heading in TestBannedCommentPatterns_GenericAttribution,
	// which contrasts fixpoint/retry "rounds" with review ones.
	"// Rounds that are not review rounds.",
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

// trackedGoFiles enumerates the deliverable scan set: tracked Go files UNION
// untracked, non-ignored Go files. The second half is load-bearing in a shared
// implementation worktree: a newly added source file compiles immediately, but
// `git ls-files --cached` cannot see it until somebody stages it. A docs gate
// that ignores the file until staging reports a false green over a tree that Go
// is already building. Ignored scratch/build output stays excluded by
// --exclude-standard.
//
// When git is unavailable (minimal CI image, sandbox without git), this falls
// back to the shared fallbackWalk, whose exclusions are a closed NAMED list
// rather than a leading-dot pattern; that is what makes it a superset of the
// deliverable set, so the gate can only get stricter, never quieter.
// The dot-pattern it replaced was a SUBSET: .github and .claude/skills hold
// tracked files. No tracked *.go lives there at d482c92f8 (`git ls-files --
// '*.go' | grep -cE '^\.[^/]+/'` → 0 of 3469), but that is an accident of
// today's layout, not a property this scan may rest on.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	files, err := gitGoFiles(root)
	if err == nil && len(files) > 0 {
		return files
	}
	t.Logf("git deliverable-file enumeration unavailable (%v) — falling back to a filesystem walk over everything but the "+
		"named excluded trees (%v), a superset of tracked plus untracked non-ignored files", err, sortedFallbackWalkSkips())
	files, walkErr := fallbackWalk(root, func(name string) bool {
		return strings.HasSuffix(name, ".go")
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", root, walkErr)
	}
	return files
}

// gitDeliverableFiles returns the sorted, duplicate-free union of tracked files
// and untracked files not excluded by the repository's ignore rules. Tracked
// paths deleted in the worktree are absent: the deliverable is what local Go
// and Bazel commands can compile now, not what the index remembers.
func gitDeliverableFiles(root, pattern string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", pattern).Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, rel := range bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0}) {
		if len(rel) == 0 {
			continue
		}
		normalized := filepath.ToSlash(string(rel))
		if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(normalized))); statErr != nil {
			if os.IsNotExist(statErr) {
				// The index still names tracked files deleted in this worktree. They
				// are no longer part of the deliverable Go is compiling.
				continue
			}
			return nil, statErr
		}
		seen[normalized] = struct{}{}
	}
	files := make([]string, 0, len(seen))
	for rel := range seen {
		files = append(files, rel)
	}
	sort.Strings(files)
	return files, nil
}

// gitGoFiles is the Go-source specialization shared by the source censuses.
func gitGoFiles(root string) ([]string, error) {
	return gitDeliverableFiles(root, "*.go")
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

// flowComment renders a comment group as one line: markers stripped, every run
// of whitespace (newlines included) collapsed to a single space. It is what
// lets a banned phrase be recognised across a line wrap, which is how the
// author of a comment actually reads it.
func flowComment(cg *ast.CommentGroup) string {
	var b strings.Builder
	for _, c := range cg.List {
		text := c.Text
		switch {
		case strings.HasPrefix(text, "//"):
			text = text[2:]
		case strings.HasPrefix(text, "/*"):
			text = strings.TrimSuffix(strings.TrimPrefix(text, "/*"), "*/")
		}
		b.WriteString(text)
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// commentGroupOffenses is the whole per-group decision, taking its inputs
// explicitly so a test can drive every arm without a source tree. That
// separation is the point: the wrapped arm has NO offender in the repository —
// deliberately — so the full-tree run cannot exercise it, and a green there
// would say nothing about whether the flowed pass works.
//
// lineOf maps a position to a 1-based line, so the caller owns the FileSet.
func commentGroupOffenses(rel string, cg *ast.CommentGroup, lineOf func(token.Pos) int) []string {
	var offenses []string

	// Which patterns already have a verdict from the per-line scan — reported
	// OR allowlisted. The allowlisted half matters: an exemption is a decision
	// about that content, and re-reporting it from the flowed pass below would
	// re-raise what was already settled.
	// EVERY matching pattern is settled, not just the first. One line often
	// trips several — a role noun inside a cycle label matches both the bare
	// role pattern and the label pattern — and stopping at the first left the
	// others unsettled, so the flowed pass below reported the same line a
	// second time. That is pinned in the wrap test. Reporting is
	// still once per line: the offense string is the line, so the allowlist
	// decision is about the text and not about which pattern found it.
	settled := map[int]bool{}
	for _, c := range cg.List {
		base := lineOf(c.Slash)
		for i, line := range strings.Split(c.Text, "\n") {
			matched := false
			for p, re := range bannedCommentPatterns {
				if re.MatchString(line) {
					settled[p] = true
					matched = true
				}
			}
			if !matched {
				continue
			}
			offense := rel + ":" + strconv.Itoa(base+i) + ": " + strings.TrimSpace(line)
			if allowlisted(offense) {
				continue
			}
			offenses = append(offenses, offense)
		}
	}

	// A banned phrase does not have to sit on one line. Wrap it and the
	// per-line scan above cannot see it — the match spans the break, while the
	// reader sees one sentence. This gate's own prose says a gate whose wording
	// is stricter than its patterns is worse than none, and a per-line-only
	// scan is exactly that.
	//
	// So the group is scanned again as ONE flowed string, reported against the
	// group's first line because a wrapped phrase has no single line to blame.
	// Per PATTERN, not per group: a pattern the line scan already settled is
	// skipped here, so a plain hit is never double-counted and an exemption is
	// not re-raised — while a DIFFERENT pattern wrapped in the same group is
	// still caught.
	flowed := flowComment(cg)
	for p, re := range bannedCommentPatterns {
		if settled[p] || !re.MatchString(flowed) {
			continue
		}
		offense := rel + ":" + strconv.Itoa(lineOf(cg.Pos())) + ": " + flowed
		if allowlisted(offense) {
			continue
		}
		offenses = append(offenses, offense)
		break
	}
	return offenses
}

// TestSourceCommentHygiene scans every tracked Go file in the repository and
// fails, with file:line, on any comment matching a banned pattern — per line,
// and again with the whole group flowed onto one line so a phrase broken across
// a wrap cannot slip through.
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

		lineOf := func(p token.Pos) int { return fset.Position(p).Line }
		for _, cg := range f.Comments {
			offenses = append(offenses, commentGroupOffenses(rel, cg, lineOf)...)
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
		// The PLURAL, which the singular-anchored pattern let through: the
		// trailing `s` is a word character, so `\bround\b` never matched. It is
		// the form a narrative attribution takes rather than a label, and it was
		// live in the tree (a CTE registration comment ending "the exact
		// two-authorities anti-pattern").
		"// two-authorities anti-pattern (review rounds 3-7).",
		"// while the check was optional, successive review rounds found the bound absent",
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
// TestBannedCommentPatterns_SurviveALineWrap pins the flowed scan. The
// full-tree gate cannot pin it: the tree has no wrapped offender (that is the
// point), so a green there is a green over an empty set and says nothing about
// whether the flowed pass works.
//
// The wrap is not an exotic case. A comment is written as prose and hard-wrapped
// afterwards, so WHICH line a phrase lands on is decided by column width, not by
// the author — the same sentence is caught or missed depending on where it
// started. A per-line-only scan therefore covers a random subset of what its
// prose claims.
func TestBannedCommentPatterns_SurviveALineWrap(t *testing.T) {
	t.Parallel()

	// Each case is a comment group whose banned phrase straddles the break, and
	// whose per-line halves are innocuous on their own — that is what makes the
	// wrap a bypass rather than a near miss, and the loop below asserts it
	// rather than trusting it.
	//
	// Every case is a MULTI-WORD pattern, because those are the only ones a
	// wrap can split: the single-word bans (a bare role noun, a bot handle, a
	// shift tag) survive any wrap, since a break falls between words and
	// flowing rejoins them with the same single space.
	wrapped := []*ast.CommentGroup{
		{List: []*ast.Comment{
			{Text: "// while the check was optional here, successive review"},
			{Text: "// rounds found the bound absent on one type after another."},
		}},
		{List: []*ast.Comment{
			{Text: "// plan-time ordinals, from round-2"},
			{Text: "// review on the ordinal accessors."},
		}},
		{List: []*ast.Comment{
			{Text: "// pins the two round-4"},
			{Text: "// review-found silent-wrong answers."},
		}},
	}
	// lineOf is a stand-in FileSet: every synthetic comment reports line 1,
	// which is all the reporting needs.
	lineOf := func(token.Pos) int { return 1 }

	for _, cg := range wrapped {
		for _, c := range cg.List {
			if matchesBanned(c.Text) {
				t.Fatalf("a half of this case matches on its own, so it does not test the "+
					"wrap: %q", c.Text)
			}
		}
		if got := commentGroupOffenses("x.go", cg, lineOf); len(got) == 0 {
			t.Errorf("a banned phrase split across a line wrap was ACCEPTED: %q", flowComment(cg))
		}
	}

	// And flowing must not INVENT a match: joining adjacent innocuous lines is
	// the risk the flowed pass introduces, so the accepted direction is pinned
	// too.
	innocuous := &ast.CommentGroup{List: []*ast.Comment{
		{Text: "// Each round of the fixpoint re-fires the rule; the ordering is"},
		{Text: "// reviewed against the Java planner before every metadata bump."},
	}}
	if got := commentGroupOffenses("x.go", innocuous, lineOf); len(got) > 0 {
		t.Errorf("flowing two innocuous lines manufactured an attribution: %v", got)
	}

	// A plain single-line hit must be reported ONCE, not again from the flowed
	// pass. Without the per-pattern `settled` bookkeeping every ordinary
	// offense would be double-counted, which inflates the failure list and
	// makes a real second offense in the same group hard to see.
	plain := &ast.CommentGroup{List: []*ast.Comment{
		{Text: "// the original value. Caught by reviewer round 7."},
		{Text: "// The rest of this comment is innocuous."},
	}}
	if got := commentGroupOffenses("x.go", plain, lineOf); len(got) != 1 {
		t.Errorf("a single-line offense must be reported exactly once, got %d: %v", len(got), got)
	}
}

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
