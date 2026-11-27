package typeimmutable

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// The analyzer's decision is a two-axis one — is the root LOCAL, and does the
// write go through a REFERENCE it owns — and neither axis is exercised by the
// repo corpus reaching zero diagnostics. A corpus reading only ever says "no
// shape in the tree today trips it", which is equally true of an analyzer that
// reports nothing at all. These drive the axes directly.

// arm is one snippet plus what the analyzer must decide about it.
type arm struct {
	name    string
	body    string
	refused bool
}

// recall arms MUST be refused. Each is a way of writing a graph the function did
// not build, and each was found in this repo rather than invented.
var recallArms = []arm{
	{
		name:    "parameter",
		body:    "func f(r *RecordType) { r.RecordName = \"x\" }",
		refused: true,
	},
	{
		name:    "call result written through a pointer",
		body:    "func f(v Val) { r := v.Type(); r.RecordName = \"x\" }",
		refused: true,
	},
	{
		name:    "call result written through a slice index",
		body:    "func f(v Val) { r := v.Type(); r.Fields[0].Name = \"x\" }",
		refused: true,
	},
	{
		name:    "shallow copy aliases its source's slice",
		body:    "func f(src *RecordType) { cp := *src; cp.Fields[0].Name = \"x\" }",
		refused: true,
	},
	{
		name:    "append onto an existing slice may reuse its array",
		body:    "func f(src *RecordType) { fs := append(src.Fields, Field{}); fs[0].Name = \"x\" }",
		refused: true,
	},
	{
		name:    "increment on a call result",
		body:    "func f(v Val) { r := v.Type(); r.Fields[0].Ordinal++ }",
		refused: true,
	},
	{
		name:    "address taken of a parameter's field",
		body:    "func f(r *RecordType) { _ = &r.Nullable }",
		refused: true,
	},
	{
		name:    "receiver field",
		body:    "type h struct{ r *RecordType }\nfunc (x *h) f() { x.r.RecordName = \"y\" }",
		refused: true,
	},
}

// precision arms MUST be allowed. Every one appears in the repo and reddening
// any of them would make the gate unusable.
var precisionArms = []arm{
	{
		name: "composite literal",
		body: "func f() { r := &RecordType{}; r.RecordName = \"x\" }",
	},
	{
		name: "make-d slice",
		body: "func f() { fs := make([]Field, 2); fs[0].Name = \"x\" }",
	},
	{
		name: "append onto a nil conversion is a copy",
		body: "func f(src *RecordType) { fs := append([]Field(nil), src.Fields...); fs[0].Name = \"x\" }",
	},
	{
		name: "append onto an empty literal is a copy",
		body: "func f(src *RecordType) { fs := append([]Field{}, src.Fields...); fs[0].Name = \"x\" }",
	},
	{
		name: "range value is a copy of the element",
		body: "func f(src *RecordType) { for _, fl := range src.Fields { fl.Ordinal = 1; _ = fl } }",
	},
	{
		name: "shallow copy's OWN field is private to the copy",
		body: "func f(src *RecordType) { cp := *src; cp.Legs = nil; _ = cp }",
	},
	{
		name: "an allocating constructor hands over ownership",
		body: "func f() { r := NewRecordType(\"n\", false, nil); r.Legs = nil }",
	},
	{
		name: "a var declaration owns its zero value",
		body: "func f() { var r RecordType; r.RecordName = \"x\"; _ = r }",
	},
}

