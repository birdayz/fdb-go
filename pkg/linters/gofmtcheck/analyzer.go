// Package gofmtcheck defines an analyzer that reports unformatted Go source files.
// Uses go/format directly — no shell out to gofmt.
package gofmtcheck

import (
	"bytes"
	"go/format"
	"os"

	"golang.org/x/tools/go/analysis"
)

var Analyzer = &analysis.Analyzer{
	Name: "gofmtcheck",
	Doc:  "reports Go source files not formatted with gofmt",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		pos := pass.Fset.Position(file.Pos())
		if pos.Filename == "" {
			continue
		}

		src, err := os.ReadFile(pos.Filename)
		if err != nil {
			continue
		}

		formatted, err := format.Source(src)
		if err != nil {
			continue
		}

		if !bytes.Equal(src, formatted) {
			pass.Report(analysis.Diagnostic{
				Pos:     file.Pos(),
				Message: "file is not gofmt'd",
			})
		}
	}
	return nil, nil
}
