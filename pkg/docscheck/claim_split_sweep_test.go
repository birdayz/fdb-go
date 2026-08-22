package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A SUPERSEDED CLAIM IS SWEPT FOR WITH A PHRASE, AND SOURCE FORMATTING BREAKS
// PHRASES IN TWO WAYS THAT A LINE-ORIENTED SEARCH CANNOT REJOIN.
//
// Both were hit while retiring one stale count from this repository, in that
// order, each time producing a confident zero from `grep` for a claim that was
// demonstrably still in the tree:
//
//	  1. COMMENT WRAPPING. `gofumpt` and ordinary editing split a sentence at a
//	     column boundary, so "544 call sites" becomes "544 call\n// sites".
//	  2. STRING-LITERAL CONCATENATION. The same sentence inside a t.Fatal is
//	     split as `"544 call " + "sites"`, which survives a sweep that only
//	     knows about (1). This is the shape that actually escaped, and it did so
//	     in the same file as a comment promising that file restated no counts.
//
// flattenClaimText collapses both. Its reach is stated by two tables rather
// than by this prose, because a gate whose sentence exceeds its code is the
// exact failure the sweep exists to catch.
//
// WHAT IT DOES NOT SEE, first and enumerated, every entry observed by running it
// (TestFlattenClaimTextDoesNotSeeTheseSplitShapes): a claim assembled at runtime
// from a variable or format verb; a claim broken by an ESCAPED newline inside one
// literal, where the separator is the two source characters `\` and `n` and not
// whitespace at all; a count spelled in words instead of digits; a claim
// straddling two files.
//
// WHAT IT DOES SEE, at 9 arms (TestFlattenClaimTextCollapsesEverySplitShape):
// comment wrapping; adjacent-literal concatenation in all four quote pairings
// -- interpreted/interpreted, raw/raw, and both mixed -- and, for the
// interpreted pair only, both with and without whitespace around the `+`; a
// concatenation appearing inside a wrapped comment; and a claim spanning the
// middle join of three literals.

// goConcatSplit matches the join between two adjacent Go string literals,
// including the degenerate `"a"+"b"` spelling with no surrounding space and the
// raw-literal and mixed spellings. Restricting this to double quotes was an
// over-claim caught by re-reading the sentence above against the pattern: a
// message assembled from raw literals is the same defect wearing a different
// quote character.
var goConcatSplit = regexp.MustCompile("[\"`]\\s*\\+\\s*[\"`]")

// flattenClaimText renders a source file as one line with both split shapes
// closed up. Comment markers become spaces (so `https://x` flattens to
// `https: //x` -- harmless for phrase claims, and noted so nobody reads a
// mangled URL as a bug), literal concatenation joins, and all runs of
// whitespace collapse to a single space.
func flattenClaimText(src string) string {
	s := strings.ReplaceAll(src, "//", " ")
	s = goConcatSplit.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}

func TestFlattenClaimTextCollapsesEverySplitShape(t *testing.T) {
	t.Parallel()

	const claim = "544 call sites scan with it"

	cases := []struct {
		name string
		src  string
	}{
		{
			name: "unsplit prose in a comment",
			src:  "// 544 call sites scan with it.\n",
		},
		{
			name: "comment wrapping",
			src:  "// because 544 call\n// sites scan with it and only the JVM suite catches it\n",
		},
		{
			name: "string concatenation with a newline between the literals",
			src:  "panic(\"because 544 call \" +\n\t\t\"sites scan with it\")\n",
		},
		{
			name: "string concatenation with no surrounding whitespace",
			src:  "panic(\"because 544 call \"+\"sites scan with it\")\n",
		},
		{
			name: "three literals, the claim spanning the middle join",
			src:  "panic(\"a\" +\n\t\"because 544 call \" +\n\t\"sites scan with it\")\n",
		},
		{
			name: "concatenation inside a wrapped comment block",
			src:  "// the message reads\n// \"544 call \" + \"sites scan with it\"\n",
		},
		{
			name: "raw literals joined with backticks",
			src:  "panic(`because 544 call ` +\n\t\t`sites scan with it`)\n",
		},
		{
			name: "mixed interpreted-then-raw literal",
			src:  "panic(\"because 544 call \" +\n\t\t`sites scan with it`)\n",
		},
		{
			name: "mixed raw-then-interpreted literal",
			src:  "panic(`because 544 call ` +\n\t\t\"sites scan with it\")\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flat := flattenClaimText(tc.src)
			if !strings.Contains(flat, claim) {
				t.Fatalf("flattenClaimText did not rejoin the claim.\n  input:     %q\n  flattened: %q\n  wanted to contain: %q",
					tc.src, flat, claim)
			}
		})
	}
}

