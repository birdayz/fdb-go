// Package unicodepin guards the RFC-265 runtime closure and compatibility-critical callers.
package unicodepin

import (
	"go/ast"
	"go/build/constraint"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const pinnedRoot = "fdb.dev/internal/unicode15"

// Analyzer is the nogo entry point. Facts protect the pinned runtime closure;
// a narrower direct-use policy also protects its compatibility-critical callers.
var Analyzer = &analysis.Analyzer{
	Name:      "unicodepin",
	Doc:       "rejects unpinned Unicode data in the Unicode 15 closure and compatibility-critical callers (RFC-265)",
	Run:       run,
	FactTypes: []analysis.Fact{new(dependencyFact)},
}

type dependencyFact struct {
	Reason string
}

func (*dependencyFact) AFact() {}

type finding struct {
	pos    token.Pos
	reason string
}

func run(pass *analysis.Pass) (any, error) {
	if !localPackage(pass.Pkg.Path()) {
		return nil, nil
	}

	var findings []finding
	for _, file := range pass.Files {
		if strings.HasSuffix(pass.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}
		if compatibilityCaller(pass.Pkg.Path(), pass.Fset.Position(file.Pos()).Filename) {
			for _, f := range compatibilityFindings(file, pass.TypesInfo) {
				pass.Reportf(f.pos, "Unicode 15 compatibility caller: %s", f.reason)
			}
		}
		findings = append(findings, sourceFindings(file)...)
		findings = append(findings, helperFindings(file, pass.TypesInfo, forbiddenHelper)...)
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !localPackage(importPath) {
				continue
			}
			var fact dependencyFact
			found := false
			for _, pkg := range pass.Pkg.Imports() {
				if pkg.Path() == importPath {
					found = pass.ImportPackageFact(pkg, &fact)
					break
				}
			}
			if !found {
				findings = append(findings, finding{imp.Pos(), "runtime dependency " + importPath + " has no Unicode pin fact"})
			} else if fact.Reason != "" {
				findings = append(findings, finding{imp.Pos(), "runtime dependency " + importPath + " -> " + fact.Reason})
			}
		}
	}

	// Export clean facts too: a missing analysis must not silently admit a wrapper.
	reasons := make([]string, 0, len(findings))
	for _, f := range findings {
		reasons = append(reasons, f.reason)
		if pinnedPackage(pass.Pkg.Path()) {
			pass.Reportf(f.pos, "Unicode 15 pin: %s", f.reason)
		}
	}
	sort.Strings(reasons)
	fact := &dependencyFact{}
	if len(reasons) != 0 {
		fact.Reason = pass.Pkg.Path() + ": " + reasons[0]
	}
	pass.ExportPackageFact(fact)
	return nil, nil
}

func localPackage(pkg string) bool {
	return pkg == "fdb.dev" || strings.HasPrefix(pkg, "fdb.dev/")
}

func pinnedPackage(pkg string) bool {
	return pkg == pinnedRoot || strings.HasPrefix(pkg, pinnedRoot+"/")
}

func compatibilityCaller(pkg, filename string) bool {
	switch pkg + "/" + path.Base(strings.ReplaceAll(filename, "\\", "/")) {
	case "fdb.dev/pkg/recordlayer/text_tokenizer.go",
		"fdb.dev/pkg/recordlayer/text_index_maintainer.go",
		"fdb.dev/pkg/recordlayer/collate_function_key_expression.go",
		"fdb.dev/pkg/recordlayer/keystore_key_manager.go",
		"fdb.dev/pkg/recordlayer/java_parse.go",
		"fdb.dev/pkg/recordlayer/query/plan/cascades/values/values.go",
		"fdb.dev/pkg/recordlayer/query/plan/cascades/values/uuid_parse.go",
		"fdb.dev/pkg/relational/core/parser/case_insensitive.go":
		return true
	}
	return false
}

func compatibilityImport(pkg string) bool {
	if pkg == "unicode" {
		return true
	}
	for _, root := range []string{
		"golang.org/x/text/cases",
		"golang.org/x/text/unicode/norm",
		"golang.org/x/text/collate",
		"golang.org/x/text/language",
	} {
		if pkg == root || strings.HasPrefix(pkg, root+"/") {
			return true
		}
	}
	return false
}

func compatibilityHelper(pkg, name string) bool {
	if pkg != "strings" && pkg != "bytes" {
		return false
	}
	switch name {
	case "ToLower", "ToUpper", "ToTitle", "ToLowerSpecial", "ToUpperSpecial", "ToTitleSpecial", "Title", "EqualFold":
		return true
	}
	return false
}

func compatibilityFindings(file *ast.File, info *types.Info) []finding {
	findings := importFindings(file, compatibilityImport)
	return append(findings, helperFindings(file, info, compatibilityHelper)...)
}

func forbiddenImport(pkg string) bool {
	if pkg == "unicode" {
		return true
	}
	// The snapshot owns its complete third-party closure, including transform.
	// Standard-library import paths have no dot in their first component.
	first, _, _ := strings.Cut(pkg, "/")
	return !localPackage(pkg) && strings.Contains(first, ".")
}

func forbiddenHelper(pkg, name string) bool {
	if pkg != "strings" && pkg != "bytes" {
		return false
	}
	switch name {
	case "ToLower", "ToUpper", "ToTitle", "ToLowerSpecial", "ToUpperSpecial", "ToTitleSpecial",
		"Title", "EqualFold", "TrimSpace", "Fields", "FieldsSeq":
		return true
	}
	return false
}

func importFindings(file *ast.File, forbidden func(string) bool) []finding {
	var findings []finding
	for _, imp := range file.Imports {
		pkg, err := strconv.Unquote(imp.Path.Value)
		if err == nil && forbidden(pkg) {
			findings = append(findings, finding{imp.Pos(), "import " + pkg + " is outside the pinned Unicode runtime closure"})
		}
	}
	return findings
}

// sourceFindings also runs over every checked-in runtime source in the regression
// test: nogo receives only files selected by the current compiler and platform.
func sourceFindings(file *ast.File) []finding {
	findings := importFindings(file, forbiddenImport)
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) && !constraint.IsPlusBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			if err != nil {
				continue // buildtag reports malformed constraints
			}
			if compilerSelector(expr) {
				findings = append(findings, finding{comment.Pos(), "compiler-release build constraint selects runtime Unicode code"})
			}
		}
	}
	return findings
}

func compilerSelector(expr constraint.Expr) bool {
	switch expr := expr.(type) {
	case *constraint.TagExpr:
		if !strings.HasPrefix(expr.Tag, "go1.") {
			return false
		}
		_, err := strconv.ParseUint(strings.TrimPrefix(expr.Tag, "go1."), 10, 32)
		return err == nil
	case *constraint.NotExpr:
		return compilerSelector(expr.X)
	case *constraint.AndExpr:
		return compilerSelector(expr.X) || compilerSelector(expr.Y)
	case *constraint.OrExpr:
		return compilerSelector(expr.X) || compilerSelector(expr.Y)
	}
	return false
}

func helperFindings(file *ast.File, info *types.Info, forbidden func(string, string) bool) []finding {
	var findings []finding
	ast.Inspect(file, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		fn, ok := info.Uses[ident].(*types.Func)
		if ok && fn.Pkg() != nil && forbidden(fn.Pkg().Path(), fn.Name()) {
			findings = append(findings, finding{ident.Pos(), path.Base(fn.Pkg().Path()) + "." + fn.Name() + " uses compiler Unicode data"})
		}
		return true
	})
	return findings
}
