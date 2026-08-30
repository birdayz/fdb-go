package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docNameVerdict is what the leading-name check concluded about one doc comment.
type docNameVerdict int

const (
	// docNameAbsent: the comment does not open with a test-function name. Either
	// there is no doc comment, or it opens with prose. "Test that the cursor
	// resumes" opens with the English WORD, not a name, and is not a citation —
	// the first cut of this gate missed that distinction and reported 7 such
	// comments as defects.
	docNameAbsent docNameVerdict = iota
	// docNameMatches: the comment opens with the name of the function it documents.
	docNameMatches
	// docNameMisattributed: the comment opens with the name of a DIFFERENT test
	// function that exists in the tree. Godoc renders that prose under the wrong
	// function, and a reader grepping for the cited name lands on the wrong body.
	docNameMisattributed
	// docNamePhantom: the comment opens with a test-function name that exists
	// NOWHERE in the tree. Usually a rename that updated the declaration and left
	// the prose behind; the citation gates in test_filter_resolution_test.go catch
	// this shape in Markdown and in test filters, but never in Go source comments.
	docNamePhantom
)

func (v docNameVerdict) String() string {
	switch v {
	case docNameAbsent:
		return "absent"
	case docNameMatches:
		return "matches"
	case docNameMisattributed:
		return "misattributed"
	case docNamePhantom:
		return "phantom"
	}
	return "unknown"
}

// leadingTestName matches a godoc opener that is a test-function NAME.
//
// The [A-Z_] after the prefix is load-bearing, not decoration: without it the
// English words "Test", "Fuzz" and "Benchmark" — which open ordinary prose —
// parse as citations. Anchoring at the start is the other half: a name mentioned
// mid-sentence ("see TestFoo for the other half") is a cross-reference, not a
// claim about the function below.
var leadingTestName = regexp.MustCompile(`^(Test|Fuzz|Benchmark)[A-Z_][A-Za-z0-9_]*`)

// classifyDocName is the whole decision, lifted out of the tree walk so every
// arm can be driven by a unit test rather than by whatever the corpus happens to
// contain today. doc is the doc comment's first line, markers already stripped;
// fn is the function it is attached to; inv is the test-function inventory.
func classifyDocName(doc, fn string, inv map[string]bool) (docNameVerdict, string) {
	cited := leadingTestName.FindString(strings.TrimSpace(doc))
	switch {
	case cited == "":
		return docNameAbsent, ""
	case cited == fn:
		return docNameMatches, cited
	case inv[cited]:
		return docNameMisattributed, cited
	default:
		return docNamePhantom, cited
	}
}

type docNameFinding struct {
	file, cited, fn string
	line            int
	verdict         docNameVerdict
}

// scanFileForDocNames applies the decision to one parsed file, and is separate
// from the corpus loop for the same reason classifyDocName is separate from it:
// the WALK makes exclusions of its own — methods, non-test declarations,
// undocumented functions, every line after the first — and those are claims
// about coverage that a unit test must be able to drive directly. Inferring them
// from a clean corpus reading is how a scope sentence ends up broader than the
// code it describes.
func scanFileForDocNames(fset *token.FileSet, f *ast.File, rel string, inv map[string]bool) (found []docNameFinding, scanned, documented int) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil {
			continue
		}
		name := fd.Name.Name
		if !strings.HasPrefix(name, "Test") && !strings.HasPrefix(name, "Fuzz") &&
			!strings.HasPrefix(name, "Benchmark") {
			continue
		}
		scanned++
		if fd.Doc == nil || len(fd.Doc.List) == 0 {
			continue
		}
		documented++
		// Doc.Text() strips the markers for BOTH comment spellings and drops
		// directives. Reading List[0].Text and trimming "//" by hand goes silently
		// blind on a /* */ doc block — a hole rather than a documented limit. No
		// such doc exists in the tree today (zero hits for a line-initial "/*"
		// across the 2239 test files that declare a test function), which is
		// exactly why the hand-rolled version would never have been noticed.
		first, _, _ := strings.Cut(fd.Doc.Text(), "\n")
		verdict, cited := classifyDocName(first, name, inv)
		if verdict == docNameAbsent || verdict == docNameMatches {
			continue
		}
		found = append(found, docNameFinding{
			file: rel, line: fset.Position(fd.Pos()).Line,
			cited: cited, fn: name, verdict: verdict,
		})
	}
	return found, scanned, documented
}

