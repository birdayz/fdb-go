package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The EMPTY-FILTER gate.
//
// `go test -run` and Bazel's `--test_filter` take a REGEXP, and a regexp that
// matches no test function is not an error: the binary selects zero tests, runs
// them all successfully, and prints PASS. Nothing distinguishes that from a real
// pass except the absence of `=== RUN` lines, which nobody reads when the verdict
// already says PASS. CLAUDE.md lists this as the first of four faces of "a green
// from an empty set"; it is the one that costs a reader the most, because the
// green is attached to a command they typed themselves.
//
// The documented instance: a test actually named `TestFieldNameNeverDecides` was
// cited, and run, as `TestFieldNameDecision`. Three readers in one day took the
// resulting PASS as evidence about the code.
//
// WHAT THIS GATE COVERS, in two arms with deliberately different scopes:
//
//   - ARM A — EXECUTABLE FILTERS. Every `-test.run=`, `--test.run=`,
//     `--test_filter=` and `-run=` value appearing in a tracked `.md`, `.yml`,
//     `.yaml`, `.sh` or `justfile` must, as a regexp, match at least one test
//     function in the repository. A command in a document is a promise that
//     running it produces a result; an unmatchable filter breaks that promise
//     silently, which is the whole defect.
//
//   - ARM B — PRESENT-TENSE CITATIONS. Every backtick-quoted `Test…`/`Fuzz…`/
//     `Benchmark…` identifier in the AUTHORITY documents — the ones that assert
//     what the repository contains RIGHT NOW — must name a real test function.
//     This arm is what would have caught the documented instance, which entered
//     as prose long before anyone typed it after `--test.run=`.
//
// WHAT IT DOES NOT COVER, and why each exclusion is a judgement rather than an
// oversight:
//
//   - The SPACE-SEPARATED `go test -run <pattern>` form. Extracting it correctly
//     needs shell tokenisation (the value is routinely quoted and routinely a
//     multi-branch alternation), and measured over this tree its occurrences are
//     dominated by prose that merely mentions the flag ("`-run` is …", "`-run` …").
//     The `=`-anchored forms above need no tokenisation and carry every real
//     copy-pasteable recipe in the tree.
//
//   - RFCs and shift handovers. Both are DATED records of a design or a night's
//     work. A test renamed afterwards does not make the record wrong, and editing
//     history to keep a gate green is the wrong incentive entirely.
//
//   - TODO.md and its siblings, which are FORWARD-looking: an unchecked item
//     naming the test it will add is correct prose about a test that must not yet
//     exist. Arm B therefore skips them — but it does not skip them SILENTLY, see
//     TestTodoTestCitationDriftIsReported, which measures the count so the
//     exclusion stays visible instead of becoming invisible.
//
// A gate nobody has seen fail is not known to work, and a gate whose extractor is
// broken is indistinguishable from a clean tree — both report zero. Both halves
// are pinned: TestTestFilterExtractorMatchesEveryDocumentedForm drives the
// extractor over every accepted and rejected form, and
// TestTestFilterGateRejectsAPhantomName runs a synthetic phantom through the
// exact resolution path the tree arms use and requires it to be rejected. The
// tree arms additionally assert their own populations are non-empty, so a green
// there is a claim that something was examined.

// authorityDocs are the documents that assert, in the present tense, what this
// repository contains. Arm B holds their test-name citations to that tense.
//
// The set is the RFC-131 "living docs" minus the forward-looking planning files:
// a living doc describes the tree as it is, so a name it cites must resolve.
var authorityDocs = map[string]bool{
	"CLAUDE.md":               true,
	"README.md":               true,
	"DIVERGENCES.md":          true,
	"road-to-prod.md":         true,
	"PRODUCTION_READINESS.md": true,
	"CHANGELOG.md":            true,
	"RELEASE.md":              true,
}

// forwardLookingPlanDocs are excluded from Arm B because an item that has not
// been done yet legitimately names a test that does not exist yet.
var forwardLookingPlanDocs = map[string]bool{
	"TODO.md":            true,
	"TODO-production.md": true,
	"TODO_client.md":     true,
}

