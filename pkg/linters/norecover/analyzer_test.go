package norecover

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// analyze type-checks src under the given filename and runs the analyzer over it,
// returning the diagnostics. In-process (no toolchain shelling, no testdata) so it
// runs identically under `go test` and Bazel — analysistest shells to `go` and does
// not work in the Bazel sandbox. src must import nothing (builtins only) so the
// type-checker needs no importer.
func analyze(t *testing.T, allow map[string]int, filename, src string) []analysis.Diagnostic {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{}).Check("p", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("typecheck: %v", err)
	}
	var diags []analysis.Diagnostic
	pass := &analysis.Pass{
		Fset:      fset,
		Files:     []*ast.File{file},
		TypesInfo: info,
		Report:    func(d analysis.Diagnostic) { diags = append(diags, d) },
	}
	if _, err := (&runner{allow: allow}).run(pass); err != nil {
		t.Fatalf("run: %v", err)
	}
	return diags
}

const oneRecover = `package p
func f() (err error) {
	defer func() { if recover() != nil { err = nil } }()
	panic("x")
}
`

// A recover() in a file not on the allowlist is flagged.
func TestRun_NonAllowlistedFlagged(t *testing.T) {
	t.Parallel()
	diags := analyze(t, Allowlist, "pkg/foo/random.go", oneRecover)
	if len(diags) != 1 {
		t.Fatalf("non-allowlisted recover: want 1 diagnostic, got %d", len(diags))
	}
}

// A recover() in an allowlisted boundary file is exempt.
func TestRun_AllowlistedExempt(t *testing.T) {
	t.Parallel()
	diags := analyze(t, Allowlist, "pkg/fdbgo/fdb/panic.go", oneRecover)
	if len(diags) != 0 {
		t.Fatalf("allowlisted recover: want 0 diagnostics, got %d", len(diags))
	}
}

// Per-file count: with N permitted, the first N are exempt and the (N+1)th is flagged.
func TestRun_PerFileCount(t *testing.T) {
	t.Parallel()
	src := `package p
func a() { defer func() { _ = recover() }(); panic(1) }
func b() { defer func() { _ = recover() }(); panic(2) }
func c() { defer func() { _ = recover() }(); panic(3) }
`
	diags := analyze(t, map[string]int{"x/y.go": 2}, "x/y.go", src)
	if len(diags) != 1 {
		t.Fatalf("3 recovers with allowance 2: want 1 diagnostic, got %d", len(diags))
	}
}

// recover() inside a _test.go file is exempt (tests recover freely).
func TestRun_TestFileExempt(t *testing.T) {
	t.Parallel()
	diags := analyze(t, Allowlist, "pkg/foo/bar_test.go", oneRecover)
	if len(diags) != 0 {
		t.Fatalf("_test.go recover: want 0 diagnostics, got %d", len(diags))
	}
}

// A local identifier named "recover" that is not the builtin must not be flagged.
func TestRun_ShadowedRecoverIgnored(t *testing.T) {
	t.Parallel()
	src := `package p
func f() {
	recover := func() any { return nil }
	_ = recover()
}
`
	diags := analyze(t, Allowlist, "pkg/foo/random.go", src)
	if len(diags) != 0 {
		t.Fatalf("shadowed non-builtin recover: want 0 diagnostics, got %d", len(diags))
	}
}
