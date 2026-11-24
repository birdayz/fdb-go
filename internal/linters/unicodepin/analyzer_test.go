package unicodepin

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// Stub exports avoid analysistest's toolchain subprocess inside Bazel. Only
// object identity matters here; the analyzer never inspects function signatures.
type stubImporter struct{}

func (stubImporter) Import(importPath string) (*types.Package, error) {
	pkg := types.NewPackage(importPath, path.Base(importPath))
	for _, name := range []string{
		"ToLower", "ToUpper", "ToTitle", "ToLowerSpecial", "ToUpperSpecial", "ToTitleSpecial",
		"Title", "EqualFold", "TrimSpace", "Fields", "FieldsSeq", "FieldsFunc", "TrimFunc",
		"Compare", "Map", "ToValidUTF8", "DecodeRuneInString", "Encode",
	} {
		pkg.Scope().Insert(types.NewFunc(token.NoPos, pkg, name, types.NewSignatureType(nil, nil, nil, nil, nil, false)))
	}
	pkg.MarkComplete()
	return pkg, nil
}

func analyze(t *testing.T, pkgPath, filename, src string, facts map[string]dependencyFact) ([]analysis.Diagnostic, *dependencyFact) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Exercise future release constraints without requiring that future compiler.
	// The analyzer reads the unchanged comments, not this type-checker metadata.
	file.GoVersion = ""
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	pkg, err := (&types.Config{Importer: stubImporter{}}).Check(pkgPath, fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatalf("typecheck: %v", err)
	}
	var diagnostics []analysis.Diagnostic
	var exported *dependencyFact
	pass := &analysis.Pass{
		Analyzer:  Analyzer,
		Fset:      fset,
		Files:     []*ast.File{file},
		Pkg:       pkg,
		TypesInfo: info,
		Report:    func(d analysis.Diagnostic) { diagnostics = append(diagnostics, d) },
		ImportPackageFact: func(pkg *types.Package, out analysis.Fact) bool {
			fact, ok := facts[pkg.Path()]
			if ok {
				*out.(*dependencyFact) = fact
			}
			return ok
		},
		ExportPackageFact: func(fact analysis.Fact) {
			copy := *fact.(*dependencyFact)
			exported = &copy
		},
	}
	if _, err := Analyzer.Run(pass); err != nil {
		t.Fatalf("run: %v", err)
	}
	if localPackage(pkgPath) && exported == nil {
		t.Fatal("analyzer did not export a dependency fact")
	}
	return diagnostics, exported
}

func requireDiagnostics(t *testing.T, diagnostics []analysis.Diagnostic, want int, fragment string) {
	t.Helper()
	if len(diagnostics) != want {
		t.Fatalf("diagnostics = %v, want %d containing %q", diagnostics, want, fragment)
	}
	for _, d := range diagnostics {
		if !d.Pos.IsValid() || !strings.Contains(d.Message, fragment) {
			t.Errorf("diagnostic = %+v, want valid position and %q", d, fragment)
		}
	}
}

func TestAnalyzerRegistration(t *testing.T) {
	t.Parallel()
	if err := analysis.Validate([]*analysis.Analyzer{Analyzer}); err != nil {
		t.Fatal(err)
	}
	if len(Analyzer.FactTypes) != 1 {
		t.Fatalf("fact types = %d, want the dependency-closure fact", len(Analyzer.FactTypes))
	}
}

func TestForbiddenImports(t *testing.T) {
	t.Parallel()
	for _, pkg := range []string{
		"unicode",
		"golang.org/x/text/cases",
		"golang.org/x/text/unicode/norm",
		"golang.org/x/text/collate",
		"golang.org/x/text/collate/build",
		"golang.org/x/text/language",
		"golang.org/x/text/language/display",
		"golang.org/x/text/internal",
		"golang.org/x/text/internal/colltab",
		"golang.org/x/text/internal/language",
		"golang.org/x/text/internal/language/compact",
		"golang.org/x/text/internal/tag",
		"golang.org/x/text/transform",
		"example.org/wrapper",
	} {
		t.Run(pkg, func(t *testing.T) {
			t.Parallel()
			diagnostics, fact := analyze(t, pinnedRoot, "runtime.go", fmt.Sprintf("package p\nimport _ %q\n", pkg), nil)
			requireDiagnostics(t, diagnostics, 1, "import "+pkg)
			if !strings.Contains(fact.Reason, pkg) {
				t.Fatalf("fact = %+v, want forbidden import", fact)
			}
		})
	}
}