// testFilterPlaceholders are filter values that are TEMPLATES, not names — a
// reader is expected to substitute before running. Each needs a reason; the map
// is the only way past Arm A.
//
// A template is recognised by having an entry here and by nothing else. There is
// deliberately no "looks like a placeholder" heuristic: `Xxx` and `<Axis>` would
// be easy to pattern-match, but so would a real test named `TestXxxOrdering`, and
// a heuristic that silently forgives a name is this gate's own failure mode.
var testFilterPlaceholders = map[string]string{
	"^$":                      "the empty-matching regexp, used on purpose to select ZERO tests so a fuzz or benchmark flag runs alone",
	"TestName$":               "the substitute-your-own-name slot in the skill docs' generic `bazelisk test` recipe",
	"TestFDB_Xxx":             "CLAUDE.md's generic example of running one FDB test",
	"TestDifferential_<Axis>": "the hunt-divergences skill's template, where <Axis> names the differential being run",
	"FuzzX/<hash>":            "the reproduce-a-fuzz-seed template; <hash> is the corpus entry the reader pastes",
	"FuzzXxx/<hash>":          "the same template in RFC-107's CI-gates prose",
}

// authorityCitationAllowlist are test names an authority doc cites CORRECTLY
// while the name does not resolve — which is only ever true when the doc's point
// is that the name is phantom.
var authorityCitationAllowlist = map[string]string{
	"TestFieldNameDecision": "CLAUDE.md names it as the WORKED EXAMPLE of a filter matching no function; the whole sentence is about the fact that it does not exist, so resolving it would destroy the example",
}

// testFilterFlag matches the `=`-anchored filter forms. The value runs to the
// first whitespace, quote, backtick or closing paren, which is exactly where
// every shell and every markdown span ends it.
var testFilterFlag = regexp.MustCompile("(?:^|[\\s\"'`(=])--?(?:test\\.run|test_filter|run)=([^\\s\"'`)]+)")

// testNameCitation matches a backtick-quoted Go test identifier. The backticks
// are load-bearing: an unquoted mention in flowing prose is a description, while
// a code span is a claim about an identifier.
var testNameCitation = regexp.MustCompile("`((?:Test|Fuzz|Benchmark)[A-Za-z0-9_]+)`")

// filterCitation is one extracted pattern with enough location to fix it.
type filterCitation struct {
	file    string
	line    int
	pattern string
}

func (c filterCitation) String() string {
	return fmt.Sprintf("%s:%d: %s", c.file, c.line, c.pattern)
}

// lineOf returns the 1-based line number of byte offset off in src.
func lineOf(src string, off int) int {
	return 1 + strings.Count(src[:off], "\n")
}

// extractTestFilterFlags pulls every `=`-anchored filter value out of one file's
// content. Trailing sentence punctuation is trimmed: a document ending the line
// with a comma or a full stop is punctuating the prose, not extending the
// regexp, and leaving it attached turns a correct citation into a phantom.
func extractTestFilterFlags(src string) []filterCitation {
	var out []filterCitation
	for _, m := range testFilterFlag.FindAllStringSubmatchIndex(src, -1) {
		pat := strings.TrimRight(src[m[2]:m[3]], ".,;:")
		if pat == "" {
			continue
		}
		out = append(out, filterCitation{line: lineOf(src, m[2]), pattern: pat})
	}
	return out
}

// extractTestNameCitations pulls every backtick-quoted test identifier out of
// one document's content.
func extractTestNameCitations(src string) []filterCitation {
	var out []filterCitation
	for _, m := range testNameCitation.FindAllStringSubmatchIndex(src, -1) {
		out = append(out, filterCitation{line: lineOf(src, m[2]), pattern: src[m[2]:m[3]]})
	}
	return out
}

