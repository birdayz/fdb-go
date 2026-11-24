package dockerskip

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// Stubs for testing and the testcontainers package: callees resolve by package
// path, and analysistest's toolchain subprocess fails in the Bazel sandbox.
var stubs = map[string]string{
	"testing": `package testing
type common struct{}
func (c *common) Skip(args ...any) {}
func (c *common) Skipf(format string, args ...any) {}
func (c *common) SkipNow() {}
func (c *common) Fatal(args ...any) {}
type T struct{ common }
type M struct{}
func (m *M) Run() int { return 0 }
`,
	TestcontainersPkg: `package foundationdb
type Container struct{}
func (c *Container) Run() (int, error) { return 0, nil }
type Cluster struct{}
func Run(ctx any, img string) (*Container, error) { return nil, nil }
func RunCluster(ctx any, size int) (*Cluster, error) { return nil, nil }
func SkipWithoutDocker(err error) bool { return false }
`,
	"other/fixture": `package fixture
func Run(ctx any, img string) (any, error) { return nil, nil }
`,
}

type stubImporter struct {
	fset *token.FileSet
	pkgs map[string]*types.Package
}

func (im *stubImporter) Import(path string) (*types.Package, error) {
	if p, ok := im.pkgs[path]; ok {
		return p, nil
	}
	src, ok := stubs[path]
	if !ok {
		return importer.Default().Import(path)
	}
	f, err := parser.ParseFile(im.fset, path+".go", src, 0)
	if err != nil {
		return nil, err
	}
	p, err := (&types.Config{Importer: im}).Check(path, im.fset, []*ast.File{f}, nil)
	if err != nil {
		return nil, err
	}
	im.pkgs[path] = p
	return p, nil
}

func analyze(t *testing.T, body string) []analysis.Diagnostic {
	t.Helper()
	src := fmt.Sprintf(`package p

import (
	"testing"

	fdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	"other/fixture"
)

var (
	_ = fixture.Run
	_ = fdbtc.Run
	_ testing.T
)

%s
`, body)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p_test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := &types.Config{Importer: &stubImporter{fset: fset, pkgs: map[string]*types.Package{}}}
	if _, err := conf.Check("p", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("typecheck: %v", err)
	}
	var diags []analysis.Diagnostic
	pass := &analysis.Pass{
		Fset:      fset,
		Files:     []*ast.File{file},
		TypesInfo: info,
		Report:    func(d analysis.Diagnostic) { diags = append(diags, d) },
	}
	if _, err := run(pass); err != nil {
		t.Fatalf("run: %v", err)
	}
	return diags
}

func TestAnalyzer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		flagged    bool
	}{
		{"TestMain skips on any error", `
func run(m *testing.M) int {
	_, err := fdbtc.Run(nil, "")
	if err != nil {
		return m.Run()
	}
	return 0
}`, true},
		{"TestMain asks SkipWithoutDocker", `
func run(m *testing.M) int {
	_, err := fdbtc.Run(nil, "")
	if err != nil {
		if fdbtc.SkipWithoutDocker(err) {
			return m.Run()
		}
		return 1
	}
	return 0
}`, false},
		{"t.Skipf on any error", `
func f(t *testing.T) {
	_, err := fdbtc.Run(nil, "")
	if err != nil {
		t.Skipf("no Docker: %v", err)
	}
}`, true},
		{"t.Skip on any error", `
func f(t *testing.T) {
	c, err := fdbtc.Run(nil, "")
	if err != nil {
		t.Skip("no Docker")
	}
	_ = c
}`, true},
		{"t.SkipNow behind a nil check written backwards", `
func f(t *testing.T) {
	_, err := fdbtc.Run(nil, "")
	if nil != err {
		t.SkipNow()
	}
}`, true},
		{"return 0 on any error", `
func run() int {
	_, err := fdbtc.Run(nil, "")
	if err != nil {
		return 0
	}
	return 1
}`, true},
		{"return 0 behind SkipWithoutDocker", `
func run() int {
	_, err := fdbtc.Run(nil, "")
	if err != nil {
		if !fdbtc.SkipWithoutDocker(err) {
			return 1
		}
		return 0
	}
	return 1
}`, false},
		{"fails on any error", `
func f(t *testing.T) {
	_, err := fdbtc.Run(nil, "")
	if err != nil {
		t.Fatal(err)
	}
}`, false},
		{"if with init", `
func f(t *testing.T) {
	if _, err := fdbtc.Run(nil, ""); err != nil {
		t.Skip("no Docker")
	}
}`, true},
		{"RunCluster", `
func f(t *testing.T) {
	_, err := fdbtc.RunCluster(nil, 3)
	if err != nil {
		t.Skip("no Docker")
	}
}`, true},
		{"assignment to an existing variable", `
func f(t *testing.T) {
	var err error
	_, err = fdbtc.Run(nil, "")
	if err != nil {
		t.Skip("no Docker")
	}
}`, true},
		{"inside a switch case", `
func f(t *testing.T, k int) {
	switch k {
	case 1:
		_, err := fdbtc.Run(nil, "")
		if err != nil {
			t.Skip("no Docker")
		}
	}
}`, true},
		{"inside a select case", `
func f(t *testing.T, ch chan int) {
	select {
	case <-ch:
		_, err := fdbtc.Run(nil, "")
		if err != nil {
			t.Skip("no Docker")
		}
	}
}`, true},
		{"a method named Run", `
func f(t *testing.T, c *fdbtc.Container) {
	_, err := c.Run()
	if err != nil {
		t.Skip("no Docker")
	}
}`, false},
		{"a call through a func value", `
func f(t *testing.T, fns []func() (int, error)) {
	_, err := fns[0]()
	if err != nil {
		t.Skip("x")
	}
}`, false},
		{"another package's Run", `
func f(t *testing.T) {
	_, err := fixture.Run(nil, "")
	if err != nil {
		t.Skip("no Docker")
	}
}`, false},
		{"a different error checked", `
func f(t *testing.T, other error) {
	_, err := fdbtc.Run(nil, "")
	_ = err
	if other != nil {
		t.Skip("other")
	}
}`, false},
		{"skip on the success branch", `
func f(t *testing.T) {
	_, err := fdbtc.Run(nil, "")
	if err == nil {
		t.Skip("nothing to do")
	}
}`, false},
		{"error discarded", `
func f(t *testing.T) {
	c, _ := fdbtc.Run(nil, "")
	if c != nil {
		t.Skip("x")
	}
}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			diags := analyze(t, tc.body)
			if got := len(diags) > 0; got != tc.flagged {
				t.Fatalf("flagged = %t (%v), want %t", got, diags, tc.flagged)
			}
		})
	}
}
