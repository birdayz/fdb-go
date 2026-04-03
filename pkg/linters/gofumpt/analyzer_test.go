package gofumpt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/tools/go/analysis"
	"mvdan.cc/gofumpt/format"
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
	if diags[0].Message != "file is not gofumpt'd" {
		t.Errorf("unexpected message: %s", diags[0].Message)
	}
}

func TestGofumptExtraRule(t *testing.T) {
	// Empty line after opening brace — valid gofmt, not gofumpt.
	diags := runOn(t, "package bad\n\nfunc Foo() {\n\n\tx := 1\n\t_ = x\n}\n")
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic (gofumpt extra rule), got %d", len(diags))
	}
}

func TestEmptyPackage(t *testing.T) {
	diags := runOn(t, "package empty\n")
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics, got %d", len(diags))
	}
}

func TestMultipleFiles(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "good.go"), []byte("package multi\n\nfunc Good() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package multi\n\nfunc Bad(){\n}\n"), 0o644)

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

func TestFlagsMatchOptions(t *testing.T) {
	// Every exported field in format.Options must have a corresponding flag.
	optType := reflect.TypeOf(format.Options{})
	fs := buildFlags()
	for i := range optType.NumField() {
		f := optType.Field(i)
		if !f.IsExported() {
			continue
		}
		name := toLower(f.Name)
		if fl := fs.Lookup(name); fl == nil {
			t.Errorf("format.Options.%s has no flag %q — buildFlags() needs updating", f.Name, name)
		}
	}
}

func toLower(s string) string {
	// Match buildFlags logic.
	b := make([]byte, len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func runOn(t *testing.T, src string) []analysis.Diagnostic {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
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