// patternResolves reports whether pattern, read the way `go test -run` reads it,
// selects at least one of names.
//
// Only the first `/`-separated element addresses a top-level function; the rest
// address subtests, which no static inventory can enumerate. The element is an
// UNANCHORED regexp, matching the testing package.
func patternResolves(pattern string, names map[string]bool) (bool, error) {
	head := strings.SplitN(pattern, "/", 2)[0]
	re, err := regexp.Compile(head)
	if err != nil {
		return false, err
	}
	for n := range names {
		if re.MatchString(n) {
			return true, nil
		}
	}
	return false, nil
}

// testFunctionInventory is every top-level Test/Fuzz/Benchmark/Example function
// in the tracked source, keyed by name.
//
// Build tags are deliberately ignored — parser.ParseFile reads the file whatever
// its constraints say — because a filter naming a tag-gated test is a correct
// citation, and a resolver that honoured tags would report it as phantom.
func testFunctionInventory(t *testing.T, root string, files []string) map[string]bool {
	t.Helper()
	inv := map[string]bool{}
	fset := token.NewFileSet()
	for _, rel := range files {
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			// A file that does not parse cannot contribute names. It is not this
			// gate's job to police syntax (the build does that), but swallowing the
			// error silently would shrink the inventory and manufacture phantoms, so
			// it is logged.
			t.Logf("test-function inventory: skipping unparseable %s: %v", rel, err)
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			n := fd.Name.Name
			if strings.HasPrefix(n, "Test") || strings.HasPrefix(n, "Fuzz") ||
				strings.HasPrefix(n, "Benchmark") || strings.HasPrefix(n, "Example") {
				inv[n] = true
			}
		}
	}
	return inv
}

// everyTrackedFile is every tracked path, via the build-membership gate's
// git-with-filesystem-fallback so the two cannot disagree about scope.
//
// The `*` pathspec is the whole tracked set (measured: identical count to a bare
// `git ls-files`, 5414 paths of which 5377 are nested — git's pathspec globbing
// crosses `/`, so this is not a top-level-only match).
func everyTrackedFile(t *testing.T, root string) []string {
	t.Helper()
	return trackedFiles(t, root, "*")
}

// scannableForFilters reports whether a tracked path can carry a runnable
// command a reader would copy.
func scannableForFilters(rel string) bool {
	switch filepath.Ext(rel) {
	case ".md", ".yml", ".yaml", ".sh":
		return true
	}
	return filepath.Base(rel) == "justfile"
}

// The inventory floor. Measured on this tree: 12091 test functions. A floor three
// orders of magnitude below that cannot be tripped by ordinary churn, and catches
// the failure that matters — an inventory that came back empty or near-empty,
// under which EVERY citation reads as a phantom and the gate's verdict is noise.
const minTestFunctionInventory = 1000

// The Arm A population floor. Measured on this tree: 43 extracted filter values.
// A collapse means the extractor stopped matching, which looks exactly like a
// clean tree.
const minExtractedTestFilters = 20

// The Arm B population floor. Measured on this tree: 71 citations across the
// seven authority docs. Floored at 30 — low enough that pruning a doc's prose
// cannot red the build, high enough that an extractor which stopped matching
// backtick spans, or a docs set that stopped being read, cannot pass.
const minAuthorityCitations = 30

