// Package constfoldcensus enumerates every call site that simplifies or
// evaluates a constant at planning (RFC-257 WS-E design 5.4(b)): the value
// and predicate simplification entry points, the constant evaluators, the
// predicate simplifier driver, and every `Evaluate(nil)` / `Eval(nil)` call.
// Its test assigns each site a reason and fails on a site nobody assigned.
package constfoldcensus

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strings"
)

// Site is one census key: the file (slash path from the module root), the
// enclosing top-level function (Recv.Name for a method), and the callee.
type Site struct {
	File, Func, Callee string
}

func (s Site) String() string { return s.File + " " + s.Func + " " + s.Callee }

// entryPoints maps each entry point to the package directory suffix that
// defines it (under pkg/, internal/, or top-level): an unqualified call counts
// only inside that directory, and a qualified one only through that package's
// name.
var entryPoints = map[string]struct{ pkgName, suffix string }{
	"SimplifyValue":               {"values", "recordlayer/query/plan/cascades/values"},
	"SimplifyPredicateValue":      {"values", "recordlayer/query/plan/cascades/values"},
	"EvaluateConstantComparand":   {"values", "recordlayer/query/plan/cascades/values"},
	"EvaluateConstant":            {"values", "recordlayer/query/plan/cascades/values"},
	"SimplifyPredicateValues":     {"predicates", "recordlayer/query/plan/cascades/predicates"},
	"EvaluatePredicateComparands": {"predicates", "recordlayer/query/plan/cascades/predicates"},
	"Simplify":                    {"cascades", "recordlayer/query/plan/cascades"},
}

func inDefiningPkg(dir, suffix string) bool {
	return dir == "pkg/"+suffix || dir == "internal/"+suffix || dir == suffix
}

// ScanSource returns the census sites of one non-test Go file. file is its
// slash path from the module root.
func ScanSource(file string, src []byte) ([]Site, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	dir := path.Dir(file)
	var sites []Site
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) == 1 {
			name = receiverName(fn.Recv.List[0].Type) + "." + name
		}
		add := func(callee string) { sites = append(sites, Site{File: file, Func: name, Callee: callee}) }
		var visit func(n ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				// `x.Evaluate(nil)` / `x.Eval(nil)`: an evaluation with no row.
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok &&
					(sel.Sel.Name == "Evaluate" || sel.Sel.Name == "Eval") &&
					len(n.Args) >= 1 && isNilIdent(n.Args[0]) {
					add(sel.Sel.Name + "(nil)")
				}
			case *ast.SelectorExpr:
				// A qualified reference, called or passed as a function value.
				if x, ok := n.X.(*ast.Ident); ok {
					if ep, ok := entryPoints[n.Sel.Name]; ok && x.Name == ep.pkgName {
						add(ep.pkgName + "." + n.Sel.Name)
					}
				}
				// The selector's name is not an unqualified reference.
				ast.Inspect(n.X, visit)
				return false
			case *ast.Ident:
				// An unqualified reference inside the defining package.
				if ep, ok := entryPoints[n.Name]; ok && inDefiningPkg(dir, ep.suffix) {
					add(ep.pkgName + "." + n.Name)
				}
			}
			return true
		}
		ast.Inspect(fn.Body, visit)
	}
	return sites, nil
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

// Count tallies sites by key.
func Count(sites []Site) map[Site]int {
	out := map[Site]int{}
	for _, s := range sites {
		out[s]++
	}
	return out
}

// Sorted returns the keys of counts in a stable order.
func Sorted(counts map[Site]int) []Site {
	keys := make([]Site, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

var populationRoots = []string{
	"pkg/",
	"internal/",
	"recordlayer/",
	"relational/",
}

func inPopulationRoot(file string) bool {
	for _, root := range populationRoots {
		if strings.HasPrefix(file, root) {
			return true
		}
	}
	return false
}

// InPopulation reports whether a module-relative slash path is a census
// file: a non-test, non-generated Go file under pkg/, internal/, recordlayer/,
// or relational/.
func InPopulation(file string) bool {
	return inPopulationRoot(file) && strings.HasSuffix(file, ".go") &&
		!strings.HasSuffix(file, "_test.go") && !strings.HasSuffix(file, ".pb.go") &&
		!strings.Contains(file, "/testdata/")
}
