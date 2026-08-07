package docscheck

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The ratchet's SITE KEY, pinned on the two properties the whole scheme exists
// for. Both are unit-level over explicit state rather than corpus observations,
// because a corpus run only exercises the arms the corpus happens to reach and
// neither of these is a fact about the corpus.

// snippetKeys returns the site keys a snippet produces, in AST order, using the
// SAME keyer the real tally uses.
func snippetKeys(t *testing.T, rel, body string) []string {
	t.Helper()
	src := "package values\n\nimport \"strings\"\n\nvar _ = strings.Contains\n\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse snippet: %v\n---\n%s", err, src)
	}
	keyer := newFieldDecisionKeyer()
	var keys []string
	scanFieldDecisions(f, func(_ token.Pos, form, fn string) {
		keys = append(keys, keyer.key(rel, fn, form))
	})
	return keys
}

// TestFieldDecisionSiteKeySurvivesEditsAbove is the property the re-keying was
// done FOR, and it is the one the old `path:line` key failed.
//
// A line number is invalidated by any edit above the site — a new import, a
// comment, an unrelated function, and most absurdly the census that MEASURES the
// site. That cost eight mechanical re-keys in one session, one of them a rebase
// conflict across four files resolvable only by discarding one side and
// re-deriving every number from the gate's own output. Nothing about the check's
// semantics ever needed a line.
//
// The two snippets below differ ONLY above the decision. Under the old scheme
// their keys differed; under this one they must not.
func TestFieldDecisionSiteKeySurvivesEditsAbove(t *testing.T) {
	t.Parallel()
	const decision = `
func Decide(a, b *FieldValue) bool {
	return a.Field == b.Field
}
`
	before := snippetKeys(t, "pkg/x/f.go", decision)
	after := snippetKeys(t, "pkg/x/f.go", `
// An unrelated comment block that did not exist before.
//
// More of it.
func Unrelated(z *FieldValue) bool { return strings.Contains(z.Field, "x") }

var somethingElse = 1
`+decision)

	// Find the SURVIVING site's key on each side rather than comparing whole
	// lists: the inserted block deliberately contains a decision of its own,
	// because that is what a real edit above a tracked site looks like — the
	// census that measures these sites is itself instrumented code.
	pick := func(keys []string, what string) string {
		t.Helper()
		var hit string
		for _, k := range keys {
			if strings.Contains(k, " # Decide # ") {
				if hit != "" {
					t.Fatalf("%s: two keys name Decide (%s and %s)", what, hit, k)
				}
				hit = k
			}
		}
		if hit == "" {
			t.Fatalf("%s: no decision was reported inside Decide (got %v).\n"+
				"If the fixture stopped producing one this test proves nothing — "+
				"absence compares equal to absence.", what, keys)
		}
		return hit
	}
	b, a := pick(before, "before"), pick(after, "after")

	if b != a {
		t.Fatalf("the site key MOVED when unrelated code was inserted above it:\n"+
			"  before: %s\n  after:  %s\n"+
			"This is the entire defect the key scheme replaces. A key that shifts "+
			"when something above it changes forces a mechanical re-key on every "+
			"commit that touches the file — including the commits that instrument "+
			"the very sites being tracked, which is how this cost eight re-keys in "+
			"one session.", b, a)
	}
	if !strings.Contains(b, "Decide") {
		t.Fatalf("the key does not name its enclosing declaration: %s\n"+
			"The declaration is the stable half of the identity; without it the key "+
			"is only a form and cannot be unique.", b)
	}
}

