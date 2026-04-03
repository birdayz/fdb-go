package gofmtcheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis"
)

func TestFormatted(t *testing.T) {
	diags := runOn(t, "package formatted\n\nfunc Foo() {\n\tx := 1\n\t_ = x\n}\n")
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics, got %d: %v", len(diags), diags)
	}
}

func TestUnformattedSpacing(t *testing.T) {
	diags := runOn(t, "package bad\n\nfunc Foo(){\n x:=1\n _=x\n}\n")
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	if diags[0].Message != "file is not gofmt'd" {
		t.Errorf("unexpected message: %s", diags[0].Message)
	}
}

func TestUnformattedTabs(t *testing.T) {
	// Spaces instead of tabs.
	diags := runOn(t, "package bad\n\nfunc Foo() {\n    x := 1\n    _ = x\n}\n")
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
}

func TestEmptyPackage(t *testing.T) {
	diags := runOn(t, "package empty\n")
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for empty package, got %d", len(diags))
	}
}

func TestMultipleFiles(t *testing.T) {
	dir := t.TempDir()

	// One good, one bad.
	os.WriteFile(filepath.Join(dir, "good.go"), []byte("package multi\n\nfunc Good() {}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package multi\n\nfunc Bad(){\n}\n"), 0644)

	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range []string{"good.go", "bad.go"} {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}

	var diags []analysis.Diagnostic
	pass := &analysis.Pass{
		Analyzer: Analyzer,
		Fset:     fset,
		Files:    files,
		Report: func(d analysis.Diagnostic) {
			diags = append(diags, d)
		},
	}

	if _, err := Analyzer.Run(pass); err != nil {
		t.Fatal(err)
	}

	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic (bad.go only), got %d", len(diags))
	}
}

// runOn writes src to a temp file, parses it, and runs the analyzer.
func runOn(t *testing.T, src string) []analysis.Diagnostic {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.go")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	var diags []analysis.Diagnostic
	pass := &analysis.Pass{
		Analyzer: Analyzer,
		Fset:     fset,
		Files:    []*ast.File{f},
		Report: func(d analysis.Diagnostic) {
			diags = append(diags, d)
		},
	}

	if _, err := Analyzer.Run(pass); err != nil {
		t.Fatal(err)
	}
	return diags
}
