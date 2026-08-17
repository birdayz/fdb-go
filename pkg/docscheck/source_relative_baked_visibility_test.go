package docscheck

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSourceRelativeBakedSitesAreVisibleToTheCensus is the post-RFC-232 zero
// ratchet for the retired compatibility predicate. Before exact field views,
// SourceRelativeBaked hid a one-accessor decision behind a method name and this
// gate required every call to be classified or made legible. RFC-232 removed
// every production caller. Reintroducing one is now the regression; an empty
// call-site population is the intended, explicitly asserted state.
func TestSourceRelativeBakedSitesAreVisibleToTheCensus(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	scanned := 0
	var sites []string
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
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		if ast.IsGenerated(f) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel != nil && sel.Sel.Name == "SourceRelativeBaked" {
				sites = append(sites, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d production Go files under %s", scanned, root)
	}
	if len(sites) != 0 {
		sort.Strings(sites)
		t.Fatalf("RFC-232 retired SourceRelativeBaked from production, but found %d call(s):\n  %s\n"+
			"Use the exact FieldValue path/frontier view required by the caller instead of "+
			"reviving the hidden single-accessor compatibility predicate.",
			len(sites), strings.Join(sites, "\n  "))
	}
}