// The negative arm. Without it every case above is satisfiable by a function
// that returns the claim unconditionally, and the table would be vacuous.
func TestFlattenClaimTextDoesNotInventAClaim(t *testing.T) {
	t.Parallel()

	// Deliberately adjacent to the real wording, so only a genuine match passes:
	// the digits differ and the noun is plural-vs-singular.
	const src = "// because 545 call\n// site scans with it\n"
	flat := flattenClaimText(src)
	if strings.Contains(flat, "544 call sites scan with it") {
		t.Fatalf("flattenClaimText fabricated a claim that is not in the source: %q", flat)
	}
	// ... while still proving the flattener RAN on this input, so the pass above
	// is not the empty-set green this repository keeps rediscovering.
	if !strings.Contains(flat, "545 call site scans with it") {
		t.Fatalf("the flattener did not process the negative fixture at all: %q", flat)
	}
}

// THE NOT-COVERED LIST, pinned rather than described, and enumerated rather
// than characterised. Each entry is a shape that was RUN through the flattener
// and observed not to rejoin -- which is the only way to write a limit that can
// be checked. A shape that starts passing here is not a test failure to silence:
// it means the reach grew and the doc comment above now understates it.
func TestFlattenClaimTextDoesNotSeeTheseSplitShapes(t *testing.T) {
	t.Parallel()

	const claim = "544 call sites"

	cases := []struct {
		name string
		src  string
		why  string
	}{
		{
			name: "assembled at runtime from a variable",
			src:  "n := 544\npanic(fmt.Sprintf(\"%d call sites scan with it\", n))\n",
			why:  "the digits and the noun never sit in the same literal, so no textual pass can join them",
		},
		{
			name: "escaped newline inside a single literal",
			src:  "panic(\"because 544 call\\nsites scan with it\")\n",
			why: "the two characters backslash-n sit between the words in the SOURCE bytes; strings.Fields " +
				"splits on real whitespace and cannot see an escape as a separator",
		},
		{
			name: "the count spelled as words",
			src:  "// five hundred and forty-four call sites scan with it\n",
			why:  "the sweep is a digit pattern; a prose spelling is a different claim to search for",
		},
		{
			name: "split across two files",
			src:  "// because 544 call\n", // the other half lives elsewhere
			why:  "flattening is per-file by construction, so a claim straddling files is out of reach",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(flattenClaimText(tc.src), claim) {
				t.Fatalf("this gate is documented as blind to %q (%s), and it just saw one.\n"+
					"That is good news for coverage and bad news for the doc comment above flattenClaimText, "+
					"which now understates the reach — widen the positive claim and move this case up to the covered table.",
					tc.name, tc.why)
			}
		})
	}
}

// withdrawnIndexCallSiteCount matches either half of the retired figure. Both
// numbers were correct measurements of different populations -- 545 counts every
// textual occurrence of ScanIndex/RebuildIndex/SetIndex under pkg/ cmd/
// conformance/, 544 the same with the generated ANTLR parser excluded -- and
// neither counted the population the sentence around them claimed. They are
// withdrawn in DIVERGENCES.md; this keeps them from drifting back in as a live
// claim somewhere else.
var withdrawnIndexCallSiteCount = regexp.MustCompile(`\b54[45]\b[^.]{0,20}?call sites`)

