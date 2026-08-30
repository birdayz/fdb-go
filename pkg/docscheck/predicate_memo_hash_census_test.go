package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// predicatesPkgDir is the package this census covers, relative to the repo root.
const predicatesPkgDir = "pkg/recordlayer/query/plan/cascades/predicates"

// minPredicateSourceFiles guards the census against the failure it is most
// likely to suffer: reading an empty directory and reporting zero hits as a
// clean bill of health. Under Bazel the sources reach this test only as a
// `data` filegroup, so a BUILD edit that drops them turns this into a test that
// passes over nothing. The package had far more than this when the floor was
// set; the number is a floor, not a count.
const minPredicateSourceFiles = 20

// TestQueryPredicatesDefineNoMemoHashMethod is the exhaustive form of a gate
// that started life inside the predicates package as a hand-written list of
// four types — while the package holds roughly nine QueryPredicate
// implementations. The claim ("no predicate type defines this") was true, and
// the coverage was under half, which is the same scope-exceeds-coverage shape
// the gate itself exists to prevent. Go cannot enumerate an interface's
// implementations at runtime, so the honest version asks the question of the
// SOURCE, where the answer is complete by construction.
//
// What it forbids: a method named HashCodeWithoutChildren on any type in the
// predicates package. That name is the memo hash one and two layers up —
// expressions.RelationalExpression and plans.RecordQueryPlan both declare it —
// so a predicate carrying it reads as "this is how predicates hash", and it is
// not. Two such methods existed, were dead, and were both wrong: one folded only
// its value and ignored its RANGES, so two sargables over different key ranges
// hashed identically; the other returned a constant, so every existential
// predicate hashed the same. A reader wiring either one in would have got a
// silent memo collapse.
//
// The predicates package has exactly two identity mechanisms — StructurallyEqual
// / StructuralHash and SemanticEqualsUnderAliasMap / SemanticHashCode — and a
// third, differently-named spelling is how the original Comparison identity bug
// came to be fixed in one layer and missed in another.
func TestQueryPredicatesDefineNoMemoHashMethod(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	dir := filepath.Join(root, predicatesPkgDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v. Under Bazel these sources are staged by the "+
			"predicates_sources filegroup in this target's `data`; if that was dropped, "+
			"this census reads an absent directory and would otherwise report a clean "+
			"result over nothing.", predicatesPkgDir, err)
	}

	fset := token.NewFileSet()
	scanned := 0
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		scanned++
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name == nil {
				continue
			}
			if fn.Name.Name != "HashCodeWithoutChildren" {
				continue
			}
			recv := "?"
			if len(fn.Recv.List) > 0 {
				recv = receiverTypeName(fn.Recv.List[0].Type)
			}
			offenders = append(offenders,
				name+":"+fset.Position(fn.Pos()).String()+" "+recv)
		}
	}

	// The vacuity guard, before the verdict: a zero-file scan would report no
	// offenders and read as a pass.
	if scanned < minPredicateSourceFiles {
		t.Fatalf("scanned %d .go files in %s, expected at least %d — the sources are not "+
			"reaching this test and its clean result means nothing",
			scanned, predicatesPkgDir, minPredicateSourceFiles)
	}
	t.Logf("census: %d source files scanned in %s", scanned, predicatesPkgDir)

	for _, o := range offenders {
		t.Errorf("%s defines HashCodeWithoutChildren. That name is the MEMO HASH at the "+
			"expression and plan layers; this package's identity mechanisms are "+
			"StructuralHash and SemanticHashCode. Fold the field set into one of those "+
			"two instead of adding a third spelling that a future reader will wire up.", o)
	}
}