// TestFreshConstructorsReallyAllocate verifies the claim freshTypeConstructors
// rests on, rather than trusting its own doc comment: every name on that list
// must be a function in the values package whose body returns a composite
// literal. A constructor that started delegating to a cache would otherwise keep
// its place on the ALLOWED side silently, which is the direction that ships bugs.
func TestFreshConstructorsReallyAllocate(t *testing.T) {
	t.Parallel()

	root := repoRootFromHere(t)
	typeFile := filepath.Join(root, "internal/recordlayer/query/plan/cascades/values/type.go")
	src, err := os.ReadFile(typeFile)
	if err != nil {
		t.Fatalf("reading values/type.go under %s: %v.\nUnder Bazel this file must be a `data` dep of "+
			"this target — a test that cannot find its input must FAIL, not skip: a "+
			"skip here would leave freshTypeConstructors unverified while the target "+
			"still reported green, which is the exact shape this analyzer exists to "+
			"prevent one level down.", root, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, typeFile, src, 0)
	if err != nil {
		t.Fatalf("parsing type.go: %v", err)
	}

	found := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || !freshTypeConstructors[fn.Name.Name] {
			continue
		}
		found[fn.Name.Name] = true
		if !returnsACompositeLiteral(fn) {
			t.Errorf("%s is on freshTypeConstructors but its body does not return a "+
				"composite literal. If it now hands back a cached or interned graph, a "+
				"caller writing to the result corrupts every value flowing that shape — "+
				"remove it from the list rather than relaxing this test", fn.Name.Name)
		}
	}
	for name := range freshTypeConstructors {
		if !found[name] {
			t.Errorf("freshTypeConstructors names %q, which is not a function in "+
				"values/type.go. A name that resolves to nothing silently admits every "+
				"call spelled that way from any package", name)
		}
	}
	if len(found) == 0 {
		t.Fatal("no constructor was located, so this test measured nothing")
	}
}

func returnsACompositeLiteral(fn *ast.FuncDecl) bool {
	saw := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return true
		}
		expr := ret.Results[0]
		if unary, isUnary := expr.(*ast.UnaryExpr); isUnary && unary.Op == token.AND {
			expr = unary.X
		}
		if _, isLit := expr.(*ast.CompositeLit); isLit {
			saw = true
		}
		return true
	})
	return saw
}

func repoRootFromHere(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "MODULE.bazel")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// TestProvenanceClassification drives the two decision axes over the arms above,
// through the same helpers the analyzer uses. It stops short of a full
// analysis.Pass — that needs a type checker the nogo harness supplies — and so
// asserts the SYNTACTIC half: which locals are recognised as constructed, and
// whether a chain passes through a reference. Those two are what every arm turns
// on; the type half (is the owner a values.Type) is exercised by the repo build.
func TestProvenanceClassification(t *testing.T) {
	t.Parallel()

	for _, tc := range append(append([]arm{}, recallArms...), precisionArms...) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn := parseSingleFunc(t, tc.body)
			built := constructedLocals(fn)

			target, ok := firstWriteTarget(fn)
			if !ok {
				t.Fatalf("no write found in the arm body; the arm cannot test anything")
			}
			root, viaReference, _, resolved := rootOfChain(target.X)
			if !resolved {
				t.Fatalf("the write target's root did not resolve; the arm is malformed")
			}
			// A pointer root counts as a reference, which the analyzer decides from
			// type info. The arms spell the pointer-ness into their names, so it is
			// applied here from the same fact the analyzer would read.
			if strings.Contains(tc.body, root+" := &") || strings.Contains(tc.body, root+" *RecordType") ||
				strings.Contains(tc.body, root+" := v.Type()") || strings.Contains(tc.body, "x."+root) {
				viaReference = true
			}
			how, local := built[root]
			refused := !local || (viaReference && !how.freshReference)
			if refused != tc.refused {
				t.Errorf("refused = %v, want %v (root %q local=%v fresh=%v viaRef=%v)",
					refused, tc.refused, root, local, how.freshReference, viaReference)
			}
		})
	}
}

// TestBothArmSetsArePopulated is the vacuity guard: a classifier that answered a
// constant would pass whichever set were empty.
func TestBothArmSetsArePopulated(t *testing.T) {
	t.Parallel()
	if len(recallArms) < 8 {
		t.Errorf("recall arms = %d, want at least 8 — each is a distinct way to write "+
			"a graph you do not own, and the set shrinking is how the gate quietly "+
			"stops covering one", len(recallArms))
	}
	if len(precisionArms) < 8 {
		t.Errorf("precision arms = %d, want at least 8 — a gate that reddens "+
			"legitimate construction gets disabled, so these are as load-bearing as "+
			"the recall arms", len(precisionArms))
	}
	for _, a := range recallArms {
		if !a.refused {
			t.Errorf("recall arm %q is marked allowed; it is in the wrong set", a.name)
		}
	}
	for _, a := range precisionArms {
		if a.refused {
			t.Errorf("precision arm %q is marked refused; it is in the wrong set", a.name)
		}
	}
}

