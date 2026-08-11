package docscheck

import (
	"bytes"
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

// sourceRelativeBakedComment recognizes the LEGIBILITY COMMENT convention: a
// comment that spells out the arity requirement hidden inside the predicate's
// name. It is deliberately the same shape the arity sweep's own regexp matches
// (`len(...Accessors)`), because making the sweep see the site is the entire
// point of writing one.
var sourceRelativeBakedComment = regexp.MustCompile(`len\(.*Accessors\)`)

// sourceRelativeBakedCommentWindow is how many lines above a call site the
// legibility comment may sit. Wide enough for a multi-line explanation
// immediately above the condition, narrow enough that an unrelated comment
// elsewhere in the function cannot launder a site into visibility.
const sourceRelativeBakedCommentWindow = 12

// TestSourceRelativeBakedSitesAreVisibleToTheCensus closes the hole that let a
// LIVE arity defect sit inside the accessor-arity census's own subject matter
// while the census reported green.
//
// THE HOLE, stated as the measurement that found it. `SourceRelativeBaked()`
// requires `len(Accessors) == 1` (values.go), but that requirement lives inside
// the PREDICATE'S NAME. A site that gates on it therefore performs an arity
// decision while containing no `len(...Accessors)` expression, so the arity
// sweep — a regexp over source — cannot see it. `bakeUnnestElementRefOrdinal`
// was exactly that: zero arity expressions, absent from `accessorAritySites`,
// and a live defect that dropped every row of an EXISTS silently because the
// single-accessor requirement skipped struct-element MEMBER references. The
// census's `arityLiveDefect: 0` floor was green throughout.
//
// THE DIRECTION IS WHAT MAKES IT SERIOUS, and it is why this guard exists rather
// than a note: such a site is INVISIBLE WHILE BROKEN and becomes VISIBLE ONLY BY
// BEING FIXED, because the repair is what introduces the explicit arity
// expression. A guard whose blind spot is correlated with the bug it exists to
// find reports success most confidently exactly when it is failing.
//
// THE RULE. Every `SourceRelativeBaked()` call site must be visible to the
// census by one of two routes, and the routes are not interchangeable in what
// they promise:
//
//   - CLASSIFIED — its enclosing symbol is a key in `accessorAritySites`, which
//     means a human recorded a verdict AND a reason. This is the stronger route.
//   - LEGIBLE — a comment within the window above quotes `len(...Accessors)`, so
//     the arity sweep counts the line and the site is at least enumerated. This
//     is the weaker route: it states a fact, not a verdict, and a site resting on
//     it is owed a classification.
//
// A site with NEITHER is a HOLE IN THE CENSUS, not a style nit — the census's
// published population and its (d)=0 floor are both claims about a set this
// site is silently outside of.
//
// This is RFC-230 §7.5's "discard class" — sites that mishandle nesting without
// an arity test the sweep can see — made ENUMERABLE for the one predicate known
// to have a member. §7.5's broader point stands: other ways of hiding an arity
// decision exist and this guard does not cover them.
func TestSourceRelativeBakedSitesAreVisibleToTheCensus(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)

	type site struct {
		rel    string
		line   int
		symbol string
	}
	var classified, legible, holes []site
	scanned := 0

	for _, rel := range trackedGoFiles(t, root) {
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		scanned++
		if !bytes.Contains(src, []byte("SourceRelativeBaked")) {
			continue
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		if ast.IsGenerated(f) {
			continue
		}

		// Lines carrying a legibility comment, expanded to the whole comment
		// group's span so a multi-line explanation covers its own last line.
		commentLine := map[int]bool{}
		for _, group := range f.Comments {
			for _, c := range group.List {
				if !sourceRelativeBakedComment.MatchString(c.Text) {
					continue
				}
				lo := fset.Position(c.Pos()).Line
				hi := fset.Position(c.End()).Line
				for ln := lo; ln <= hi; ln++ {
					commentLine[ln] = true
				}
			}
		}

		symbols := funcSpansOf(fset, f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "SourceRelativeBaked" {
				return true
			}
			// The receiver is deliberately NOT inspected. Any call of this
			// selector is a use whatever it is called on, and the DECLARATION is
			// a FuncDecl rather than a CallExpr so it never reaches here — there
			// is nothing a receiver check could exclude that is not already
			// excluded. (An earlier revision rendered the receiver into a buffer
			// and never read it, which read like a filter that had been
			// forgotten rather than one deliberately not needed.)
			line := fset.Position(call.Pos()).Line
			symbol := enclosingSymbol(symbols, call.Pos())
			s := site{rel: rel, line: line, symbol: symbol}

			if _, ok := accessorAritySites[rel+"#"+symbol]; ok {
				classified = append(classified, s)
				return true
			}
			for ln := line; ln >= line-sourceRelativeBakedCommentWindow && ln > 0; ln-- {
				if commentLine[ln] {
					legible = append(legible, s)
					return true
				}
			}
			holes = append(holes, s)
			return true
		})
	}

	// ---- Anti-vacuity, in BOTH directions.
	//
	// The first guard is the ordinary one: an empty scan classifies nothing and
	// reports success. The second is the one this instrument specifically needs
	// — if the predicate is ever renamed, this walk finds zero call sites and
	// goes green while covering nothing, which is the same failure mode it was
	// built to catch, aimed at itself.
	if scanned < 100 {
		t.Fatalf("scanned only %d production Go files under %s — that is not the real "+
			"source tree. Fix sourceTreeRoot/runfiles staging; do NOT trust this run",
			scanned, root)
	}
	total := len(classified) + len(legible) + len(holes)
	if total == 0 {
		t.Fatalf("found ZERO SourceRelativeBaked() call sites across %d files. Either the "+
			"predicate was RENAMED — in which case this guard now covers nothing and must "+
			"be re-pointed at the new name — or the walk is broken. An empty population "+
			"cannot enumerate anything, and this guard exists precisely because a site "+
			"that is invisible reads identically to a site that is fine", scanned)
	}

	t.Logf("SourceRelativeBaked() visibility: %d call site(s) — %d classified in the arity "+
		"census, %d legible by comment only, %d unguarded",
		total, len(classified), len(legible), len(holes))

	if len(holes) > 0 {
		sort.Slice(holes, func(i, j int) bool {
			if holes[i].rel != holes[j].rel {
				return holes[i].rel < holes[j].rel
			}
			return holes[i].line < holes[j].line
		})
		var b strings.Builder
		for _, h := range holes {
			fmt.Fprintf(&b, "\n\t%s:%d (in %s)", h.rel, h.line, h.symbol)
		}
		t.Errorf("%d SourceRelativeBaked() call site(s) are invisible to the arity census:%s\n\n"+
			"THIS IS A HOLE IN THE CENSUS, NOT A STYLE NIT. SourceRelativeBaked() requires "+
			"len(Accessors) == 1, so each of these performs an ARITY DECISION while "+
			"containing no len(...Accessors) expression for the sweep to find. The census's "+
			"published population and its (d)=0 live-defect floor are claims about a set "+
			"these sites sit outside of.\n\n"+
			"THE ALARM DIRECTION IS GROWTH: a site appearing here means an arity decision "+
			"was added where nothing will count it. A site LEAVING this list is the fix "+
			"landing, never something to chase.\n\n"+
			"WHY IT MATTERS AT THIS PREDICATE SPECIFICALLY: bakeUnnestElementRefOrdinal sat "+
			"here with a LIVE defect — the single-accessor requirement skipped struct-element "+
			"MEMBER references, which then mis-resolved and made EXISTS drop every row "+
			"SILENTLY — while the census reported green. Such a site is invisible while "+
			"broken and becomes visible only by being FIXED.\n\n"+
			"TWO WAYS TO CLEAR ONE, and they promise different things:\n"+
			"\t(1) CLASSIFY it — add its `file#symbol` to accessorAritySites with a class "+
			"and a reason. This is the real answer; it records a verdict.\n"+
			"\t(2) MAKE IT LEGIBLE — put a comment above the condition quoting "+
			"`len(Accessors) == 1`, which the arity sweep then counts (it lands in "+
			"arityCommentLines, so update that constant too). This states a FACT, not a "+
			"verdict, and leaves the classification owed.\n"+
			"Do NOT silence this by deleting the site from the walk", len(holes), b.String())
	}
}