// TestFieldDecisionSiteKeyMovesWhenTheSiteDoes is the other direction, and
// without it the test above is satisfied by a key that never changes at all —
// which would make the ratchet unable to notice a site that was moved or altered,
// i.e. a permanent allowlist.
func TestFieldDecisionSiteKeyMovesWhenTheSiteDoes(t *testing.T) {
	t.Parallel()
	base := snippetKeys(t, "pkg/x/f.go", `
func Decide(a, b *FieldValue) bool {
	return a.Field == b.Field
}
`)
	if len(base) != 1 {
		t.Fatalf("fixture produced %d decisions, want 1: %v", len(base), base)
	}

	for _, tc := range []struct {
		name string
		body string
		why  string
	}{
		{
			"moved to a different function",
			`
func Renamed(a, b *FieldValue) bool {
	return a.Field == b.Field
}
`,
			"a decision that moved to another declaration is a different site, and its " +
				"old entry must go stale rather than silently cover the new one",
		},
		{
			"the decision changed shape",
			`
func Decide(a, b *FieldValue) bool {
	return strings.Contains(a.Field, b.Field)
}
`,
			"a comparison replaced by a Contains call is a different decision with a " +
				"different fix",
		},
		{
			"moved to a different file",
			`
func Decide(a, b *FieldValue) bool {
	return a.Field == b.Field
}
`,
			"the file is part of the identity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := "pkg/x/f.go"
			if tc.name == "moved to a different file" {
				rel = "pkg/x/other.go"
			}
			got := snippetKeys(t, rel, tc.body)
			if len(got) != 1 {
				t.Fatalf("fixture produced %d decisions, want 1: %v", len(got), got)
			}
			if got[0] == base[0] {
				t.Fatalf("the site key did NOT move: %s\n%s.\nA key that never changes "+
					"turns the debt list into a permanent allowlist pointing at code that "+
					"has changed underneath it.", got[0], tc.why)
			}
		})
	}
}

// TestFieldDecisionSiteKeyOrdinalSeparatesTwins pins the disambiguator.
//
// Measured over the tracked tree, the (file, declaration, form) triple alone
// collapses 199 decisions onto 154 keys, and 15 of the 52 debt entries land on 5
// shared triples — the worst being a switch that returns six differently
// formatted names through one local. Without the ordinal those six become one
// entry, and fixing one silently covers the other five.
func TestFieldDecisionSiteKeyOrdinalSeparatesTwins(t *testing.T) {
	t.Parallel()
	keys := snippetKeys(t, "pkg/x/f.go", `
func Twins(a, b *FieldValue) string {
	if a.Field == b.Field {
		return "x"
	}
	if b.Field == a.Field {
		return "y"
	}
	return ""
}
`)
	if len(keys) != 2 {
		t.Fatalf("fixture produced %d decisions, want 2: %v", len(keys), keys)
	}
	if keys[0] == keys[1] {
		t.Fatalf("two decisions of the same form in one declaration share a key: %s\n"+
			"They are separate decisions with separate fixes; one entry covering both "+
			"lets a fix to one silently discharge the other.", keys[0])
	}
	if !strings.HasSuffix(keys[0], " # 1") || !strings.HasSuffix(keys[1], " # 2") {
		t.Fatalf("ordinals are not source-order 1,2: %v", keys)
	}
}

// TestFieldDecisionSiteKeysAreUnique is the guarantee the key doc claims, checked
// over the real tree rather than asserted.
//
// Uniqueness is what lets one entry mean one decision. If two decisions ever
// share a key, the debt list silently under-counts and a fix to one discharges
// the other — so a future collision must be a RED here rather than a quiet merge.
func TestFieldDecisionSiteKeysAreUnique(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	seen := map[string]int{}
	total := 0
	files := 0
	for _, rel := range trackedGoFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue
		}
		files++
		keyer := newFieldDecisionKeyer()
		scanFieldDecisions(f, func(_ token.Pos, form, fn string) {
			seen[keyer.key(rel, fn, form)]++
			total++
		})
	}
	if files == 0 {
		t.Fatal("scanned no files — the walk is broken, so a green result proves nothing")
	}
	if total == 0 {
		t.Fatal("found no decisions at all; uniqueness over an empty set is vacuous")
	}
	var dupes []string
	for k, n := range seen {
		if n > 1 {
			dupes = append(dupes, k)
		}
	}
	sort.Strings(dupes)
	if len(dupes) != 0 {
		t.Fatalf("%d site key(s) host more than one decision:\n  %s\n"+
			"One entry would then cover several decisions and a fix to one would "+
			"discharge the rest. The ordinal suffix exists to prevent this; if it is "+
			"no longer sufficient the key needs another discriminator, not a merged "+
			"entry.", len(dupes), strings.Join(dupes, "\n  "))
	}
	t.Logf("site keys unique over %d decisions in %d files", total, files)
}
