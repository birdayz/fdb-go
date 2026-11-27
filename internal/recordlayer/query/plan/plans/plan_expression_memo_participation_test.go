package plans

import (
	"embed"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// plansSourceFS embeds this package's own source so the assertion below can
// enumerate the DECLARED plan types — the enumeration Go reflection cannot
// provide and an explicit list would rot. Embedding rather than reading the
// directory is what makes it work under the Bazel sandbox, which stages
// runfiles rather than the source tree. Test files are filtered at parse time.
//
//go:embed *.go
var plansSourceFS embed.FS

// TestEveryPlanTypeIsAMemoParticipant pins RFC-183 P5 step 3: every plan type
// must carry GetRecordQueryPlan, because that method is the half of cascades'
// physicalPlanExpression assertion that PlanExprBase cannot supply (the base
// is zero-size and has no back-pointer to the value embedding it).
//
// The failure this guards is the silent one: a new plan type without the
// accessor is not recognised as physical, so the memo treats it as a logical
// expression and the yield-time shell and reachability checks skip it
// entirely — no error, just a node the invariants stop covering.
//
// A plan type is identified by its GetChildren method, which the
// RecordQueryPlan interface requires of every implementation.
func TestEveryPlanTypeIsAMemoParticipant(t *testing.T) {
	t.Parallel()

	entries, err := plansSourceFS.ReadDir(".")
	if err != nil {
		t.Fatalf("reading embedded sources: %v", err)
	}

	fset := token.NewFileSet()
	planTypes := map[string]bool{}   // declares GetChildren
	hasAccessor := map[string]bool{} // declares GetRecordQueryPlan

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := plansSourceFS.ReadFile(name)
		if readErr != nil {
			t.Fatalf("reading embedded %s: %v", name, readErr)
		}
		f, parseErr := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", name, parseErr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			ident, ok := star.X.(*ast.Ident)
			if !ok {
				continue
			}
			switch fn.Name.Name {
			case "GetChildren":
				planTypes[ident.Name] = true
			case "GetRecordQueryPlan":
				hasAccessor[ident.Name] = true
			}
		}
	}

	if len(planTypes) == 0 {
		t.Fatal("found no plan types — the GetChildren discovery heuristic broke")
	}

	var missing []string
	for typ := range planTypes {
		if !hasAccessor[typ] {
			missing = append(missing, typ)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d plan type(s) lack GetRecordQueryPlan, so the memo cannot "+
			"recognise them as physical expressions: %s",
			len(missing), strings.Join(missing, ", "))
	}
}