func TestAllowedImports(t *testing.T) {
	t.Parallel()
	for _, pkg := range []string{
		"unicode/utf8", "unicode/utf16", "strings", "bytes", "encoding/binary", "fmt",
		pinnedRoot + "/text/unicode/norm", pinnedRoot + "/text/language", pinnedRoot + "/text/internal/colltab",
		pinnedRoot + "/text/transform",
	} {
		t.Run(pkg, func(t *testing.T) {
			t.Parallel()
			diagnostics, fact := analyze(t, pinnedRoot, "runtime.go", fmt.Sprintf("package p\nimport _ %q\n", pkg), map[string]dependencyFact{pkg: {}})
			requireDiagnostics(t, diagnostics, 0, "")
			if fact.Reason != "" {
				t.Fatalf("clean import exported tainted fact: %+v", fact)
			}
		})
	}
}

func TestForbiddenHelperValues(t *testing.T) {
	t.Parallel()
	for _, pkg := range []string{"strings", "bytes"} {
		for _, helper := range []string{
			"ToLower", "ToUpper", "ToTitle", "ToLowerSpecial", "ToUpperSpecial", "ToTitleSpecial",
			"Title", "EqualFold", "TrimSpace", "Fields", "FieldsSeq",
		} {
			t.Run(pkg+"."+helper, func(t *testing.T) {
				t.Parallel()
				src := fmt.Sprintf("package p\nimport alias %q\nvar _ = alias.%s\n", pkg, helper)
				diagnostics, fact := analyze(t, pinnedRoot+"/text/language", "runtime.go", src, nil)
				requireDiagnostics(t, diagnostics, 1, pkg+"."+helper)
				if !strings.Contains(fact.Reason, pkg+"."+helper) {
					t.Fatalf("fact = %+v, want helper value", fact)
				}
			})
		}
	}
}

func TestHelperResolution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"call", `package p; import "strings"; func f() { strings.ToLower() }`, 1},
		{"dot import value", `package p; import . "strings"; var _ = ToLower`, 1},
		{"dot import call", `package p; import . "bytes"; func f() { EqualFold() }`, 1},
		{"shadowed import", `package p; import "strings"; var _ = strings.Compare; func f() { strings := struct{ToLower func()}{func(){}}; strings.ToLower() }`, 0},
		{"local function", `package p; func ToLower() {}; var _ = ToLower`, 0},
		{"local method", `package p; type S struct{}; func (S) ToUpper() {}; var _ = S{}.ToUpper`, 0},
		{"encoding and supplied predicates", `package p; import "strings"; var _, _, _, _, _ = strings.FieldsFunc, strings.TrimFunc, strings.Compare, strings.Map, strings.ToValidUTF8`, 0},
		{"bytes supplied predicates", `package p; import "bytes"; var _, _, _ = bytes.FieldsFunc, bytes.TrimFunc, bytes.Compare`, 0},
		{"utf8 mechanic", `package p; import "unicode/utf8"; var _ = utf8.DecodeRuneInString`, 0},
		{"utf16 mechanic", `package p; import "unicode/utf16"; var _ = utf16.Encode`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			diagnostics, _ := analyze(t, pinnedRoot, "runtime.go", tc.src, nil)
			requireDiagnostics(t, diagnostics, tc.want, "uses compiler Unicode data")
		})
	}
}

func TestCompilerConstraints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		constraint string
		want       int
	}{
		{"//go:build go1.27", 1},
		{"//go:build !go1.27", 1},
		{"//go:build linux && go1.27", 1},
		{"//go:build !((go1.26 || amd64) && go1.27)", 1},
		{"//go:build linux || go1.27", 1},
		{"// +build go1.27", 1},
		{"// +build linux,go1.27", 1},
		{"//go:build linux && amd64", 0},
		{"//go:build custom_go1.27", 0},
		{"//go:build go1.bad || go1. || go1.27rc1", 0},
		{"// go1.27 is an ordinary comment", 0},
	} {
		t.Run(tc.constraint, func(t *testing.T) {
			t.Parallel()
			diagnostics, _ := analyze(t, pinnedRoot, "tables.go", tc.constraint+"\n\npackage p\n", nil)
			requireDiagnostics(t, diagnostics, tc.want, "compiler-release build constraint")
		})
	}
	diagnostics, _ := analyze(t, pinnedRoot, "runtime.go", "package p\n//go:build go1.27\nvar _ = 1\n", nil)
	requireDiagnostics(t, diagnostics, 0, "")
}

