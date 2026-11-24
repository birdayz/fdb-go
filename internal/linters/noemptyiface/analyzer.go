// Package noemptyiface defines an analyzer that rejects interface{} in favor of any.
package noemptyiface

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"
)

var Analyzer = &analysis.Analyzer{
	Name: "noemptyiface",
	Doc:  "reports uses of interface{} that should be any",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			iface, ok := n.(*ast.InterfaceType)
			if !ok {
				return true
			}
			// Empty interface: no methods.
			if iface.Methods != nil && iface.Methods.NumFields() > 0 {
				return true
			}
			// Skip if this IS the `type any = interface{}` declaration
			// in the universe scope (builtin). We only see user code here
			// so this check is for generated code that redeclares it.
			pos := pass.Fset.Position(iface.Pos())
			if pos.Filename == "" {
				return true
			}
			pass.Report(analysis.Diagnostic{
				Pos:     iface.Pos(),
				End:     iface.End(),
				Message: "use any instead of interface{}",
				SuggestedFixes: []analysis.SuggestedFix{{
					Message: "replace with any",
					TextEdits: []analysis.TextEdit{{
						Pos:     iface.Pos(),
						End:     iface.End(),
						NewText: []byte("any"),
					}},
				}},
			})
			return true
		})
	}
	return nil, nil
}
