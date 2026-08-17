package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Some plan accessors hand back the LIVE slice backing a field that is folded into
// the plan's structural key, rather than a copy. `GetSortKeys`, `GetInValues` and
// `GetInSources` all do, deliberately: their readers are the planner's hot loop —
// the cost model iterates these per candidate — so a defensive copy on every call
// would tax the exact path the structural-hash memo was added to relieve.
//
// The cost of that choice is an aliasing hazard the receiver-write ratchet next door
// structurally cannot see. `plan.GetSortKeys()[0].Desc = true` rewrites the plan's
// identity with its pointer unchanged, through no method on the plan and no
// assignment to any receiver. `SortKey`'s fields are exported, so it needs no
// privileged access at all.
//
// That is the same failure the memo's owner check cannot detect — content changing
// under a stable identity — reached from the read end instead of the write end. The
// write end is closed by copying in the constructors and builders, so no two plans
// share one array. This closes the read end, and it is the cheaper half: the writes
// simply do not exist, and a static check keeps it that way at no runtime cost.
//
// # Why a gate rather than the doc note that was there
//
// Each of those getters already documents "callers must not write through it". A
// convention true only by documentation is exactly what this PR spent its length
// removing elsewhere: three in-place setters were correct by an unwritten ordering
// at every call site until the memo made the shape lethal. Zero write sites today is
// the moment to ratchet it, not a reason to skip it.
//
// # What is NOT covered, said plainly
//
// The element level. IN values are `any` and `[]byte` is a supported IN value type,
// so two plans can still share a `[]byte` payload; a write through one is invisible
// here and to every other gate. There is no writer today and closing it would mean
// type-switching on `any` in a planner path, so it is documented at
// `copyPreservingNil` rather than gated.
//
// A METHOD CALL in the chain is also not descended — `GetSortKeys()[0].Norm().F = x`
// goes unreported. That is deliberate and not the same judgement as the element
// level: descending arbitrary calls would flag any `f(x).field = v` whose root merely
// passes through a watched name, and a gate that fires on legal code gets read past.
// The element WRITE, by contrast, is now detected even though the element COPY is
// not closed — those are two different halves and only the copy is open.

// liveIdentityGetters are accessors that return a live slice which is folded into
// the receiver's structural key. Writing through one rewrites plan identity.
var liveIdentityGetters = map[string]bool{
	"GetSortKeys":  true,
	"GetInValues":  true,
	"GetInSources": true,
}

// rootGetterCall reports the method name when e is ultimately rooted at a CALL to
// one of the watched getters.
//
// It descends the same shapes as an assignment target — index, selector, pointer,
// paren — but bottoms out at a CallExpr instead of an identifier. That is the whole
// difference between this gate and the receiver-write one: there the root is the
// receiver, here it is a function result nobody owns.
func rootGetterCall(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.SelectorExpr:
			e = t.X
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.SliceExpr:
			e = t.X
		case *ast.TypeAssertExpr:
			// IN values are `any`, so a type assertion is not an exotic spelling of
			// an element write — it is the ONLY one. `p.GetInValues()[0].([]byte)[0] = 1`
			// compiles, and because the copy is deep to the slice level and not the
			// element level, it mutates the payload shared by a plan and its copy:
			// both read back the modified bytes. Without this arm the gate was blind
			// to the single form the hazard can take.
			e = t.X
		case *ast.CallExpr:
			sel, ok := t.Fun.(*ast.SelectorExpr)
			if !ok {
				return ""
			}
			if liveIdentityGetters[sel.Sel.Name] {
				return sel.Sel.Name
			}
			return ""
		default:
			return ""
		}
	}
}

// scanLiveGetterWrites reports every assignment whose target is rooted at a call to
// a live-identity getter.
func scanLiveGetterWrites(f *ast.File, report func(pos token.Pos, getter string)) {
	ast.Inspect(f, func(n ast.Node) bool {
		var targets []ast.Expr
		switch s := n.(type) {
		case *ast.AssignStmt:
			targets = s.Lhs
		case *ast.IncDecStmt:
			targets = []ast.Expr{s.X}
		default:
			return true
		}
		for _, target := range targets {
			if getter := rootGetterCall(target); getter != "" {
				report(target.Pos(), getter)
			}
		}
		return true
	})
}