// TestDocCommentNamesItsOwnFunction rejects a test doc comment that opens with
// the name of a DIFFERENT test function. That is narrower than godoc's naming
// convention, deliberately, and the two are easy to conflate.
//
// What it does NOT check — every shape below driven against the code in
// TestScanFileForDocNames_WalkExclusions and TestClassifyDocName_DrivesEveryArm,
// not inferred from a clean corpus reading:
//
//   - a doc comment that never names its function ("Pins the cursor contract").
//     Permitted. Requiring the name is a style rule over thousands of comments;
//     this gate is about a comment that is WRONG, not one that is terse.
//   - a function carrying no doc comment at all. Permitted, same reason.
//   - a wrong name appearing anywhere but first ("See TestFoo for the other
//     half"). That is a cross-reference, not a claim about the function below.
//   - a method, or any declaration not named Test/Fuzz/Benchmark. Helpers and
//     production-file comments citing tests are unscanned.
//   - the second and later lines of the block. Only the first line is read, so a
//     wrong name deeper in the prose passes.
//
// It has no known false positive. The first draft claimed one — a comment
// opening with a SUBTEST name — and that was wrong: the name truncates at the
// slash and resolves to the PARENT test, which on its own function is a match.
// Both halves are pinned in TestClassifyDocName_DrivesEveryArm.
//
// This is worth a gate because nothing else here could see it. The citation
// gates in test_filter_resolution_test.go read Markdown authority docs and
// test-filter flags, never Go source comments — and forty of those comments were
// wrong when this gate was written.
//
// They were not one defect, which is why the failure reports the name pair and
// refuses to suggest a fix. Four kinds appeared:
//
//   - SYNONYM RENAME: the function was renamed and the body still fits. The
//     leading name is the only thing to change.
//   - MISPLACED BLOCK: a later insertion separated a block from its function with
//     no blank line, so godoc attached it to whatever followed while its real
//     subject carried none. The block must MOVE, not be renamed — it was never
//     about the function it drifted onto.
//   - SUPERSEDED BLOCK: prose describing behaviour the code no longer has, glued
//     above the test for the NEW behaviour. One had godoc rendering a retired
//     "returns nil" contract as the current one.
//   - DRIFTED BODY: the description no longer matches what the test asserts. Here
//     a name-only rewrite is HARMFUL — it gives a wrong body a matching name and
//     converts a grep-detectable defect into an invisible one.
//
// A bulk rename over this gate's output was tried once and reverted for exactly
// the last reason.
func TestDocCommentNamesItsOwnFunction(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	files := trackedTestFiles(t, root)
	inv := testFunctionInventory(t, root, files)

	var found []docNameFinding
	scanned, documented := 0, 0

	fset := token.NewFileSet()
	for _, rel := range files {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.ParseComments)
		if err != nil {
			// Syntax is the build's job, not this gate's — but a swallowed parse
			// error shrinks the scan silently, which is the one failure the floors
			// below exist to catch. Log it so a shrinking population has a cause.
			t.Logf("doc-name gate: skipping unparseable %s: %v", rel, err)
			continue
		}
		fileFound, fileScanned, fileDocumented := scanFileForDocNames(fset, f, rel, inv)
		found = append(found, fileFound...)
		scanned += fileScanned
		documented += fileDocumented
	}

	// Two floors, because two different collapses make a clean verdict
	// indistinguishable from a gate that stopped looking.
	if scanned < minTestFunctionInventory {
		t.Fatalf("doc-name gate scanned %d test functions, want >= %d — the tracked-file walk "+
			"or the parse stopped reaching the tree, and a zero from it means nothing",
			scanned, minTestFunctionInventory)
	}
	// The DOCUMENTED floor is the one unique to this gate: a walk that finds every
	// function but attaches no comments reports zero forever and looks perfect.
	// Measured with this file present: 6260 documented of 13284 scanned, 47%.
	// Floored at a tenth — beyond ordinary churn, and unreachable by a walk whose
	// comment attachment has died.
	if documented < scanned/10 {
		t.Fatalf("only %d of %d scanned test functions carried a doc comment (want >= %d) — comment "+
			"attachment is not working, so every function looks undocumented and no mismatch "+
			"can be found", documented, scanned, scanned/10)
	}
	t.Logf("doc-name gate: %d documented of %d scanned test functions, %d citation(s) wrong",
		documented, scanned, len(found))

	for _, m := range found {
		switch m.verdict {
		case docNameMisattributed:
			t.Errorf("%s:%d: doc comment opens with %q but documents %q.\n"+
				"    READ THE FUNCTION before changing anything. A name-only rewrite is right only "+
				"when the body still describes %q; if the block belongs to %q it must be MOVED "+
				"there, and if the body describes behaviour the test no longer has, renaming it "+
				"hides that instead of fixing it.", m.file, m.line, m.cited, m.fn, m.fn, m.cited)
		case docNamePhantom:
			t.Errorf("%s:%d: doc comment opens with %q, which is not a test function anywhere in "+
				"the tree, and documents %q. Either the declaration was renamed and the prose was "+
				"not, or the cited test was deleted — read the body to tell which.",
				m.file, m.line, m.cited, m.fn)
		}
	}
}

