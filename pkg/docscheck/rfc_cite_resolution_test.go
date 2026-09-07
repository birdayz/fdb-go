package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const rfc238Path = "rfcs/238-a-qualifier-is-structure-not-punctuation.md"

// A CITE NAMING A BASENAME THE TREE HOLDS THREE OF IS NOT A CITE, and this is
// the half of a cite gate that is worth building. RFC-238 §7d measures the
// other half -- "does the cited line look like code" -- and rejects it: most
// resolved cites that are not code land on a `//` line, so that check scores
// citation STYLE. TestRFCCiteCensusRepoWide prints the figures. Resolution is
// different: an ambiguous or dangling cite is wrong under every style.
//
// It is not hypothetical. §7d's own population was computed twice by a scratch
// checker that indexed Go files by BASENAME, so `metadata.go:1343-1345`
// resolved against whichever of the tree's three `metadata.go` files the walk
// met first, and the resulting classification was reported as a fact about
// this document. Those cites are now written with their directory and this
// test holds them there.
//
// SCOPE, as the list of what it does NOT cover. NO COUNTS APPEAR HERE ON
// PURPOSE: two independent counts of the same file disagreed on every
// per-document tally this comment used to carry, because "how many cites"
// depends on unstated choices no sentence here settled. TestRFCCiteCensusRepoWide prints
// the corpus census on every run; this list says what is excluded, not how much.
//
//   - Only `rfcs/238-...md`. The rest of `rfcs/*.md` carries ambiguous,
//     dangling and past-EOF cites in quantity -- a real cleanup, not a one-line
//     widening of this test.
//   - Only GO cites. RFC-238 also cites `.java` files in the vendored tree,
//     which is gitignored, so the resolution this gate performs is unavailable
//     for them at all; and one `.yamsql` file, which IS tracked and could be
//     covered but is not, because the parser here understands only `.go`.
//   - Not the Go cites that resolve uniquely only because their basename
//     happens to be unique in the tree TODAY. A second file of that name makes
//     them ambiguous and this gate reddens, which is the design rather than a
//     wart: the cite gets qualified at that point, as the `metadata.go` ones
//     were.
//   - Not a cite to anything that is not a file:line -- a dangling reference to
//     a TEST NAME is invisible here, and this PR shipped one, in the comment on
//     the very guard it was adding.
func TestRFC238CitesResolveUniquely(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	index := goFileSuffixIndex(t, root)

	cites := citesIn(t, filepath.Join(root, rfc238Path))
	if len(cites) < 40 {
		t.Fatalf("only %d distinct cites parsed out of %s; the scan lost the document "+
			"(check sourceTreeRoot/runfiles staging) rather than the document losing its cites — "+
			"a resolution test over an empty population passes for the wrong reason", len(cites), rfc238Path)
	}

	for _, c := range cites {
		cands := index[c.path]
		lines := 0
		if len(cands) == 1 {
			lines = lineCount(t, filepath.Join(root, cands[0]))
		}
		if problem := citeProblem(c, cands, lines); problem != "" {
			t.Errorf("cite %q %s", c.text, problem)
		}
	}
}

// citeProblem is the whole decision, kept OUT of the loop above so it can be
// driven by a test. This document has zero ambiguous, zero dangling and zero
// past-EOF cites, so every arm of it is unexercised by the corpus — a gate
// whose error paths only ever run when someone breaks the tree is a gate nobody
// has seen work. An earlier version also hand-inlined the bounds rule here
// while the extracted citeInBounds was pinned separately, which is two
// implementations of one rule with one of them untested.
func citeProblem(c rfcCite, cands []string, lines int) string {
	switch {
	case len(cands) == 0:
		return "resolves to NO file in the tracked tree"
	case len(cands) > 1:
		return fmt.Sprintf("is AMBIGUOUS — %d tracked files match that path suffix (%s). "+
			"Qualify it with enough leading directory to name one file; a reader (and any "+
			"tool) otherwise resolves it to whichever the walk meets first",
			len(cands), strings.Join(cands, ", "))
	case !citeInBounds(c, lines):
		if c.end != 0 {
			return fmt.Sprintf("names lines %d-%d of %s, which has %d lines",
				c.start, c.end, cands[0], lines)
		}
		return fmt.Sprintf("names line %d of %s, which has %d lines", c.start, cands[0], lines)
	}
	return ""
}

