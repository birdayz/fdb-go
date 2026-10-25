package recordlayerdeps

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// analyze runs the analyzer over one parsed file of package pkgPath. Imports are
// only read from the AST, so the file is not type-checked (analysistest shells to
// `go` and does not work in the Bazel sandbox).
func analyze(t *testing.T, pkgPath, filename, src string) []analysis.Diagnostic {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var diags []analysis.Diagnostic
	pass := &analysis.Pass{
		Fset:   fset,
		Files:  []*ast.File{file},
		Pkg:    types.NewPackage(pkgPath, file.Name.Name),
		Report: func(d analysis.Diagnostic) { diags = append(diags, d) },
	}
	if _, err := run(pass); err != nil {
		t.Fatalf("run: %v", err)
	}
	return diags
}

const importsRelationalAPI = `package executor

import (
	"context"

	"fdb.dev/pkg/relational/api"
)
`

func TestRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, pkgPath, filename, src string
		want                         int
	}{
		{"record layer importing relational", "fdb.dev/pkg/recordlayer/query/executor", "pkg/recordlayer/query/executor/executor.go", importsRelationalAPI, 1},
		{"record layer root", "fdb.dev/pkg/recordlayer", "pkg/recordlayer/store.go", importsRelationalAPI, 1},
		{"test file exempt", "fdb.dev/pkg/recordlayer/query/executor", "pkg/recordlayer/query/executor/executor_test.go", importsRelationalAPI, 0},
		{"relational importing relational", "fdb.dev/pkg/relational/core/embedded", "pkg/relational/core/embedded/x.go", importsRelationalAPI, 0},
		{"sibling prefix is not the record layer", "fdb.dev/pkg/recordlayerx", "pkg/recordlayerx/x.go", importsRelationalAPI, 0},
		{"no relational import", "fdb.dev/pkg/recordlayer/query/executor", "pkg/recordlayer/query/executor/x.go", "package executor\n\nimport \"fdb.dev/pkg/relationalx/api\"\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := len(analyze(t, tc.pkgPath, tc.filename, tc.src)); got != tc.want {
				t.Fatalf("diagnostics = %d, want %d", got, tc.want)
			}
		})
	}
}
