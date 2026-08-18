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
// share one array.
//
// This gate closes the read-end writes it can see STATICALLY, within one function:
// those rooted at the getter call itself, and those rooted at a local the call was
// assigned to. It does not "close the read end" outright, and the difference is worth
// stating precisely rather than generously — an over-broad scope claim in this very
// header is what let two fail-open instruments through review, so the NOT-covered
// list below is load-bearing rather than decoration.
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
//
// Note this is NOT the same as passing the slice to a call that mutates it in place.
// `sort.SliceStable(p.GetInValues(), less)` and `copy(p.GetInValues(), src)` reorder
// or overwrite the plan's own array with no assignment anywhere, and those ARE
// covered, by a closed four-name list checked on the first argument only. The closed
// list is what separates them: it cannot flag a shape by accident, whereas descending
// any call could. An in-place mutator outside that list — a helper of our own, a
// third-party sorter — is not covered.
//
// ALIASING ACROSS A FUNCTION BOUNDARY. The taint is per-function, so handing the
// slice to a helper that writes it — `mutate(p.GetSortKeys())` — is invisible. Closing
// that needs interprocedural analysis, which is far past what a docscheck gate should
// carry; the single-function form is where the hazard would realistically be written,
// and it is covered. Aliasing through a struct field or a map value is unhandled for
// the same reason: the taint tracks identifiers only.

// liveIdentityGetters are accessors that return a live slice which is folded into
// the receiver's structural key. Writing through one rewrites plan identity.
//
// The DETECTOR matches on name alone, deliberately. It over-approximates — a
// same-named method on some other type reports too — and that is the safe direction
// for a ratchet at zero, since it also happens to cover
// `LogicalSortExpression.GetSortKeys`, whose keys are identity-bearing in the same
// way.
var liveIdentityGetters = map[string]bool{
	"GetSortKeys":  true,
	"GetInValues":  true,
	"GetInSources": true,
}