// EVERY ERROR ARM OF THE GATE, driven directly. The corpus reaches none of
// them, so without this the gate's failure behaviour is an assumption.
func TestCiteProblemNamesEveryWayACiteCanBeUnusable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		c     rfcCite
		cands []string
		lines int
		want  string // substring; "" means the cite must be accepted
	}{
		{"clean", rfcCite{start: 10}, []string{"a/b.go"}, 100, ""},
		{"clean range", rfcCite{start: 10, end: 20}, []string{"a/b.go"}, 100, ""},
		{"dangling", rfcCite{start: 10}, nil, 0, "resolves to NO file"},
		{"ambiguous", rfcCite{start: 10}, []string{"a/b.go", "c/b.go"}, 0, "is AMBIGUOUS — 2 tracked files"},
		{"start past EOF", rfcCite{start: 101}, []string{"a/b.go"}, 100, "names line 101 of a/b.go, which has 100 lines"},
		{"end past EOF", rfcCite{start: 90, end: 101}, []string{"a/b.go"}, 100, "names lines 90-101 of a/b.go, which has 100 lines"},
	} {
		got := citeProblem(tc.c, tc.cands, tc.lines)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s: rejected with %q, want accepted", tc.name, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("%s: got %q, want it to contain %q", tc.name, got, tc.want)
		}
	}
}

// The WEAK set §7d enumerates, held as a set rather than a count. A count says
// nothing about which cite moved; this fails naming the difference, and the
// failure means §7d's enumeration needs re-running — not that the cite is bad.
//
// A range counts as code when EITHER endpoint is code, which is the rule §7d
// states. The scratch checker that preceded it classified each ENDPOINT
// separately and reported every non-code one, which is what made
// `record_types_property.go:37-51`
// -- code ranges that CLOSE on a brace -- read weak. A draft of §7d then wrote
// a sentence explaining those two, and the sentence was false for a third cite.
func TestRFC238WeakCitesAreTheOnesSection7dNames(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	index := goFileSuffixIndex(t, root)

	want := []string{
		"cascades_generator.go:2830",
		"colref.go:95",
		"derived_unnest.go:250",
		"full_unordered_scan.go:110-118",
		"positional_row.go:7",
	}

	var got []string
	classified := 0
	for _, c := range citesIn(t, filepath.Join(root, rfc238Path)) {
		cands := index[c.path]
		if len(cands) != 1 {
			// Reported by TestRFC238CitesResolveUniquely; not this test's alarm.
			continue
		}
		lines := fileLines(t, filepath.Join(root, cands[0]))
		if !citeInBounds(c, len(lines)) {
			// Reported by TestRFC238CitesResolveUniquely. A range whose END is
			// past EOF is DANGLING, not classifiable: counting it as resolved is
			// what put two such ranges into the repo-wide totals (a
			// 36-line span cited against a 99-line file, and one more).
			continue
		}
		classified++
		class := classifyCiteLine(lines[c.start-1])
		if class != "code" && c.end > c.start && c.end <= len(lines) &&
			classifyCiteLine(lines[c.end-1]) == "code" {
			class = "code"
		}
		if class != "code" {
			got = append(got, c.text)
		}
	}
	if classified < 40 {
		t.Fatalf("only %d cites were classified; the population collapsed and an empty "+
			"weak set would otherwise read as agreement with §7d", classified)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("weak-cite set has moved.\n got: %v\nwant: %v\n"+
			"§7d enumerates this set and explains each entry; re-run the census and update the "+
			"section rather than editing this list alone", got, want)
	}
}

type rfcCite struct {
	text  string
	path  string
	start int
	end   int
}

func classifyCiteLine(line string) string {
	t := strings.TrimSpace(line)
	switch {
	case t == "":
		return "blank"
	case t == "}" || t == "{" || t == "})" || t == "},":
		return "brace"
	case strings.HasPrefix(t, "//"):
		return "comment"
	default:
		return "code"
	}
}