func parseSingleFunc(t *testing.T, body string) *ast.FuncDecl {
	t.Helper()
	src := "package p\ntype Field struct{ Name string; Ordinal int }\n" +
		"type RecordType struct{ RecordName string; Nullable bool; Fields []Field; Legs []int }\n" +
		"type Val interface{ Type() *RecordType }\n" +
		"func NewRecordType(string, bool, []Field) *RecordType { return &RecordType{} }\n" + body
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "arm.go", src, 0)
	if err != nil {
		t.Fatalf("parsing arm: %v\n%s", err, src)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "f" {
			return fn
		}
	}
	t.Fatal("arm body must declare func f")
	return nil
}

// firstWriteTarget returns the first selector-shaped write in a function, which
// is the one every arm is written around.
func firstWriteTarget(fn *ast.FuncDecl) (*ast.SelectorExpr, bool) {
	var found *ast.SelectorExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		var targets []ast.Expr
		switch s := n.(type) {
		case *ast.AssignStmt:
			targets = s.Lhs
		case *ast.IncDecStmt:
			targets = []ast.Expr{s.X}
		case *ast.UnaryExpr:
			if s.Op == token.AND {
				targets = []ast.Expr{s.X}
			}
		}
		for _, target := range targets {
			if sel, ok := target.(*ast.SelectorExpr); ok {
				found = sel
				return false
			}
		}
		return true
	})
	return found, found != nil
}

type stubValuesImporter struct {
	valuesPkgPath string
}

func (s stubValuesImporter) Import(importPath string) (*types.Package, error) {
	pkg := types.NewPackage(importPath, path.Base(importPath))
	if importPath == s.valuesPkgPath {
		fieldStruct := types.NewStruct([]*types.Var{
			types.NewField(token.NoPos, pkg, "Name", types.Typ[types.String], false),
		}, nil)
		fieldType := types.NewNamed(types.NewTypeName(token.NoPos, pkg, "Field", nil), fieldStruct, nil)
		recStruct := types.NewStruct([]*types.Var{
			types.NewField(token.NoPos, pkg, "RecordName", types.Typ[types.String], false),
			types.NewField(token.NoPos, pkg, "Fields", types.NewSlice(fieldType), false),
		}, nil)
		recType := types.NewNamed(types.NewTypeName(token.NoPos, pkg, "RecordType", nil), recStruct, nil)
		pkg.Scope().Insert(fieldType.Obj())
		pkg.Scope().Insert(recType.Obj())
	}
	pkg.MarkComplete()
	return pkg, nil
}

func runTypeImmutablePass(t *testing.T, pkgPath, filename, valuesPkgPath, src string) []analysis.Diagnostic {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	pkg, err := (&types.Config{Importer: stubValuesImporter{valuesPkgPath: valuesPkgPath}}).Check(pkgPath, fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatalf("typecheck %s (%s): %v", pkgPath, filename, err)
	}
	var diagnostics []analysis.Diagnostic
	pass := &analysis.Pass{
		Analyzer:  Analyzer,
		Fset:      fset,
		Files:     []*ast.File{file},
		Pkg:       pkg,
		TypesInfo: info,
		Report:    func(d analysis.Diagnostic) { diagnostics = append(diagnostics, d) },
	}
	if _, err := Analyzer.Run(pass); err != nil {
		t.Fatalf("run: %v", err)
	}
	return diagnostics
}

