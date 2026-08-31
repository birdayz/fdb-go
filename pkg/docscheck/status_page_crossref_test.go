package docscheck

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The production-status page cross-references `TODO.md` constantly, and until
// this file existed it did so by LINE NUMBER. Every one of those anchors was
// stale: `TODO.md:10404` (cited for CQ-46) landed in the middle of a CQ-65
// paragraph, `TODO.md:10046` (CQ-30) in a memo-interning sentence, and
// `TODO.md:10590` (CQ-53) in an INFORMATION_SCHEMA escape-clause note.
//
// The failure is structural, not clerical. `TODO.md` is a ~14k-line living list
// that grows in the MIDDLE — the revision that booked the RFC-197 tail added 180
// lines across four items and shifted every anchor below them — so a line anchor
// into it is wrong the moment anyone edits an earlier item, and it is wrong
// SILENTLY, still rendering as a precise-looking citation.
//
// So the rot class is removed rather than guarded: the page cites the stable item
// ID (`CQ-53`), which is what a reader searches for anyway, and this test holds
// the page to that. The two halves below are the two ways the citation can lie —
// by pointing at a line (which will drift) or by naming an item that does not
// exist (a typo, or an item renamed out from under the page).

// todoLineAnchor matches a `TODO.md:1234` style citation on the status page.
var todoLineAnchor = regexp.MustCompile(`TODO\.md:\d+`)

// todoCompletedArchive holds the entries that were in TODO.md and are done. It is
// history, never a work list, but it is where a completed item's ID lives after the
// split — so the "findable" half of this gate reads it alongside the open list.
const todoCompletedArchive = "shifts/2026-08-31-todo-completed-archive.md"

// statusPageItemRef matches a CQ-item reference in the status page's prose.
var statusPageItemRef = regexp.MustCompile(`\bCQ-(\d+)\b`)

// todoItemHeading matches a TODO.md item heading, checked or unchecked:
// `- [ ] **CQ-46 (HIGH, STRUCTURAL) — ...` / `- [x] **CQ-53 (MED/L, ...`.
var todoItemHeading = regexp.MustCompile(`(?m)^- \[[ x]\] \*\*(?:~~)?CQ-(\d+)\b`)

// TestStatusPageCrossReferencesResolve holds the status page's `TODO.md`
// citations to a form that cannot silently rot, and then to the list itself.
func TestStatusPageCrossReferencesResolve(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	page := readDoc(t, root, productionStatusAuthority)
	todo := readDoc(t, root, "TODO.md")
	// The list is split: TODO.md carries open work, the archive carries the entries
	// that are done. FINDABILITY is checked against both halves, for the reason the
	// comment below gives — a completed item the page still names must stay findable,
	// and after the split that means findable in either half. Measured when the split
	// landed: CQ-47, CQ-80, CQ-81, CQ-83 and CQ-90 are cited by the page and live only
	// in the archive, so searching the open half alone reddens on all five. The FORMAT
	// check below stays on the open list, where the convention it pins is still in use.
	findable := todo + "\n" + readDoc(t, root, todoCompletedArchive)

	// (1) No line anchors. This is a FORM rule and it is deliberately absolute:
	// an anchor that happens to be correct today is still a citation whose
	// correctness nothing maintains, and the three that existed were all wrong.
	if stale := todoLineAnchor.FindAllString(page, -1); len(stale) > 0 {
		t.Errorf("%s cites TODO.md by LINE NUMBER (%s). TODO.md grows in the middle, so a line "+
			"anchor is wrong the moment an earlier item is edited — and wrong silently, still "+
			"reading as a precise citation. Cite the stable item ID (`CQ-53`) instead",
			productionStatusAuthority, strings.Join(dedupe(stale), ", "))
	}

	// (2) Every item the page names must be FINDABLE in TODO.md. Without this
	// the first half is a downgrade, not a fix: swapping a rotting-but-specific
	// anchor for an unverified name would trade a citation that goes stale for
	// one that was never checked at all.
	//
	// Findable, not "has a live heading". The page legitimately refers to items
	// that have been completed and folded away — `CQ-89` is named a dozen times
	// as the change a migration exists to repair, and its own heading is gone.
	// Demanding a heading would make the guard an argument for keeping dead
	// items forever, which is a worse document than the one it is protecting.
	// A typo or a rename still fails, which is the whole catch.
	if !todoItemHeading.MatchString(todo) {
		t.Fatal("no `- [ ] **CQ-N` item headings found in TODO.md — the item format has " +
			"changed, and the ID convention this guard rests on should be rechecked")
	}

	var missing []string
	seen := make(map[string]bool)
	for _, m := range statusPageItemRef.FindAllStringSubmatch(page, -1) {
		id := "CQ-" + m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if !regexp.MustCompile(`\b` + id + `\b`).MatchString(findable) {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		t.Errorf("%s cites %s, which appears NOWHERE in TODO.md or in %s. Either the item was "+
			"renamed and the page still quotes the old ID, or the reference is a typo — both are "+
			"a status page pointing an adopter at work that cannot be found",
			productionStatusAuthority, id, todoCompletedArchive)
	}
}

// dedupe collapses repeats so a failure names each distinct offender once.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
