// Package recordlayerdeps is a nogo analyzer that keeps the record layer
// independent of the SQL layer, as Java's fdb-record-layer-core is of
// fdb-relational: a non-test file under pkg/recordlayer must not import a
// package under pkg/relational. Test files are exempt.
package recordlayerdeps

import (
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const (
	recordLayerPath = "fdb.dev/pkg/recordlayer"
	relationalPath  = "fdb.dev/pkg/relational"
)

// Analyzer is the nogo entry point.
var Analyzer = &analysis.Analyzer{
	Name: "recordlayerdeps",
	Doc:  "flags a pkg/recordlayer import of pkg/relational: the record layer does not depend on the SQL layer",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	if !within(pass.Pkg.Path(), recordLayerPath) {
		return nil, nil
	}
	for _, file := range pass.Files {
		if strings.HasSuffix(filepath.ToSlash(pass.Fset.Position(file.Package).Filename), "_test.go") {
			continue
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil || !within(path, relationalPath) {
				continue
			}
			pass.Report(analysis.Diagnostic{
				Pos: spec.Pos(),
				End: spec.End(),
				Message: "pkg/recordlayer must not import " + path + ": return a record-layer error " +
					"or type and translate it in pkg/relational",
			})
		}
	}
	return nil, nil
}

// within reports whether path is root or a package below it.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}