// TestValuesPkgAndAllowlistPaths verifies that both valuesPkgSuffix matching
// and Allowlist lookups work identically across legacy pkg/... and future
// internal/... package and file paths.
func TestValuesPkgAndAllowlistPaths(t *testing.T) {
	t.Parallel()

	for _, valuesPkg := range []string{
		"fdb.dev/pkg/recordlayer/query/plan/cascades/values",
		"fdb.dev/internal/recordlayer/query/plan/cascades/values",
		"fdb.dev/recordlayer/query/plan/cascades/values",
	} {
		t.Run("enforced/"+valuesPkg, func(t *testing.T) {
			t.Parallel()
			src := "package caller\nimport values \"" + valuesPkg + "\"\nfunc mutate(r *values.RecordType) { r.RecordName = \"x\" }\n"
			diags := runTypeImmutablePass(t, "fdb.dev/internal/relational/query", "internal/relational/query/caller.go", valuesPkg, src)
			if len(diags) != 1 || !strings.Contains(diags[0].Message, "RecordType.RecordName") {
				t.Fatalf("diagnostics = %+v, want 1 diagnostic for %s", diags, valuesPkg)
			}
		})
	}

	// Unrelated package with a matching struct name must not be flagged.
	unrelatedSrc := "package caller\nimport values \"fdb.dev/internal/other/values\"\nfunc mutate(r *values.RecordType) { r.RecordName = \"x\" }\n"
	if diags := runTypeImmutablePass(t, "fdb.dev/internal/relational/query", "internal/relational/query/caller.go", "fdb.dev/internal/other/values", unrelatedSrc); len(diags) != 0 {
		t.Fatalf("unrelated package flagged: %+v", diags)
	}
	// Suffix collision on package name must not match valuesPkgSuffix.
	collisionSrc := "package caller\nimport values \"fdb.dev/internal/myrecordlayer/query/plan/cascades/values\"\nfunc mutate(r *values.RecordType) { r.RecordName = \"x\" }\n"
	if diags := runTypeImmutablePass(t, "fdb.dev/internal/relational/query", "internal/relational/query/caller.go", "fdb.dev/internal/myrecordlayer/query/plan/cascades/values", collisionSrc); len(diags) != 0 {
		t.Fatalf("package suffix collision flagged: %+v", diags)
	}

	allowCases := []struct {
		pkgPath   string
		filename  string
		fnName    string
		valuesPkg string
		wantDiags int
	}{
		{"fdb.dev/internal/recordlayer/query/plan/cascades/values", "internal/recordlayer/query/plan/cascades/values/qov_source_layout.go", "restoreQOVRecordLayout", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 0},
		{"fdb.dev/internal/recordlayer/query/plan/cascades/values", "/abs/execroot/internal/recordlayer/query/plan/cascades/values/qov_source_layout.go", "restoreQOVRecordLayout", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 0},
		{"fdb.dev/internal/recordlayer/query/plan/cascades/values", "internal/recordlayer/query/plan/cascades/values/ordinal_layout_test.go", "TestOrdinalLayoutSnapshotsEveryMutableInputAndGetter", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 0},
		{"fdb.dev/internal/recordlayer/query/plan/cascades", "internal/recordlayer/query/plan/cascades/plan_leg_concat_layout_test.go", "TestPlanLegConcatLayout_ReturnsADefensiveCopy", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 0},
		{"fdb.dev/pkg/relational/core/query", "pkg/relational/core/query/inline_values_translation_test.go", "TestInlineValuesTranslationRejectsCollectionTypeDrift", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 0},
		{"fdb.dev/internal/relational/core/query", "internal/relational/core/query/inline_values_translation_test.go", "TestInlineValuesTranslationRejectsCollectionTypeDrift", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 0},
		{"fdb.dev/internal/relational/query", "internal/relational/query/inline_values_translation_test.go", "TestInlineValuesTranslationRejectsCollectionTypeDrift", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 0},
		{"fdb.dev/internal/recordlayer/query/plan/cascades/values", "internal/recordlayer/query/plan/cascades/values/proto_field_exact_type_test.go", "TestFieldTypeForProtoFieldReturnsIndependentTypeGraphs", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 0},
		// Negative controls: wrong function, unlisted file, or prefix collision.
		{"fdb.dev/internal/recordlayer/query/plan/cascades/values", "internal/recordlayer/query/plan/cascades/values/qov_source_layout.go", "otherFunc", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 1},
		{"fdb.dev/internal/recordlayer/query/plan/cascades/values", "internal/recordlayer/query/plan/cascades/values/unlisted.go", "restoreQOVRecordLayout", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 1},
		{"fdb.dev/internal/recordlayer/query/plan/cascades/values", "notinternal/recordlayer/query/plan/cascades/values/qov_source_layout.go", "restoreQOVRecordLayout", "fdb.dev/internal/recordlayer/query/plan/cascades/values", 1},
	}
	for _, tc := range allowCases {
		t.Run(tc.filename+"/"+tc.fnName, func(t *testing.T) {
			t.Parallel()
			src := "package p\nimport values \"" + tc.valuesPkg + "\"\nfunc " + tc.fnName + "(r *values.RecordType) { r.RecordName = \"x\" }\n"
			diags := runTypeImmutablePass(t, tc.pkgPath, tc.filename, tc.valuesPkg, src)
			if len(diags) != tc.wantDiags {
				t.Fatalf("diagnostics = %+v, want %d", diags, tc.wantDiags)
			}
		})
	}
}