func TestEveryTestFilterFlagResolves(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	files := everyTrackedFile(t, root)
	inv := testFunctionInventory(t, root, files)
	if len(inv) < minTestFunctionInventory {
		t.Fatalf("test-function inventory holds %d name(s), want >= %d. The inventory is this gate's "+
			"denominator: with it empty or truncated every filter in the tree reads as a phantom and "+
			"every verdict below is noise. Check that the tracked-file walk reached the source tree.",
			len(inv), minTestFunctionInventory)
	}

	var found []filterCitation
	for _, rel := range files {
		if !scannableForFilters(rel) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, c := range extractTestFilterFlags(string(b)) {
			c.file = rel
			found = append(found, c)
		}
	}
	if len(found) < minExtractedTestFilters {
		t.Fatalf("extracted %d test-filter flag(s) from the tracked docs, want >= %d. A gate that "+
			"extracts nothing reports the same green as a clean tree; this floor is what separates "+
			"them. Either the tree lost its `bazelisk test --test_arg=\"--test.run=…\"` recipes or "+
			"testFilterFlag stopped matching them.", len(found), minExtractedTestFilters)
	}

	var bad []string
	usedPlaceholder := map[string]bool{}
	for _, c := range found {
		if _, ok := testFilterPlaceholders[c.pattern]; ok {
			usedPlaceholder[c.pattern] = true
			continue
		}
		ok, err := patternResolves(c.pattern, inv)
		switch {
		case err != nil:
			bad = append(bad, c.String()+" — not a valid regexp: "+err.Error())
		case !ok:
			bad = append(bad, c.String()+" — matches no test function")
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("%d test-filter flag(s) match no test function:\n  %s\n"+
			"Running any of these prints PASS with zero `=== RUN` lines — a green about the FILTER, "+
			"not about the code. Fix the name, or, if it is a template a reader substitutes into, add "+
			"it to testFilterPlaceholders with the reason.", len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("Arm A: %d filter flag(s) over %d test function(s); %d placeholder value(s) in use",
		len(found), len(inv), len(usedPlaceholder))

	for pat, why := range testFilterPlaceholders {
		if !usedPlaceholder[pat] {
			t.Errorf("testFilterPlaceholders holds %q (%s) but no document uses it. A stale exemption "+
				"is a hole nobody can see; drop the entry.", pat, why)
		}
	}
}

func TestEveryAuthorityDocTestCitationResolves(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	files := everyTrackedFile(t, root)
	inv := testFunctionInventory(t, root, files)
	if len(inv) < minTestFunctionInventory {
		t.Fatalf("test-function inventory holds %d name(s), want >= %d — see "+
			"TestEveryTestFilterFlagResolves for why an empty denominator voids this verdict.",
			len(inv), minTestFunctionInventory)
	}

	var found []filterCitation
	seenDoc := map[string]bool{}
	for _, rel := range files {
		if !authorityDocs[rel] {
			continue
		}
		seenDoc[rel] = true
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, c := range extractTestNameCitations(string(b)) {
			c.file = rel
			found = append(found, c)
		}
	}
	for doc := range authorityDocs {
		if !seenDoc[doc] {
			t.Errorf("authorityDocs names %q but the tracked-file walk never saw it. An authority "+
				"listed and never read contributes a silent zero to this gate.", doc)
		}
	}
	if len(found) < minAuthorityCitations {
		t.Fatalf("extracted %d test-name citation(s) from the authority docs, want >= %d. Below this "+
			"the arm is reporting an absence of EXTRACTION rather than an absence of drift.",
			len(found), minAuthorityCitations)
	}

	var bad []string
	usedAllow := map[string]bool{}
	for _, c := range found {
		if _, ok := authorityCitationAllowlist[c.pattern]; ok {
			usedAllow[c.pattern] = true
			continue
		}
		if !inv[c.pattern] {
			bad = append(bad, c.String())
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("%d test-name citation(s) in the authority docs name no test function:\n  %s\n"+
			"These documents describe the tree AS IT IS, so a name they cite is a claim that a reader "+
			"can go run it — and running it prints PASS over zero tests. Correct the name, drop the "+
			"claim, or, if the doc's point is precisely that the name is phantom, record it in "+
			"authorityCitationAllowlist with that reason.", len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("Arm B: %d citation(s) across %d authority doc(s)", len(found), len(seenDoc))

	for name, why := range authorityCitationAllowlist {
		if !usedAllow[name] {
			t.Errorf("authorityCitationAllowlist holds %q (%s) but no authority doc cites it. Drop "+
				"the entry rather than leaving an unused exemption behind.", name, why)
		}
	}
}

// TestTodoTestCitationDriftIsReported keeps Arm B's forward-looking exclusion
// VISIBLE.
//
// The planning docs are excluded from Arm B on purpose — an unchecked item naming
// the test it will add is correct prose about a test that must not exist yet — but
// an exclusion that produces no output is how a scope decision becomes invisible.
// This measures the drift instead of asserting on it, and it fails only if the
// planning docs stop citing test names AT ALL, which would mean the measurement
// itself had died.
func TestTodoTestCitationDriftIsReported(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	files := everyTrackedFile(t, root)
	inv := testFunctionInventory(t, root, files)
	total, unresolved := 0, map[string]string{}
	for _, rel := range files {
		if !forwardLookingPlanDocs[rel] {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, c := range extractTestNameCitations(string(b)) {
			total++
			if !inv[c.pattern] {
				unresolved[c.pattern] = fmt.Sprintf("%s:%d", rel, c.line)
			}
		}
	}
	if total == 0 {
		t.Fatalf("the forward-looking planning docs cite no test names at all. That is not a clean "+
			"result — it means this measurement stopped reading them, and the scope Arm B excludes "+
			"went dark with it. Docs read: %v", forwardLookingPlanDocs)
	}
	names := make([]string, 0, len(unresolved))
	for n := range unresolved {
		names = append(names, n+" ("+unresolved[n]+")")
	}
	sort.Strings(names)
	t.Logf("planning-doc test citations: %d total, %d distinct unresolved (expected non-zero: an "+
		"unchecked item names the test it will add).\n  %s", total, len(unresolved), strings.Join(names, "\n  "))
}

// TestTestFilterExtractorMatchesEveryDocumentedForm drives the extractor over
// every form the gate claims to cover and every form it claims to reject.
//
// Without this the two tree arms are unfalsifiable: an extractor that matched
// nothing would report a clean tree, and the population floors above would be the
// only thing between that and a permanently green gate.
func TestTestFilterExtractorMatchesEveryDocumentedForm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{"go test long flag", "go test ./pkg -test.run=TestAlpha\n", []string{"TestAlpha"}},
		{"double dash", "go test ./pkg --test.run=TestAlpha\n", []string{"TestAlpha"}},
		{"bazel test_arg", "bazelisk test //pkg:pkg_test --test_arg=\"--test.run=TestAlpha$\"\n", []string{"TestAlpha$"}},
		{"bazel test_filter", "bazelisk test //pkg:pkg_test --test_filter=TestAlpha\n", []string{"TestAlpha"}},
		{"bare run equals", "go test -run=TestAlpha ./pkg\n", []string{"TestAlpha"}},
		{"subtest path kept whole", "go test -test.run=TestAlpha/sub_case\n", []string{"TestAlpha/sub_case"}},
		{"backtick span ends the value", "run `-test.run=TestAlpha` to check\n", []string{"TestAlpha"}},
		{"paren ends the value", "(see -test.run=TestAlpha)\n", []string{"TestAlpha"}},
		{"trailing prose comma trimmed", "use -test.run=TestAlpha, then read the log\n", []string{"TestAlpha"}},
		{"trailing full stop trimmed", "use -test.run=TestAlpha.\n", []string{"TestAlpha"}},
		{"two on one line", "-test.run=TestAlpha and --test_filter=TestBeta\n", []string{"TestAlpha", "TestBeta"}},
		{"alternation kept whole", "-test.run=TestAlpha|TestBeta\n", []string{"TestAlpha|TestBeta"}},
		// Rejected forms — each is a documented non-coverage, and a change that
		// silently started matching one would widen the gate without review.
		{"space separated run is out of scope", "go test -run TestAlpha ./pkg\n", nil},
		{"bare flag mention is not a citation", "the -test.run flag narrows the corpus\n", nil},
		{"flag named with no value", "pass -test.run= to select nothing\n", nil},
		{"a longer flag ending in run is not matched", "--gotestsum-rerun=TestAlpha\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := []string{}
			for _, c := range extractTestFilterFlags(tc.src) {
				got = append(got, c.pattern)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("extracted %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("extracted %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestTestFilterGateRejectsAPhantomName is the NEGATIVE CONTROL.
//
// It pushes a synthetic document through the exact extract-then-resolve path the
// tree arms use and requires the phantom to come out the far side as a violation,
// beside a real name that must not. A gate proven only on a clean tree has never
// been shown capable of failing, and CLAUDE.md's own worked example is a gate
// that could not.
func TestTestFilterGateRejectsAPhantomName(t *testing.T) {
	t.Parallel()
	// The inventory is stated literally rather than read from the tree: the
	// control has to hold even if every real test is renamed.
	inv := map[string]bool{"TestFieldNameNeverDecides": true, "TestAlpha": true}

	const doc = "Run `bazelisk test //pkg/docscheck:docscheck_test " +
		"--test_arg=\"--test.run=TestFieldNameDecision\"` to check the ratchet, and " +
		"`--test.run=TestFieldNameNeverDecides` for the real one.\n"

	cites := extractTestFilterFlags(doc)
	if len(cites) != 2 {
		t.Fatalf("extractor found %d filter(s) in the control document, want 2 — the control cannot "+
			"prove the gate fires if the extractor never sees the phantom: %v", len(cites), cites)
	}

	got := map[string]bool{}
	for _, c := range cites {
		ok, err := patternResolves(c.pattern, inv)
		if err != nil {
			t.Fatalf("resolving %q: %v", c.pattern, err)
		}
		got[c.pattern] = ok
	}
	if got["TestFieldNameDecision"] {
		t.Fatalf("the gate RESOLVED the phantom TestFieldNameDecision against an inventory that does "+
			"not contain it. This is the exact defect the gate exists to catch, so a gate that passes "+
			"here cannot catch anything: inventory %v", inv)
	}
	if !got["TestFieldNameNeverDecides"] {
		t.Fatalf("the gate REJECTED TestFieldNameNeverDecides, which is in the inventory. Without this " +
			"half the control is satisfied by a resolver that rejects everything, which would red the " +
			"whole tree rather than catch a phantom.")
	}

	// The same control on Arm B's citation path, which reads names out of prose
	// rather than out of a command.
	const prose = "pinned by `TestFieldNameDecision` and by `TestAlpha`\n"
	names := extractTestNameCitations(prose)
	if len(names) != 2 {
		t.Fatalf("citation extractor found %d name(s) in the control prose, want 2: %v", len(names), names)
	}
	if inv[names[0].pattern] || !inv[names[1].pattern] {
		t.Fatalf("citation control: want %q unresolved and %q resolved against %v",
			names[0].pattern, names[1].pattern, inv)
	}
}

// TestPatternResolvesReadsAFilterTheWayGoTestDoes pins the resolution semantics
// themselves, which are what makes a "matches nothing" verdict trustworthy.
func TestPatternResolvesReadsAFilterTheWayGoTestDoes(t *testing.T) {
	t.Parallel()
	inv := map[string]bool{"TestAlphaOrdering": true, "FuzzBeta": true}
	for _, tc := range []struct {
		name    string
		pattern string
		want    bool
		wantErr bool
	}{
		{"unanchored substring matches, as -run does", "AlphaOrder", true, false},
		{"exact name matches", "TestAlphaOrdering", true, false},
		{"anchored prefix matches", "^TestAlpha", true, false},
		{"anchored whole name matches", "^TestAlphaOrdering$", true, false},
		{"anchored whole name that is a PREFIX of a real one does not", "^TestAlpha$", false, false},
		{"subtest suffix is ignored — only the head addresses a function", "TestAlphaOrdering/case_1", true, false},
		{"a phantom head is not rescued by a real subtest suffix", "TestAlphaOrderin9/case_1", false, false},
		{"alternation resolves when any branch does", "TestNoSuch|FuzzBeta", true, false},
		{"alternation with no live branch does not", "TestNoSuch|FuzzNoSuch", false, false},
		{"a phantom resolves to nothing", "TestFieldNameDecision", false, false},
		{"an invalid regexp is an error, not a silent pass", "TestAlpha(", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := patternResolves(tc.pattern, inv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("patternResolves(%q) err = %v, wantErr %v", tc.pattern, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("patternResolves(%q) = %v, want %v", tc.pattern, got, tc.want)
			}
		})
	}
}