// liveIdentityGetterOwners keys the VACUITY GUARD on the receiver type, which the
// detector deliberately ignores.
//
// Name alone is not enough here, and the failure was live rather than theoretical:
// `GetSortKeys` is defined TWICE — on `RecordQueryInMemorySortPlan` and on
// `LogicalSortExpression` — so a guard asking merely "does some method of this name
// exist" was satisfied by the unrelated one. Renaming the PLAN's getter, the very
// accessor this gate was built for, left the gate reporting a confident zero. The
// single-definition getters (`GetInValues`, `GetInSources`) fired correctly, so the
// guard looked verified because the case that was checked was the case that worked.
//
// That is the guard's own failure mode turned on itself: an instrument that cannot
// tell "nothing wrong" from "nothing to look at".
var liveIdentityGetterOwners = map[string]string{
	"GetSortKeys":  "RecordQueryInMemorySortPlan",
	"GetInValues":  "RecordQueryInJoinPlan",
	"GetInSources": "RecordQueryInUnionPlan",
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

// mutatingCallName reports whether fun names a call that mutates its FIRST argument
// in place.
//
// A closed list, deliberately. These four are the idioms that reorder or overwrite a
// slice with no assignment anywhere, and keeping the list closed is exactly what
// makes this arm safe where descending arbitrary calls would not be: there is no
// shape it can flag by accident. Reachability is not hypothetical — `sort.` appears
// across the query subtree, and `in_source.go` sorts a DEFENSIVE COPY one function
// away from `WithInValues`, so deleting that copy as redundant writes the unguarded
// form directly.
func mutatingCallName(fun ast.Expr) bool {
	switch t := fun.(type) {
	case *ast.Ident:
		return t.Name == "copy"
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok || pkg.Name != "sort" {
			return false
		}
		switch t.Sel.Name {
		case "Slice", "SliceStable", "Sort":
			return true
		}
	}
	return false
}

// rootLocalIdent walks an assignment target down to the identifier it is rooted at,
// descending the same shapes as rootGetterCall. It is how a write THROUGH a local
// alias is traced back to the local.
// It also reports whether the descent crossed a SHARING node — a pointer
// dereference, an index, or a type assertion. That distinction is what makes range
// variables tractable: `for _, k := range p.GetSortKeys()` copies each element, so
// writing a value field of the copy cannot reach the plan, but the copy still shares
// anything the element merely POINTS at. `SortKey.ValueExpr` is an interface and
// `GetInValues()` yields `any`, so `v.([]byte)[0] = 1` inside a range writes the
// payload both the copy and the plan hold.
func rootLocalIdent(e ast.Expr) (name string, through, viaSharing bool) {
	depth := 0
	shared := false
	for {
		switch t := e.(type) {
		case *ast.IndexExpr:
			e, depth, shared = t.X, depth+1, true
		case *ast.IndexListExpr:
			e, depth, shared = t.X, depth+1, true
		case *ast.SelectorExpr:
			e, depth = t.X, depth+1
		case *ast.StarExpr:
			e, depth, shared = t.X, depth+1, true
		case *ast.ParenExpr:
			e = t.X
		case *ast.SliceExpr:
			e, depth, shared = t.X, depth+1, true
		case *ast.TypeAssertExpr:
			e, depth, shared = t.X, depth+1, true
		case *ast.Ident:
			// depth 0 is a bare rebinding (`keys = other`), which replaces the alias
			// rather than writing through it.
			return t.Name, depth > 0, shared
		default:
			return "", false, false
		}
	}
}

// scanLiveGetterWrites reports writes that reach a live-identity slice, in both the
// shapes that occur: rooted DIRECTLY at the getter call, and rooted at a LOCAL the
// getter's result was assigned to.
//
// The aliased form is not an exotic variant — it is what anyone performing more than
// one write naturally writes, and the gate was blind to it while its own precision
// table asserted silence on the very assignment that creates the alias:
//
//	keys := p.GetSortKeys()
//	keys[0].Desc = true      // rewrites p's structural key, pointer unchanged
//
// The taint is per-function and deliberately simple. An identifier becomes tainted
// when it is assigned an expression rooted at a watched getter, and untainted when it
// is rebound to anything else. Only writes THROUGH a tainted identifier report; a
// bare rebinding does not, because it replaces the alias rather than mutating what it
// points at.
//
// A `range` variable is tainted SEPARATELY, as a copy, because "it is a copy so
// mutating it is silent" was an incomplete scope claim of exactly the kind this file
// keeps producing. `for _, k := range p.GetSortKeys()` copies the struct and the
// interface header — not the pointee and not a backing array. So a value-field write
// on the copy really is silent and must stay so, while a write that crosses a
// SHARING node reaches state the plan still holds:
//
//	for _, k := range p.GetSortKeys() { k.Desc = true }        // copy, silent
//	for _, v := range p.GetInValues() { v.([]byte)[0] = 1 }    // shared payload, reported
//
// `GetInValues` yields `any` and `SortKey.ValueExpr` is an interface, so the second
// shape is reachable in this tree rather than hypothetical. The copy taint therefore
// reports only when rootLocalIdent says the descent crossed a pointer dereference,
// an index, or a type assertion.
//
// Exactly which spellings it follows, because "assigned to a local" turned out to
// cover less than it sounded like:
//
//   - `:=` and `=`, via AssignStmt.
//   - `var keys = p.GetSortKeys()`, via DeclStmt → GenDecl → ValueSpec. This is
//     ordinary Go and was silent until it was named: the earlier pass switched on
//     AssignStmt alone, so the most common alternative spelling never reached the
//     taint map.
//   - alias OF an alias, transitively — `k2 := k1` propagates whatever `k1` aliases,
//     to any depth. Before that, a bare identifier on the right-hand side matched no
//     getter and took the untaint branch, so chaining actively CLEARED the taint
//     rather than merely failing to extend it.
func scanLiveGetterWrites(f *ast.File, report func(pos token.Pos, getter string)) {
	scan := func(body *ast.BlockStmt) {
		tainted := map[string]string{}     // local ident -> the getter it aliases
		copyTainted := map[string]string{} // range COPY -> the getter it was copied from

		// taint records name as aliasing whatever src aliases: either src is rooted at
		// a watched getter, or src is itself an already-tainted identifier. The second
		// case is what makes `k2 := k1` propagate rather than silently untaint.
		taint := func(name string, src ast.Expr) (string, bool) {
			if getter := rootGetterCall(src); getter != "" {
				return getter, true
			}
			if id, ok := src.(*ast.Ident); ok {
				if getter, ok := tainted[id.Name]; ok {
					return getter, true
				}
			}
			return "", false
		}

		ast.Inspect(body, func(n ast.Node) bool {
			var targets []ast.Expr
			var rhs []ast.Expr
			assignTok := token.ILLEGAL
			switch s := n.(type) {
			case *ast.AssignStmt:
				targets, rhs, assignTok = s.Lhs, s.Rhs, s.Tok
			case *ast.IncDecStmt:
				targets = []ast.Expr{s.X}
			case *ast.CallExpr:
				// In-place mutation through a CALL, not an assignment:
				// `sort.SliceStable(p.GetInValues(), less)` reorders the plan's own
				// backing array and rewrites its structural key with the pointer
				// unchanged. Structurally invisible to everything above, which only
				// looks at assignment targets.
				//
				// A CLOSED list of four names, first argument only. That is what keeps
				// it free of the over-approximation that rules out descending arbitrary
				// calls: `copy(dst, p.GetInValues())` reads the getter and is correctly
				// silent, as is `len(p.GetInValues())`. This is deliberately not the
				// same shape as the method-call-in-chain case the header declines.
				if !mutatingCallName(s.Fun) || len(s.Args) == 0 {
					return true
				}
				if getter := rootGetterCall(s.Args[0]); getter != "" {
					report(s.Args[0].Pos(), getter)
				} else if id, ok := s.Args[0].(*ast.Ident); ok {
					if getter, found := tainted[id.Name]; found {
						report(s.Args[0].Pos(), getter)
					}
				}
				return true
			case *ast.RangeStmt:
				// Ranging over a watched getter copies each element. The copy shares
				// whatever the element points at, so the value variable is tainted as a
				// COPY: writes through it report only when they cross a sharing node.
				// Resolve the range SOURCE through the taint maps, not just as a direct
				// call. `vals := p.GetInValues(); for _, v := range vals` is the second
				// category this file's header positively claims to cover — a write
				// rooted at a local the call was assigned to — and matching only
				// `rootGetterCall(s.X)` missed it. Falling through copyTainted covers
				// the nested case: an outer range copy shares the inner slice's backing
				// array, so its elements are shared too.
				getter := rootGetterCall(s.X)
				if getter == "" {
					if id, ok := s.X.(*ast.Ident); ok {
						if g, found := tainted[id.Name]; found {
							getter = g
						} else if g, found := copyTainted[id.Name]; found {
							getter = g
						}
					}
				}
				if getter != "" {
					if id, ok := s.Value.(*ast.Ident); ok && id.Name != "_" {
						copyTainted[id.Name] = getter
					}
				}
				return true
			case *ast.DeclStmt:
				// `var keys = p.GetSortKeys()` is a ValueSpec, not an AssignStmt, so
				// without this arm the most ordinary alternative spelling of `:=` never
				// reaches the taint map at all.
				gen, ok := s.Decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					return true
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if getter, ok := taint(name.Name, vs.Values[i]); ok {
							tainted[name.Name] = getter
						}
					}
				}
				return true
			default:
				return true
			}

			for i, target := range targets {
				// Rooted directly at the getter call.
				if getter := rootGetterCall(target); getter != "" {
					report(target.Pos(), getter)
					continue
				}
				// Rooted at a local that aliases one.
				if name, through, viaSharing := rootLocalIdent(target); through {
					if getter, ok := tainted[name]; ok {
						report(target.Pos(), getter)
					} else if getter, ok := copyTainted[name]; ok && viaSharing {
						// A range COPY: only a write that crosses a pointer, index or
						// type assertion reaches state the plan still holds. A plain
						// field write on the copy is correctly silent.
						report(target.Pos(), getter)
					}
					continue
				}
				// A bare identifier target: taint it, or clear a stale taint.
				id, ok := target.(*ast.Ident)
				if !ok || i >= len(rhs) {
					continue
				}
				// The two maps want OPPOSITE rebinding behaviour, which is why this is
				// not one delete. For `tainted`, a `:=` on an already-tainted name is an
				// inner shadow and the outer alias is still live, so the taint must
				// survive. For `copyTainted`, the range variable's scope has ALREADY
				// ENDED by the time any later statement rebinds its name, so every
				// rebinding — `:=` or `=` — refers to a different variable and the taint
				// must go. Leaving it made the gate report a later `v := other(); v[0] = 1`
				// as a plan-identity rewrite: a false positive, which on a ratchet at zero
				// blocks a legitimate change and is worse than a miss.
				delete(copyTainted, id.Name)
				if getter, ok := taint(id.Name, rhs[i]); ok {
					tainted[id.Name] = getter
				} else if assignTok != token.DEFINE {
					// `:=` DECLARES. When the name is already tainted this can only be an
					// inner-scope shadow, and the outer alias is still live — so clearing
					// here would untaint a variable that is still aliasing the plan. The
					// taint map is flat, which is what makes the distinction necessary.
					delete(tainted, id.Name)
				}
			}
			return true
		})
	}

	// FuncDecl bodies AND package-level function literals (`var f = func() {…}` is a
	// GenDecl, so a FuncDecl-only walk skipped it entirely — defeating not just the
	// taint layer but the direct getter detection too). FuncLits nested inside a
	// FuncDecl are already reached, since ast.Inspect descends into them.
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			scan(fn.Body)
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok && lit.Body != nil {
				scan(lit.Body)
			}
			return true
		})
	}
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
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			if liveIdentityGetterOwners[fn.Name.Name] == "" {
				continue
			}
			// Qualified by receiver type: a same-named getter on another type must
			// not satisfy the guard for this one.
			defined[receiverTypeName(fn.Recv.List[0].Type)+"."+fn.Name.Name] = true
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
	for getter, owner := range liveIdentityGetterOwners {
		if !defined[owner+"."+getter] {
			t.Errorf("%s.%s is not defined in the tree, so this gate is watching an accessor "+
				"that no longer exists. Either it was renamed (update liveIdentityGetters and "+
				"liveIdentityGetterOwners in the same change) or it now returns a copy and can "+
				"be dropped from the watch list.\n"+
				"Note this is checked per RECEIVER TYPE on purpose: %s is also defined on other "+
				"types, and a name-only check was satisfied by one of those while the accessor "+
				"this gate exists for had been renamed away.", owner, getter, getter)
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
		// These two arms were previously mislabelled: an assignment called "compound
		// assign" (a duplicate of the arm above) and a compound assign called
		// "increment". The word "increment" appearing made the IncDecStmt branch READ as
		// covered while nothing drove it — deleting that whole branch left every arm and
		// the tree ratchet green.
		{"compound assign", `plan.GetSortKeys()[0].Field += "x"`},
		{"increment", `plan.GetInValues()[0]++`},
		// A slice of the live array still aliases it.
		{"write through a reslice", `plan.GetInValues()[1:][0] = 3`},
		// Aliased through a local: the two-statement form, which anyone doing more
		// than one write reaches for. The gate was blind to this while its own
		// precision table asserted silence on the assignment that creates the alias.
		{"aliased through a local", "keys := plan.GetSortKeys()\nkeys[0].Desc = true"},
		{"aliased nested index", "src := plan.GetInSources()\nsrc[0][0] = 1"},
		{"aliased element via type assertion", "b := plan.GetInValues()[0].([]byte)\nb[0] = 1"},
		{"aliased then incremented", "v := plan.GetInValues()\nv[0]++"},
		// `var x = …` is a ValueSpec, not an AssignStmt, and was never taint-tracked.
		{"aliased via var declaration", "var keys = plan.GetSortKeys()\nkeys[0].Desc = true"},
		// Alias of an alias: the RHS is a bare ident, which previously UNTAINTED.
		{"alias of an alias", "k1 := plan.GetSortKeys()\nk2 := k1\nk2[0].NullsFirst = true"},
		// An inner-scope `:=` on a shadowing name must not clear the OUTER alias.
		// The taint map is flat, so an unconditional delete untainted a live alias.
		{"outer alias survives an inner shadow", "keys := plan.GetSortKeys()\nif c { keys := other()\n_ = keys }\nkeys[0].Desc = true"},
		// A range COPY shares whatever the element points at. GetInValues yields
		// `any`, so the payload is shared even though the interface header is copied.
		{"range copy written through its interface payload", "for _, v := range plan.GetInValues() { v.([]byte)[0] = 1 }"},
		{"range copy written through a pointer field", "for _, k := range plan.GetSortKeys() { *k.Ptr = true }"},
		// Ranging over an ALIAS rather than over the call. This sits inside what the
		// header positively claims — a write rooted at a local the call was assigned to.
		{"range over a tainted alias", "vals := plan.GetInValues()\nfor _, v := range vals { v.([]byte)[0] = 1 }"},
		// An outer range copy shares the inner slice, so its elements are shared too.
		{"inner range over an outer range copy", "for _, src := range plan.GetInSources() { for _, v := range src { v.([]byte)[0] = 1 } }"},
		// In-place mutation through a call argument: no assignment at all.
		{"sort.SliceStable over the live slice", "sort.SliceStable(plan.GetInValues(), less)"},
		{"sort.Slice over a tainted alias", "vals := plan.GetInValues()\nsort.Slice(vals, less)"},
		{"copy into the live slice", "copy(plan.GetInValues(), src)"},
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
		// A range variable is a COPY in Go, so mutating it cannot reach the plan.
		{"range variable is a copy", `for _, k := range plan.GetSortKeys() { k.Desc = true }`},
		{"read through an alias", "keys := plan.GetSortKeys()\n_ = keys[0].Desc"},
		{"alias rebound before the write", "keys := plan.GetSortKeys()\nkeys = other\nkeys[0].Desc = true"},
		{"local from an unwatched source", "keys := thing.Whatever()\nkeys[0] = 1"},
		// A range variable's scope ends with the loop, so a later local reusing the
		// name is a DIFFERENT variable. Reporting it is a false positive, and on a
		// ratchet at zero a false positive blocks legitimate work.
		{"name reused after a range loop", "for _, v := range plan.GetInValues() { _ = v }\nv := other()\nv[0] = 1"},
		{"name reassigned after a range loop", "var v []int\nfor _, v := range plan.GetInValues() { _ = v }\nv = other()\nv[0] = 1"},
		// Reading the getter as a non-first argument is not a mutation.
		{"getter as copy source", "copy(dst, plan.GetInValues())"},
		{"getter inside len", "n := len(plan.GetInValues())\n_ = n"},
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

// TestLiveGetterGateScansPackageLevelClosures pins that a function literal assigned
// at package level is scanned at all.
//
// `var mutate = func() { … }` is a GenDecl, not a FuncDecl, so a walk over
// `f.Decls` looking only for FuncDecl skipped its body entirely — defeating not just
// the taint layer but the original direct-getter detection, which is the oldest and
// most basic thing this gate does. It needs its own test because every other arm
// goes through a snippet wrapper that puts the body inside a FuncDecl, so none of
// them could ever have exercised this path.
func TestLiveGetterGateScansPackageLevelClosures(t *testing.T) {
	t.Parallel()

	const src = `package p

var mutate = func() { plan.GetSortKeys()[0].Desc = true }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got []string
	scanLiveGetterWrites(f, func(_ token.Pos, getter string) { got = append(got, getter) })
	if len(got) == 0 {
		t.Error("a write inside a package-level closure was not reported; the walker only " +
			"descended FuncDecl bodies, so this shape bypassed even the direct-getter check")
	}
}