func TestRuntimeScopeAndTestExclusion(t *testing.T) {
	t.Parallel()
	const src = `package p; import "strings"; var _ = strings.ToLower`
	for _, pkg := range []string{pinnedRoot, pinnedRoot + "/tools", pinnedRoot + "/text/internal/language/compact"} {
		diagnostics, _ := analyze(t, pkg, "runtime.go", src, nil)
		requireDiagnostics(t, diagnostics, 1, "strings.ToLower")
	}
	for _, pkg := range []string{"fdb.dev/tools/unicode15gen", "fdb.dev/pkg/other", pinnedRoot + "extra"} {
		diagnostics, fact := analyze(t, pkg, "runtime.go", src, nil)
		requireDiagnostics(t, diagnostics, 0, "")
		if fact.Reason == "" {
			t.Fatalf("local wrapper %s did not export its violation", pkg)
		}
	}
	diagnostics, fact := analyze(t, pinnedRoot, "runtime_test.go", src, nil)
	requireDiagnostics(t, diagnostics, 0, "")
	if fact.Reason != "" {
		t.Fatalf("test source tainted runtime fact: %+v", fact)
	}
	diagnostics, fact = analyze(t, "third.party/example", "runtime.go", src, nil)
	requireDiagnostics(t, diagnostics, 0, "")
	if fact != nil {
		t.Fatalf("external package exported local closure fact: %+v", fact)
	}
}