// goFileSuffixIndex maps every path SUFFIX at a directory boundary to the
// tracked files carrying it, so a cite disambiguates itself with as much
// leading directory as it chooses to write.
func goFileSuffixIndex(t *testing.T, root string) map[string][]string {
	t.Helper()
	if nested := nestedRepoRootsUnder(t, root); len(nested) > 0 {
		t.Fatalf("a nested copy of the repository is present under the source tree at %v.\n"+
			"Every basename it carries is duplicated, so every cite resolving by basename\n"+
			"becomes AMBIGUOUS and this census would report totals about a tree that is not\n"+
			"the source tree. Remove the copy (scratch extracts belong outside the worktree).", nested)
	}
	files := trackedGoFiles(t, root)
	if len(files) < 1000 {
		t.Fatalf("only %d tracked Go files enumerated; the scan lost the tree, and every cite "+
			"would then resolve to nothing", len(files))
	}
	index := map[string][]string{}
	for _, rel := range files {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i := range parts {
			suf := strings.Join(parts[i:], "/")
			index[suf] = append(index[suf], rel)
		}
	}
	return index
}

func citesIn(t *testing.T, path string) []rfcCite {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	seen := map[string]bool{}
	var out []rfcCite
	for _, line := range strings.Split(string(b), "\n") {
		for _, c := range scanCitesInLine(line) {
			if seen[c.text] {
				continue
			}
			seen[c.text] = true
			out = append(out, c)
		}
	}
	return out
}

func scanCitesInLine(s string) []rfcCite {
	var out []rfcCite
	for i := 0; i < len(s); i++ {
		j := strings.Index(s[i:], ".go:")
		if j < 0 {
			break
		}
		j += i
		k := j
		for k > 0 && isCitePathByte(s[k-1]) {
			k--
		}
		p := j + len(".go:")
		st := p
		for p < len(s) && s[p] >= '0' && s[p] <= '9' {
			p++
		}
		if p == st {
			i = j + len(".go")
			continue
		}
		start, _ := strconv.Atoi(s[st:p])
		end := 0
		if p < len(s) && s[p] == '-' {
			q := p + 1
			for q < len(s) && s[q] >= '0' && s[q] <= '9' {
				q++
			}
			if q > p+1 {
				end, _ = strconv.Atoi(s[p+1 : q])
				p = q
			}
		}
		out = append(out, rfcCite{
			text:  s[k:p],
			path:  strings.TrimPrefix(s[k:j]+".go", "/"),
			start: start,
			end:   end,
		})
		i = p - 1
	}
	return out
}

func isCitePathByte(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
		ch == '_' || ch == '/' || ch == '.' || ch == '-'
}

// citeInBounds reports whether BOTH endpoints name a real source line. A range
// whose end is past EOF is dangling exactly as a bare line past EOF is; the
// first version checked only the start when classifying, which counted two such
// ranges in the repo-wide corpus as resolved code.
func citeInBounds(c rfcCite, lines int) bool {
	if c.start < 1 || c.start > lines {
		return false
	}
	return c.end == 0 || c.end <= lines
}

// fileLines returns the SOURCE lines, without the empty string that Split
// leaves behind a terminal newline. That sentinel is not a line: counting it
// made every newline-terminated file report one line too many, so a cite naming
// line N+1 of an N-line file passed the bounds check -- the file-shrank-by-one
// drift this gate exists to catch -- and classified as "blank".
func fileLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(b), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	return len(fileLines(t, path))
}

