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
// WHAT IT DOES NOT SEE, first and enumerated, at 5 arms every one of which was
// observed by running it (TestFlattenClaimTextDoesNotSeeTheseSplitShapes): a
// claim assembled at runtime from a variable or format verb; a claim broken by
// an ESCAPED newline inside one literal, where the separator is the two source
// characters `\` and `n` and not whitespace at all; a count spelled in words
// instead of digits; a claim straddling two files; and a `//` comment sitting
// in a concatenation join -- see goConcatSplit for why that last one cannot be
// closed at the same time as comment wrapping.
//
// WHAT IT DOES SEE, at 11 arms (TestFlattenClaimTextCollapsesEverySplitShape):
// comment wrapping; adjacent-literal concatenation in all four quote pairings
// -- interpreted/interpreted, raw/raw, and both mixed -- and, for the
// interpreted pair only, both with and without whitespace around the `+`; a
// concatenation appearing inside a wrapped comment; a claim spanning the middle
// join of three literals; and a block comment in the join, on one line or
// spanning several.

// goConcatSplit matches the join between two adjacent Go string literals,
// including the degenerate `"a"+"b"` spelling with no surrounding space and the
// raw-literal and mixed spellings. Restricting this to double quotes was an
// over-claim caught by re-reading the sentence above against the pattern: a
// message assembled from raw literals is the same defect wearing a different
// quote character.
//
// It also crosses a BLOCK COMMENT sitting in the join: `"a" + /* why */ "b"` is
// legal Go and puts non-whitespace between the literals, which a
// whitespace-only pattern cannot span, so the claim stayed split and the sweep
// stayed green.
//
// A `//` comment in the same position is NOT crossed, and that is a real limit
// rather than an oversight -- the two requirements contradict each other under
// any purely textual pass. Comment WRAPPING is closed by turning `//` into a
// space and KEEPING the prose, which is what rejoins "544 call\n// sites";
// closing `"544 call " + // why\n "sites"` needs that prose DELETED to
// end-of-line instead. One flattener cannot do both, and real tokenization is
// the only thing that can. Pinned in the NOT-covered table.
var goConcatSplit = regexp.MustCompile("[\"`](?:\\s|/\\*[^*]*\\*+(?:[^/*][^*]*\\*+)*/)*\\+(?:\\s|/\\*[^*]*\\*+(?:[^/*][^*]*\\*+)*/)*[\"`]")

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
		{
			name: "block comment sitting in the join",
			src:  "panic(\"because 544 call \" + /* the count */ \"sites scan with it\")\n",
		},
		{
			name: "block comment spanning lines in the join",
			src:  "panic(\"because 544 call \" +\n\t\t/* why this\n\t\t   is here */ \"sites scan with it\")\n",
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
		{
			name: "line comment sitting in a concatenation join",
			src:  "panic(\"because 544 call \" + // the count\n\t\t\"sites scan with it\")\n",
			why: "closing this needs the comment's PROSE deleted to end-of-line, while closing comment " +
				"WRAPPING needs that same prose kept — one textual pass cannot do both, so this shape " +
				"is out of reach without real tokenization. The block-comment sibling IS covered, " +
				"because deleting it is compatible with both",
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
// withdrawn in DIVERGENCES.md.
//
// WHERE THIS GATE LOOKS, precisely, because "keeps them from drifting back in
// somewhere else" was a scope sentence considerably wider than the walk: Go
// files under pkg/, cmd/ and conformance/ excluding /parser/gen/, plus the
// canonical livingDocs set. NOT swept: shifts/, rfcs/, .claude/, root-level Go,
// and any other tree. A count restated in an RFC is out of reach, and closing
// that means widening the walk, not rewording this.
//
// The gap tolerance is 40 characters, not 20: "544 Scan/Rebuild/SetIndex call
// sites" needs 23 -- the regex has already consumed the three digits before the
// bounded gap begins, so counting them again gives 26 -- and the earlier bound
// let the most natural phrasing of the
// claim through. `[^.]` still stops the match at a sentence boundary so an
// unrelated number two sentences away cannot pair with a later "call sites".
var withdrawnIndexCallSiteCount = regexp.MustCompile(`\b54[45]\b[^.]{0,40}?call sites`)

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
	//
	// livingDocs, NOT a private list. An earlier revision hardcoded three files,
	// which left the count free to reappear in PRODUCTION_READINESS.md,
	// CHANGELOG.md, RELEASE.md, docs/mt-saas.md or road-to-prod.md while this
	// gate stayed green -- a scope sentence ("the living docs") broader than the
	// set it walked. Reusing the canonical variable makes the two the same thing
	// by construction, so a doc added to the project is swept without anyone
	// editing this file.
	for _, doc := range livingDocs {
		b, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			continue
		}
		flat := flattenClaimText(string(b))
		offenders := unwithdrawnCountClaims(flat)
		if len(offenders) > 0 {
			mdHits = append(mdHits, doc)
		}
		for _, o := range offenders {
			t.Errorf("%s states a withdrawn index-call-site count as a live claim:\n  …%s…", doc, o)
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

// unwithdrawnCountClaims returns an excerpt for every occurrence of a retired
// figure that is stated as a LIVE claim, i.e. every match not accompanied by a
// withdrawal.
//
// THE EXEMPTION IS SENTENCE-SCOPED, not window-scoped, and that is why this is
// a function rather than three lines inline. An earlier revision granted the
// exemption whenever "withdrawn" appeared within 1200 characters of the match,
// which reproduced the very masking the same commit was fixing one level up:
// switching from FindStringIndex to FindAllStringIndex made every match get
// CHECKED, and a live restatement added anywhere near the withdrawal paragraph
// still found "WITHDRAWN" in its own window and passed. Both halves had to move.
//
// A match is exempt only when withdrawal language sits in its own sentence or
// the one immediately before it -- the real shape of the legitimate mention,
// where a heading sentence carries "WITHDRAWN" and the mention follows. Anything
// further away is a different statement.
func unwithdrawnCountClaims(flat string) []string {
	var out []string
	for _, loc := range withdrawnIndexCallSiteCount.FindAllStringIndex(flat, -1) {
		if withdrawalNear(flat, loc[0], loc[1]) {
			continue
		}
		start := loc[0] - 120
		if start < 0 {
			start = 0
		}
		end := loc[1] + 120
		if end > len(flat) {
			end = len(flat)
		}
		out = append(out, flat[start:end])
	}
	return out
}

// withdrawalNear reports whether a withdrawal accompanies the match at
// [matchStart, matchEnd): the sentence containing it, plus the one before it.
func withdrawalNear(flat string, matchStart, matchEnd int) bool {
	// Back up over two sentence boundaries; the flattened text uses ". " as the
	// separator, and the claim regex already refuses to span one.
	from := matchStart
	for i := 0; i < 2; i++ {
		prev := strings.LastIndex(flat[:from], ". ")
		if prev < 0 {
			from = 0
			break
		}
		from = prev
	}
	to := len(flat)
	if next := strings.Index(flat[matchEnd:], ". "); next >= 0 {
		to = matchEnd + next
	}
	window := flat[from:to]
	return strings.Contains(window, "WITHDRAWN") || strings.Contains(window, "withdrawn")
}

// EVERY ARM OF THE EXEMPTION, driven from fixtures rather than from whatever the
// living docs happen to contain. That distinction is the whole reason this test
// exists: as of this commit NO living doc and NO swept Go file matches the claim
// regex at all, so the corpus sweep executes the exemption ZERO times. Without
// this table the sentence-scoping, the all-matches loop and the widened gap are
// all validated by an empty-population green -- in the file whose own header is
// written against exactly that failure.
func TestWithdrawalExemptionIsScopedToTheStatement(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		doc  string
		want int
	}{
		{
			name: "a live claim alone is flagged",
			doc:  "The index API is wide. 544 call sites pass a pre-Build index. That is the scope.",
			want: 1,
		},
		{
			name: "a withdrawal in the same sentence exempts it",
			doc:  "The figure of 544 call sites is WITHDRAWN as unscoped.",
			want: 0,
		},
		{
			name: "a withdrawal in the preceding sentence exempts it",
			doc:  "THE EXACT FIGURE IS WITHDRAWN. It read 544 call sites and counted something else.",
			want: 0,
		},
		{
			// THE TWO-HIT CASE, which a 1200-character window let through: the
			// withdrawal is real and nearby, and the second statement is a fresh
			// live claim that must still be caught.
			name: "a later live claim is not masked by an earlier withdrawal",
			doc: "THE EXACT FIGURE IS WITHDRAWN. It read 544 call sites and counted something else. " +
				"Some intervening prose about the index registry and its callers. " +
				"Anyway there are 545 call sites to convert.",
			want: 1,
		},
		{
			name: "two live claims are both reported",
			doc:  "There are 544 call sites here. And elsewhere 545 call sites remain.",
			want: 2,
		},
		{
			// The widened gap, pinned as its own arm. The regex has already
			// consumed the three digits when the bounded gap begins, so this
			// phrasing needs 23 characters of slack -- not the 26 an earlier
			// comment claimed, which counted the digits twice.
			name: "the natural phrasing with a 23-character gap is caught",
			doc:  "There are 544 Scan/Rebuild/SetIndex call sites to convert.",
			want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := unwithdrawnCountClaims(flattenClaimText(tc.doc))
			if len(got) != tc.want {
				t.Fatalf("unwithdrawnCountClaims returned %d offender(s), want %d.\n  doc: %q\n  got: %q",
					len(got), tc.want, tc.doc, got)
			}
		})
	}
}