func TestIndirectLocalDependencies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  string
		bad  bool
	}{
		{"unicode import", `package p; import _ "unicode"`, true},
		{"norm import", `package p; import _ "golang.org/x/text/unicode/norm"`, true},
		{"unknown external wrapper", `package p; import _ "example.org/wrapper"`, true},
		{"helper function value", `package p; import "strings"; var _ = strings.ToLower`, true},
		{"compiler selector", "//go:build go1.27\n\npackage p", true},
		{"fixed encoding", `package p; import _ "unicode/utf8"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leaf := "fdb.dev/pkg/leaf"
			middle := "fdb.dev/pkg/middle"
			facts := map[string]dependencyFact{}
			_, fact := analyze(t, leaf, "leaf.go", tc.src, facts)
			facts[leaf] = *fact
			_, fact = analyze(t, middle, "middle.go", fmt.Sprintf("package p; import _ %q", leaf), facts)
			facts[middle] = *fact
			diagnostics, _ := analyze(t, pinnedRoot, "runtime.go", fmt.Sprintf("package p; import _ %q", middle), facts)
			want := 0
			if tc.bad {
				want = 1
			}
			requireDiagnostics(t, diagnostics, want, middle+" -> "+middle+": runtime dependency "+leaf)
		})
	}
}

var compatibilityFiles = []struct {
	pkg  string
	file string
}{
	{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/text_tokenizer.go"},
	{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/text_index_maintainer.go"},
	{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/collate_function_key_expression.go"},
	{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/keystore_key_manager.go"},
	{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/java_parse.go"},
	{"fdb.dev/pkg/recordlayer/query/plan/cascades/values", "pkg/recordlayer/query/plan/cascades/values/values.go"},
	{"fdb.dev/pkg/recordlayer/query/plan/cascades/values", "pkg/recordlayer/query/plan/cascades/values/uuid_parse.go"},
	{"fdb.dev/pkg/relational/core/parser", "pkg/relational/core/parser/case_insensitive.go"},
}

func TestCompatibilityCallerMutations(t *testing.T) {
	t.Parallel()
	if len(compatibilityFiles) != 8 {
		t.Fatalf("compatibility caller controls = %d, want 8", len(compatibilityFiles))
	}
	for _, caller := range compatibilityFiles {
		t.Run(caller.file, func(t *testing.T) {
			t.Parallel()
			for _, pkg := range []string{
				"unicode", "golang.org/x/text/cases", "golang.org/x/text/unicode/norm",
				"golang.org/x/text/collate", "golang.org/x/text/collate/build",
				"golang.org/x/text/language", "golang.org/x/text/language/display",
			} {
				t.Run(pkg, func(t *testing.T) {
					t.Parallel()
					src := fmt.Sprintf("package p; import _ %q", pkg)
					diagnostics, _ := analyze(t, caller.pkg, caller.file, src, nil)
					requireDiagnostics(t, diagnostics, 1, "Unicode 15 compatibility caller: import "+pkg)
				})
			}
			for _, pkg := range []string{"strings", "bytes"} {
				for _, name := range []string{
					"ToLower", "ToUpper", "ToTitle", "ToLowerSpecial", "ToUpperSpecial", "ToTitleSpecial", "Title", "EqualFold",
				} {
					for _, use := range []string{"value", "call", "dot import"} {
						t.Run(pkg+"."+name+"/"+use, func(t *testing.T) {
							t.Parallel()
							src := fmt.Sprintf("package p; import alias %q; var _ = alias.%s", pkg, name)
							if use == "call" {
								src = fmt.Sprintf("package p; import alias %q; func f() { alias.%s() }", pkg, name)
							} else if use == "dot import" {
								src = fmt.Sprintf("package p; import . %q; var _ = %s", pkg, name)
							}
							diagnostics, _ := analyze(t, caller.pkg, caller.file, src, nil)
							requireDiagnostics(t, diagnostics, 1, "Unicode 15 compatibility caller: "+pkg+"."+name)
						})
					}
				}
			}
		})
	}
}

func TestCompatibilityCallerAllowedControls(t *testing.T) {
	t.Parallel()
	for _, caller := range compatibilityFiles {
		t.Run(caller.file, func(t *testing.T) {
			t.Parallel()
			for _, pkg := range []string{
				"unicode/utf8", "unicode/utf16", "golang.org/x/text/transform", "example.org/wrapper",
				"golang.org/x/text/casesextra", "golang.org/x/text/unicode/normextra",
				pinnedRoot, pinnedRoot + "/text/unicode/norm", pinnedRoot + "/text/collate",
				pinnedRoot + "/text/language",
			} {
				src := fmt.Sprintf("package p; import _ %q", pkg)
				diagnostics, _ := analyze(t, caller.pkg, caller.file, src, map[string]dependencyFact{pkg: {}})
				requireDiagnostics(t, diagnostics, 0, "")
			}
			const src = `package p
import "strings"
import "bytes"
var _, _, _, _, _, _ = strings.TrimSpace, strings.Fields, strings.FieldsSeq, strings.Compare, bytes.Compare, bytes.ToValidUTF8
func f() { strings.TrimSpace() }
`
			diagnostics, _ := analyze(t, caller.pkg, caller.file, src, nil)
			requireDiagnostics(t, diagnostics, 0, "")
			const shadow = `package p; import "strings"; var _ = strings.Compare; func f() { strings := struct{ToLower func()}{func(){}}; strings.ToLower() }`
			diagnostics, _ = analyze(t, caller.pkg, caller.file, shadow, nil)
			requireDiagnostics(t, diagnostics, 0, "")
			diagnostics, _ = analyze(t, caller.pkg, caller.file, `package p; import _ "fdb.dev/pkg/wrapper"`, map[string]dependencyFact{
				"fdb.dev/pkg/wrapper": {Reason: "fdb.dev/pkg/wrapper: uses unicode"},
			})
			requireDiagnostics(t, diagnostics, 0, "")
			diagnostics, _ = analyze(t, caller.pkg, caller.file, "//go:build go1.27\n\npackage p", nil)
			requireDiagnostics(t, diagnostics, 0, "")
		})
	}
}

func TestCompatibilityCallerBoundaries(t *testing.T) {
	t.Parallel()
	const src = `package p; import "strings"; import _ "unicode"; var _ = strings.ToLower`
	for _, tc := range []struct {
		pkg, filename string
		want          int
	}{
		{"fdb.dev/pkg/recordlayer", "/tmp/execroot/pkg/recordlayer/text_tokenizer.go", 2},
		{"fdb.dev/pkg/recordlayer", `C:\execroot\pkg\recordlayer\text_tokenizer.go`, 2},
		{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/text_index_maintainer.go", 2},
		{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/text_index_maintainer_test.go", 0},
		{"fdb.dev/pkg/unrelated", "pkg/unrelated/text_index_maintainer.go", 0},
		{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/unrelated.go", 0},
		{"fdb.dev/pkg/recordlayer", "pkg/recordlayer/text_tokenizer_test.go", 0},
		{"fdb.dev/pkg/unrelated", "pkg/unrelated/text_tokenizer.go", 0},
		{"fdb.dev/pkg/recordlayer/query/plan/plans", "pkg/recordlayer/query/plan/plans/values.go", 0},
	} {
		diagnostics, _ := analyze(t, tc.pkg, tc.filename, src, nil)
		requireDiagnostics(t, diagnostics, tc.want, "Unicode 15 compatibility caller:")
	}
}

func TestMissingDependencyFactFailsClosed(t *testing.T) {
	t.Parallel()
	diagnostics, _ := analyze(t, pinnedRoot, "runtime.go", `package p; import _ "fdb.dev/pkg/wrapper"`, nil)
	requireDiagnostics(t, diagnostics, 1, "has no Unicode pin fact")
}

func TestTestOnlyDependencyDoesNotTaintRuntime(t *testing.T) {
	t.Parallel()
	diagnostics, fact := analyze(t, pinnedRoot, "helper_test.go", `package p; import _ "fdb.dev/pkg/wrapper"; import _ "unicode"`, nil)
	requireDiagnostics(t, diagnostics, 0, "")
	if fact.Reason != "" {
		t.Fatalf("test-only imports tainted runtime: %+v", fact)
	}
}

type sourceCensus struct {
	files    int
	packages map[string]int
	findings []string
}

func scanSources(t *testing.T, root string) sourceCensus {
	t.Helper()
	census := sourceCensus{packages: map[string]int{}}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		census.files++
		rel, err := filepath.Rel(root, filepath.Dir(filename))
		if err != nil {
			return err
		}
		census.packages[filepath.ToSlash(rel)]++
		for _, f := range sourceFindings(file) {
			census.findings = append(census.findings, fmt.Sprintf("%s: %s", fset.Position(f.pos), f.reason))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("source census: %v", err)
	}
	if census.files == 0 {
		t.Fatal("source census found no runtime Go files; check Bazel data dependencies")
	}
	return census
}

func TestCompilerExcludedSourceIsChecked(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for filename, src := range map[string]string{
		"runtime.go":     "package p\n",
		"future.go":      "//go:build go1.99\n\npackage p\nimport _ \"unicode\"\nimport _ \"example.org/wrapper\"\n",
		"future_test.go": "//go:build go1.99\n\npackage p\nimport _ \"unicode\"\n",
	} {
		if err := os.WriteFile(filepath.Join(root, filename), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	census := scanSources(t, root)
	if census.files != 2 || len(census.findings) != 3 {
		t.Fatalf("census = %+v, want 2 runtime files and 3 findings", census)
	}
	for _, f := range census.findings {
		if !strings.Contains(f, "future.go:") {
			t.Errorf("finding %q did not come from compiler-excluded source", f)
		}
	}
	if err := os.Remove(filepath.Join(root, "future.go")); err != nil {
		t.Fatal(err)
	}
	clean := scanSources(t, root)
	if clean.files != 1 || len(clean.findings) != 0 {
		t.Fatalf("nonempty clean control = %+v, want 1 runtime file, no findings", clean)
	}
}

func TestPinnedRuntimeSources(t *testing.T) {
	t.Parallel()
	root := runtimeSourceRoot(t)
	census := scanSources(t, root)
	for _, pkg := range []string{
		".", "text/collate", "text/unicode/norm", "text/transform", "text/language",
		"text/internal/colltab", "text/internal/language", "text/internal/language/compact", "text/internal/tag",
	} {
		if census.packages[pkg] == 0 {
			t.Errorf("package %s has no source inputs; check //internal/unicode15:all_sources", pkg)
		}
	}
	for _, f := range census.findings {
		t.Error(f)
	}
	t.Logf("checked %d runtime source files in %d packages, including compiler-excluded sources", census.files, len(census.packages))
}

func runtimeSourceRoot(t *testing.T) string {
	t.Helper()
	if sd, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); sd != "" && ws != "" {
		root := filepath.Join(sd, ws, "internal/unicode15")
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			t.Fatalf("missing Bazel runtime sources at %s; check test data", root)
		}
		return root
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		root := filepath.Join(cwd, "internal/unicode15")
		if st, err := os.Stat(root); err == nil && st.IsDir() {
			return root
		}
		parent := filepath.Dir(cwd)
		if cwd == parent {
			t.Fatal("cannot locate internal/unicode15 source inputs")
		}
		cwd = parent
	}
}