// THE TWO PLACES A CITE CAN BE WRONGLY ACCEPTED, driven directly rather than
// through the corpus. Both were wrong on arrival, and the corpus could not have
// caught either: RFC-238 carries no past-EOF cite and no cite on the last line
// of a file, so every arm below is a shape the census never reaches. Repo-wide
// the shapes are real -- 23 past-EOF cites across `rfcs/*.md`, of which a range
// with an in-bounds START and a dangling END was counted as RESOLVED CODE until
// citeInBounds checked both.
func TestCiteInBoundsRejectsEveryPastEOFShape(t *testing.T) {
	t.Parallel()
	const n = 100
	for _, tc := range []struct {
		name string
		c    rfcCite
		want bool
	}{
		{"bare line inside", rfcCite{start: 100}, true},
		{"bare line one past EOF", rfcCite{start: 101}, false},
		{"line zero", rfcCite{start: 0}, false},
		{"range fully inside", rfcCite{start: 90, end: 100}, true},
		{"range END one past EOF", rfcCite{start: 90, end: 101}, false},
		{"range START past EOF", rfcCite{start: 101, end: 105}, false},
	} {
		if got := citeInBounds(tc.c, n); got != tc.want {
			t.Errorf("%s: citeInBounds(%+v, %d) = %v, want %v", tc.name, tc.c, n, got, tc.want)
		}
	}
}

// A newline-terminated file of N lines reports N. Counting the empty string
// Split leaves behind the terminal newline made every such file one line too
// long, so a cite naming line N+1 passed the bounds check -- the
// file-shrank-by-one drift this gate exists to catch -- and classified "blank".
// Written through a temp file so the arm does not rest on some tracked file
// keeping its current length.
func TestFileLinesDropsTheTerminalNewlineSentinel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
		want    int
	}{
		{"newline-terminated", "a\nb\nc\n", 3},
		{"no trailing newline", "a\nb\nc", 3},
		{"single line, terminated", "a\n", 1},
		{"empty file", "", 0},
	} {
		p := filepath.Join(dir, tc.name+".txt")
		if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := lineCount(t, p); got != tc.want {
			t.Errorf("%s: lineCount = %d, want %d — a wrong count here silently widens "+
				"every bounds check in this file by that much", tc.name, got, tc.want)
		}
	}
}

// THE REPO-WIDE CENSUS §7d QUOTES, computed rather than remembered.
//
// It ASSERTS nothing about the totals. Pinning them would be the style gate
// §7d rejects, and they move with every RFC edit. What it does is PRINT them,
// so the section's figures can be re-derived by running one target instead of
// trusting a number in prose — which matters because §7d shipped a wrong count
// in four consecutive revisions while the underlying algorithm was correct, and
// because two people implementing its stated rule from prose got different
// answers.
//
// The assertions are the anti-vacuity floors. Without them a scan that lost the
// tree, or a parser that matched nothing, would print a tidy all-zero census
// and pass.
//
//	bazelisk test //pkg/docscheck:docscheck_test --test_output=streamed \
//	  --test_arg=-test.run=TestRFCCiteCensusRepoWide --test_arg=-test.v
func TestRFCCiteCensusRepoWide(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	index := goFileSuffixIndex(t, root)

	entries, err := os.ReadDir(filepath.Join(root, "rfcs"))
	if err != nil {
		t.Fatalf("reading rfcs/: %v", err)
	}
	var docs int
	seen := map[string]bool{}
	var all []rfcCite
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		docs++
		for _, c := range citesIn(t, filepath.Join(root, "rfcs", e.Name())) {
			if seen[c.text] {
				continue
			}
			seen[c.text] = true
			all = append(all, c)
		}
	}

	var ambiguous, dangling, pastEOF int
	classes := map[string]int{}
	for _, c := range all {
		cands := index[c.path]
		switch {
		case len(cands) == 0:
			dangling++
			continue
		case len(cands) > 1:
			ambiguous++
			continue
		}
		lines := fileLines(t, filepath.Join(root, cands[0]))
		if !citeInBounds(c, len(lines)) {
			pastEOF++
			continue
		}
		class := classifyCiteLine(lines[c.start-1])
		if class != "code" && c.end > c.start && classifyCiteLine(lines[c.end-1]) == "code" {
			class = "code"
		}
		classes[class]++
	}
	resolved := classes["code"] + classes["comment"] + classes["brace"] + classes["blank"]
	weak := resolved - classes["code"]

	t.Logf("CENSUS over %d files in rfcs/: %d distinct cites — %d ambiguous, %d dangling, "+
		"%d past-EOF, %d resolved (%d code / %d comment / %d brace / %d blank), %d weak",
		docs, len(all), ambiguous, dangling, pastEOF, resolved,
		classes["code"], classes["comment"], classes["brace"], classes["blank"], weak)

	// Anti-vacuity. These are floors on the POPULATION, not on the findings:
	// they fail when the scan lost the tree, never when the corpus improves.
	if docs < 100 {
		t.Fatalf("only %d RFC files scanned; the walk lost rfcs/", docs)
	}
	if len(all) < 2000 {
		t.Fatalf("only %d distinct cites parsed; the parser matched almost nothing, and "+
			"every count above is a fact about that failure rather than about the corpus", len(all))
	}
	if resolved < 1500 {
		t.Fatalf("only %d cites resolved of %d; the Go file index is broken, which would "+
			"make the dangling and ambiguous counts meaningless", resolved, len(all))
	}
}