func TestWithdrawnIndexCallSiteCountsDoNotReappear(t *testing.T) {
	t.Parallel()

	// sourceTreeRoot, NOT repoRoot. Under Bazel this test runs in a runfiles
	// tree holding only its declared `data`, so repoRoot's `pkg/` contains the
	// few dozen files staged for OTHER gates in this package and none of the
	// sources being swept. An earlier revision used repoRoot and the scanned
	// floor below caught it at 64 files -- which is the floor doing its job,
	// and the reason it is a hard failure rather than a log line.
	root := sourceTreeRoot(t)

	// A phrase known to be present, carried through the identical code path, so
	// a zero-hit result is a statement about the tree and not about the walk.
	control := regexp.MustCompile(`RebuildIndex`)

	// The gate's own file states the retired figure in its doc comment and in
	// every fixture below, so sweeping itself it always finds itself -- the same
	// self-match that makes `pgrep -f <marker>` useless for detecting the
	// process that ran it. It is excluded BY PATH and the exclusion is proven to
	// have fired (selfSeen), because an exclusion that silently stops matching
	// is indistinguishable from one that is working.
	const selfPath = "pkg/docscheck/claim_split_sweep_test.go"
	selfSeen := false

	var (
		scanned    int
		controlHit int
		offenders  []string
		mdHits     []string
	)

	// trackedGoFiles is git-scoped with a filesystem fallback that skips
	// `.claude/worktrees/`. Both halves matter here: stale agent worktrees carry
	// pre-withdrawal copies of exactly this claim, and sweeping them would
	// report findings about code that was never on this branch.
	for _, rel := range trackedGoFiles(t, root) {
		switch {
		case rel == selfPath:
			selfSeen = true
			continue
		case !strings.HasPrefix(rel, "pkg/") && !strings.HasPrefix(rel, "cmd/") && !strings.HasPrefix(rel, "conformance/"):
			continue
		case strings.Contains(rel, "/parser/gen/"):
			// Generated ANTLR output: megabytes of numeric tables, no prose.
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(root, rel))
		if readErr != nil {
			continue
		}
		scanned++
		flat := flattenClaimText(string(b))
		if control.MatchString(flat) {
			controlHit++
		}
		if loc := withdrawnIndexCallSiteCount.FindStringIndex(flat); loc != nil {
			start := loc[0] - 80
			if start < 0 {
				start = 0
			}
			end := loc[1] + 80
			if end > len(flat) {
				end = len(flat)
			}
			offenders = append(offenders, fmt.Sprintf("%s\n     …%s…", rel, flat[start:end]))
		}
	}

	// Vacuity guards, both directions. A collapse of `scanned` means the walk
	// found nothing to read and every conclusion below is empty; a collapse of
	// `controlHit` means it read files but the flattener destroyed them.
	if scanned < 500 {
		t.Fatalf("swept only %d files; this tree has thousands of .go files under pkg/ cmd/ conformance/, so the walk is broken and the zero below means nothing", scanned)
	}
	if controlHit == 0 {
		t.Fatalf("positive control %q matched 0 of %d swept files; the sweep cannot be trusted to find anything", control, scanned)
	}
	if !selfSeen {
		t.Fatalf("this gate's own file (%s) was never offered to the sweep, so its self-exclusion is dead code. "+
			"Either the file was renamed -- update selfPath -- or trackedGoFiles stopped returning it, which would mean "+
			"the corpus is narrower than the floor above implies", selfPath)
	}

	if len(offenders) != 0 {
		t.Fatalf("a withdrawn index-call-site count is stated as a live claim in %d Go file(s):\n  %s\n\n"+
			"Both 544 and 545 were real measurements of populations that were NOT the one the sentence around them described. "+
			"State the population, or cite DIVERGENCES.md where they are withdrawn — do not restate the number.",
			len(offenders), strings.Join(offenders, "\n  "))
	}

	// The prose side. TODO.md restated one of the figures for two revisions after
	// DIVERGENCES.md had already qualified it, so the living docs are swept for
	// the same claim shape as the sources.
	for _, doc := range []string{"TODO.md", "README.md", "DIVERGENCES.md"} {
		b, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			continue
		}
		flat := flattenClaimText(string(b))
		loc := withdrawnIndexCallSiteCount.FindStringIndex(flat)
		if loc == nil {
			continue
		}
		mdHits = append(mdHits, doc)
		window := flat[max(0, loc[0]-1200):min(len(flat), loc[1]+1200)]
		if !strings.Contains(window, "WITHDRAWN") && !strings.Contains(window, "withdrawn") {
			t.Errorf("%s states a withdrawn index-call-site count with no withdrawal in the surrounding 1200 chars:\n  …%s…", doc, window)
		}
	}
	_ = mdHits // a doc may legitimately hold zero; the anchor below is what must hold.

	// THE ANCHOR THIS GATE IS ORPHANED WITHOUT. Everything above is a negative
	// check, and a negative check passes just as well when the thing it protects
	// has been deleted. So the withdrawal itself is pinned positively: if it goes,
	// this gate is watching for the return of a number nothing explains, and the
	// failure below says so rather than letting the sweep pass into irrelevance.
	//
	// Note the two figures appear in that entry as BARE numbers, deliberately —
	// the entry recounts how they were measured without restating either as a
	// call-site claim, which is why the regex above does not match there and why
	// this anchor is a separate check rather than a window around a hit.
	div, err := os.ReadFile(filepath.Join(root, "DIVERGENCES.md"))
	if err != nil {
		t.Fatalf("reading DIVERGENCES.md, which holds the withdrawal this gate depends on: %v", err)
	}
	divFlat := flattenClaimText(string(div))
	for _, anchor := range []string{
		"THE EXACT FIGURE IS WITHDRAWN",
		"gives 545",
		"gives 544",
	} {
		if !strings.Contains(divFlat, anchor) {
			t.Fatalf("DIVERGENCES.md no longer contains %q, so the withdrawal that explains this gate is gone. "+
				"Reconcile the gate with the new expected state rather than deleting it: with the entry removed, a "+
				"reappearing count is unexplained rather than merely stale, and the alarm direction has inverted.", anchor)
		}
	}
}