// TestNothingWritesThroughALiveIdentityGetter is the ratchet. Zero, no allowlist: a
// caller that needs to modify these values builds a new plan through the copying
// builder, which is what the builder exists for.
//
// Unlike its siblings this one covers TEST files too. The hazard needs no privileged
// access — `SortKey`'s fields are exported and the getters are exported — so a test
// in any package can reach it, and a test that mutates a plan the memo already holds
// is a real defect rather than a fixture convenience.
func TestNothingWritesThroughALiveIdentityGetter(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	defined := map[string]bool{}
	var findings []string

	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasPrefix(rel, "gen/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && liveIdentityGetters[fn.Name.Name] {
				defined[fn.Name.Name] = true
			}
		}
		scanLiveGetterWrites(f, func(pos token.Pos, getter string) {
			findings = append(findings, fmt.Sprintf("%s:%d: writes through %s()",
				rel, fset.Position(pos).Line, getter))
		})
	}

	// Vacuity guard, and it is the one that matters here: this gate watches a LIST of
	// names. Rename a getter and the gate keeps reporting a confident zero over a name
	// nothing calls any more — a green that means "nothing to look at" instead of
	// "nothing wrong". Require every watched name to still be defined.
	for getter := range liveIdentityGetters {
		if !defined[getter] {
			t.Errorf("no method named %s is defined in the tree, so this gate is watching a "+
				"name that no longer exists. Either it was renamed (update "+
				"liveIdentityGetters in the same change) or it now returns a copy and can be "+
				"dropped from the watch list.", getter)
		}
	}

	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d site(s) write through an accessor that returns the plan's LIVE "+
			"identity-bearing slice.\n"+
			"These getters do not copy, on purpose: the cost model reads them per candidate\n"+
			"in the planner's hot loop. So an element write rewrites the plan's structural\n"+
			"key while its pointer stays the same — invisible to the structural-hash memo's\n"+
			"owner check, which compares identity and not content.\n"+
			"Build a new plan through the copying builder instead.\n\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

// --- detector pins -------------------------------------------------------------

func scanLiveGetterSnippet(t *testing.T, body string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", "package p\n\nfunc f() {\n"+body+"\n}\n", parser.ParseComments)
	if err != nil {
		t.Fatalf("parse snippet: %v\n---\n%s", err, body)
	}
	var got []string
	scanLiveGetterWrites(f, func(_ token.Pos, getter string) { got = append(got, getter) })
	return got
}

func TestLiveGetterDetectorFiresOnEveryWriteShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body string }{
		// The shape named in the getters' own doc comments.
		{"field of an indexed element", `plan.GetSortKeys()[0].Desc = true`},
		{"whole element replaced", `plan.GetSortKeys()[0] = k`},
		{"element of a scalar list", `plan.GetInValues()[2] = 7`},
		{"nested index", `plan.GetInSources()[0][1] = 9`},
		{"compound assign", `plan.GetInValues()[0] = 1`},
		{"increment through the getter", `plan.GetSortKeys()[0].Field += "x"`},
		// A slice of the live array still aliases it.
		{"write through a reslice", `plan.GetInValues()[1:][0] = 3`},
		// Not in first position of a multi-assign.
		{"multi-assign", `a, plan.GetInValues()[0] = 1, 2`},
		// The ONLY spelling of an element write, since IN values are `any`. Verified
		// to compile and to mutate the payload shared by a plan and its copy.
		{"element write through a type assertion", `plan.GetInValues()[0].([]byte)[0] = 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scanLiveGetterSnippet(t, tc.body); len(got) == 0 {
				t.Errorf("detector reported nothing; this write would rewrite plan identity "+
					"unnoticed:\n\t%s", tc.body)
			}
		})
	}
}

func TestLiveGetterDetectorStaysSilentOnReads(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body string }{
		{"plain read", `x := plan.GetSortKeys()[0].Desc; _ = x`},
		{"range", `for _, k := range plan.GetSortKeys() { _ = k }`},
		{"len", `n := len(plan.GetInValues()); _ = n`},
		{"assigning the slice header to a local", `keys := plan.GetSortKeys(); _ = keys`},
		// Writing through a DIFFERENT getter is not this gate's business.
		{"unwatched getter", `plan.GetChildren()[0] = nil`},
		// Writing a local copy is the prescribed form.
		{"write to a copy", `keys := append([]int(nil), 1); keys[0] = 2; _ = keys`},
		// A same-named call on something that is not a plan still reports; that is
		// accepted over-approximation, so pin the shape that must NOT report: a
		// bare call with no write.
		{"call with no assignment", `plan.GetSortKeys()`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scanLiveGetterSnippet(t, tc.body); len(got) > 0 {
				t.Errorf("detector reported %v on a read-only shape; a gate that flags reads "+
					"gets read past:\n\t%s", got, tc.body)
			}
		})
	}
}