// nestedRepoRootsUnder reports directories below root that carry their own
// MODULE.bazel — a second copy of this repository living inside the worktree.
//
// It matters here and not only in the abstract: a nested copy DOUBLES every
// basename, so every duplicated path suffix becomes ambiguous and a census over
// the result is fiction that looks like a finding. `git ls-files` excludes such
// a copy, but the filesystem fallback used when git is unavailable does not, so
// the guard belongs at the point of use rather than in the walk.
//
// The exclusions match the walk's own: build output, VCS metadata, the vendored
// Java tree, and sibling worktrees.
func nestedRepoRootsUnder(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			n := fi.Name()
			if n == ".git" || n == "fdb-record-layer" || n == "worktrees" ||
				strings.HasPrefix(n, "bazel-") {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.Name() != "MODULE.bazel" {
			return nil
		}
		if filepath.Dir(p) == root {
			return nil // the repository's own
		}
		found = append(found, filepath.Dir(p))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for nested repositories: %v", root, err)
	}
	sort.Strings(found)
	return found
}

// classifyCiteLine's four classes, driven directly. The corpus exercises only
// whichever classes it happens to contain, and `weak` — the figure §7d leans on
// hardest — is entirely this function's output, so a classification regression
// would otherwise move every census number while every test stayed green.
func TestClassifyCiteLineCoversEveryClass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ line, want string }{
		{"", "blank"},
		{"   \t ", "blank"},
		{"}", "brace"},
		{"\t}", "brace"},
		{"{", "brace"},
		{"})", "brace"},
		{"},", "brace"},
		{"// a comment", "comment"},
		{"\t// indented comment", "comment"},
		{"func f() {", "code"},
		{"\treturn nil", "code"},
		{"\tx := 1 // trailing comment is still code", "code"},
	} {
		if got := classifyCiteLine(tc.line); got != tc.want {
			t.Errorf("classifyCiteLine(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// nestedRepoRootsUnder, driven against a constructed tree. Its whole purpose is
// to fire on a state the corpus must never be in, so the corpus can never
// exercise it: without this arm the guard is a branch nobody has seen run, and
// the census it protects would report a doubled population as a finding.
func TestNestedRepoRootsUnderFindsASecondCopy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "MODULE.bazel"), []byte("module(name='r')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The control: the repository's own MODULE.bazel must NOT count as nested,
	// or the guard would refuse every tree including the real one.
	if got := nestedRepoRootsUnder(t, root); len(got) != 0 {
		t.Fatalf("the root's own MODULE.bazel was reported as nested: %v", got)
	}

	nested := filepath.Join(root, "scratchpad", "abc123")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "MODULE.bazel"), []byte("module(name='r')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := nestedRepoRootsUnder(t, root)
	if len(got) != 1 || got[0] != nested {
		t.Errorf("nestedRepoRootsUnder = %v, want exactly [%s]", got, nested)
	}

	// And an excluded tree does NOT count: a sibling worktree carries its own
	// MODULE.bazel by construction and is not a copy inside this one.
	wt := filepath.Join(root, "worktrees", "other")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "MODULE.bazel"), []byte("module(name='r')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := nestedRepoRootsUnder(t, root); len(got) != 1 {
		t.Errorf("nestedRepoRootsUnder = %v, want the excluded worktree to stay excluded "+
			"(exactly one hit, the scratchpad copy)", got)
	}
}