// TestClassifyDocName_DrivesEveryArm pins the decision at every outcome,
// including the two the tree cannot currently produce. That is the point: the
// corpus reading is zero, so a corpus-only check exercises the "matches" and
// "absent" arms and NOTHING else — the first firing of either real arm would be
// the first time it ran.
func TestClassifyDocName_DrivesEveryArm(t *testing.T) {
	t.Parallel()
	inv := map[string]bool{"TestRealOther": true, "TestSubject": true, "BenchmarkReal": true}

	for _, tc := range []struct {
		name, doc, fn string
		want          docNameVerdict
		wantCited     string
	}{
		{"matching name", "TestSubject asserts the thing.", "TestSubject", docNameMatches, "TestSubject"},
		{"english word Test", "Test that the cursor resumes.", "TestSubject", docNameAbsent, ""},
		{"english word Benchmark", "Benchmark for the overhead.", "BenchmarkThing", docNameAbsent, ""},
		{"english word Fuzz", "Fuzz the encoder.", "FuzzThing", docNameAbsent, ""},
		{"prose with no prefix", "Pins the cursor contract.", "TestSubject", docNameAbsent, ""},
		{"name mid-sentence only", "See TestRealOther for the other half.", "TestSubject", docNameAbsent, ""},
		{"misattributed to a real test", "TestRealOther pins the sibling.", "TestSubject", docNameMisattributed, "TestRealOther"},
		{"misattributed benchmark", "BenchmarkReal measures it.", "BenchmarkOther", docNameMisattributed, "BenchmarkReal"},
		{"phantom, renamed away", "TestLongGone pins the old shape.", "TestSubject", docNamePhantom, "TestLongGone"},
		{"phantom fuzz target", "FuzzGoneAway explores it.", "TestSubject", docNamePhantom, "FuzzGoneAway"},
		// A subtest citation truncates at the slash and resolves to the PARENT
		// test, so on its own function it is a match and not a false positive.
		// The first draft of this gate documented the opposite as a known cost;
		// this row is why that claim is not in the doc comment.
		{"subtest name on its own test", "TestSubject/the_empty_case is the interesting one.", "TestSubject", docNameMatches, "TestSubject"},
		{"subtest name on another test", "TestSubject/the_empty_case is the interesting one.", "TestOther", docNameMisattributed, "TestSubject"},
		{"underscore continuation", "Test_Subject does the thing.", "TestSubject", docNamePhantom, "Test_Subject"},
		{"empty comment", "", "TestSubject", docNameAbsent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, cited := classifyDocName(tc.doc, tc.fn, inv)
			if got != tc.want || cited != tc.wantCited {
				t.Fatalf("classifyDocName(%q, %q) = %v/%q, want %v/%q",
					tc.doc, tc.fn, got, cited, tc.want, tc.wantCited)
			}
		})
	}

	// Every arm must be reachable, or a table that quietly lost a case would
	// still pass. This asserts the table drives all four, not merely that each
	// row agrees with itself.
	seen := map[docNameVerdict]int{}
	for _, doc := range []string{
		"TestSubject x", "Test that x", "TestRealOther x", "TestLongGone x",
	} {
		v, _ := classifyDocName(doc, "TestSubject", inv)
		seen[v]++
	}
	for _, want := range []docNameVerdict{docNameMatches, docNameAbsent, docNameMisattributed, docNamePhantom} {
		if seen[want] == 0 {
			t.Errorf("no input in this test produces the %v arm — it ships untested", want)
		}
	}
}

// TestScanFileForDocNames_WalkExclusions drives the exclusions the WALK makes,
// which classifyDocName never sees. Each is a line in the gate's not-covered
// list, and a not-covered list written from the code rather than from a run is
// the specific way a scope sentence ends up broader than its gate.
func TestScanFileForDocNames_WalkExclusions(t *testing.T) {
	t.Parallel()
	inv := map[string]bool{"TestRealOther": true}

	const src = `package p

import "testing"

type h struct{}

// TestRealOther is cited on a METHOD, which the walk skips.
func (h) TestMethodShaped(t *testing.T) {}

// TestRealOther is cited on a helper, which is not a test declaration.
func helperNotATest(t *testing.T) {}

func TestUndocumented(t *testing.T) {}

// Pins the cursor contract without naming itself.
func TestProseLeading(t *testing.T) {}

// A first line that names nothing.
// TestRealOther appears only on the SECOND line and is not read.
func TestSecondLineIgnored(t *testing.T) {}

/* TestRealOther in a block comment is read like any other doc. */
func TestBlockCommentForm(t *testing.T) {}

// TestRealOther is cited here and this one really is wrong.
func TestGenuinelyMisattributed(t *testing.T) {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "exclusions.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	found, scanned, documented := scanFileForDocNames(fset, f, "exclusions.go", inv)

	// Five Test-prefixed top-level declarations — TestUndocumented, TestProseLeading,
	// TestSecondLineIgnored, TestBlockCommentForm, TestGenuinelyMisattributed. The
	// METHOD and the non-test helper are excluded and are the reason this is 5 and
	// not 7. Asserting the counts is what makes the exclusions checkable: a walk
	// that skipped everything would also report zero findings.
	//
	// The first draft of this assertion said 6 and 5, off by one in both, because
	// the numbers were predicted rather than run. That is the failure this whole
	// file is built around, so it is worth naming where it happened.
	if scanned != 5 {
		t.Errorf("scanned %d declarations, want 5 (the method and the non-test helper are excluded)", scanned)
	}
	if documented != 4 {
		t.Errorf("documented %d, want 4 (TestUndocumented carries no doc comment)", documented)
	}

	// Exactly two fire: the block-comment form and the plain one. Both are
	// genuinely misattributed; everything else above is a documented exclusion.
	wantFiring := map[string]bool{"TestBlockCommentForm": true, "TestGenuinelyMisattributed": true}
	got := map[string]docNameVerdict{}
	for _, m := range found {
		got[m.fn] = m.verdict
	}
	if len(got) != len(wantFiring) {
		t.Errorf("gate fired on %v, want exactly %v", got, wantFiring)
	}
	for fn := range wantFiring {
		if got[fn] != docNameMisattributed {
			t.Errorf("%s: verdict %v, want misattributed — this shape must NOT be excluded", fn, got[fn])
		}
	}
	// The block-comment arm is the one that would regress silently if the marker
	// stripping went back to trimming "//" by hand, so name it in its own check.
	if got["TestBlockCommentForm"] != docNameMisattributed {
		t.Errorf("a /* */ doc block was not read — the gate is blind to that spelling")
	}
}
