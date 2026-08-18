// Package typeimmutable is a nogo analyzer that enforces the immutability
// RFC-234 relies on: a `values.Type` graph may be written only by the function
// that built it.
//
// # Why this has to be a build-time gate rather than a test
//
// `ExactTypeHandle.Type`, `QuantifiedObjectValue.FlowedType` and
// `fieldValue.Type` return the thawed graph CACHED ON AN INTERNED HANDLE. They
// used to return a fresh copy per call, and that copy was the largest single
// object allocation in the planner — 175.7M objects per sweep, of which those
// three accessors were 73%. Sharing removed 128.9M of them.
//
// What the copy bought was a permission nothing in production used: a census
// over 103 packages found 21 writes into a Type field and every one is into a
// graph its own function had just constructed. But "nothing does this today" is
// not a property, it is an observation, and the first write to a shared graph
// does not corrupt one caller's copy — it corrupts every value flowing that
// shape, process-wide, because the graph hangs off an INTERNED node. That
// failure is silent, order-dependent, and crosses parallel tests.
//
// # Why it covers tests
//
// Because that is where the mutation actually lives. Deduped by position, 60 of
// the 81 mutation sites in this repo are in `_test.go`, and the test side writes
// seven fields production never touches at all (`RecordName`, `Fields`,
// `Nullable`, `TypeCode`, `ElementType`, `InnerType`, `PrimitiveType.Nullable`).
// A gate scoped to production sources would have watched the half that was
// already clean. It is not hypothetical either: flipping the accessors with the
// old tests in place made ONE test's mutation reach TWO unrelated tests through
// the intern table.
//
// # The rule, and what it does NOT catch
//
// The not-covered list first, because a gate's scope sentence written from the
// code it just matched is how these over-claim.
//
//   - A write reached through a function CALL is invisible: `mutate(rt)` where
//     `mutate` writes `rt.Nullable` is diagnosed at `mutate`'s own body if that
//     body is in an analyzed package, and not at the call site. Interprocedural
//     provenance is not attempted.
//   - A write through a value stored in a container (`m[k].Fields[i].Name = …`
//     via a map of pointers, a slice of `*RecordType`) resolves its root to the
//     container, not to the graph, and is refused only if the container itself
//     is not local — which is stricter than the truth, not looser.
//   - Reflection, `unsafe`, and `encoding/gob`-style rehydration are not
//     modelled at all.
//   - Generated code excluded via nogo_config, as for every analyzer here.
//
// What it DOES catch, by shape:
//
//  1. a write to a field of a Type reached from a PARAMETER, a call result, a
//     receiver's field, or a package-level var — anything the function did not
//     build;
//  2. a write THROUGH a reference (a slice index or a pointer deref) where the
//     reference was not itself locally constructed — which is the shallow
//     copy-on-write hazard: `withLegs := *record` copies the struct header and
//     SHARES `Fields`, so `withLegs.Fields[i].Name = …` writes the interned
//     graph while looking local;
//  3. `++`/`--`, which is `*ast.IncDecStmt` and not an assignment;
//  4. `&t.Field`, which hands a writable pointer out of the function.
//
// A write to a direct field of a locally-copied struct (`withLegs.Legs = legs`)
// is ALLOWED: that writes the copy's own header and is the sanctioned
// copy-on-write form.
package typeimmutable

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// valuesPkgSuffix identifies the package that owns the Type hierarchy. Matched
// as a suffix so a vendored or relocated module root does not silently disable
// the gate — a path check that fails OPEN is the shape this repo keeps naming.
const valuesPkgSuffix = "recordlayer/query/plan/cascades/values"

// guardedTypes are the structs whose fields carry a Type's identity or shape.
// Field and EnumValue are value-typed MEMBERS of a RecordType/EnumType: a write
// into one reached through a Type's slice mutates the Type just as surely as a
// write to the Type's own field.
var guardedTypes = map[string]bool{
	"PrimitiveType": true,
	"RecordType":    true,
	"ArrayType":     true,
	"EnumType":      true,
	"RelationType":  true,
	"anyRecordType": true,
	"Field":         true,
	"EnumValue":     true,
}

// Allowlist maps a repo-relative file path (matched as a path suffix) to the
// function names within it that are permitted to write a Type they did not
// build. Every entry is a deliberate contract that must be stated at the site.
var Allowlist = map[string]map[string]string{
	"pkg/recordlayer/query/plan/cascades/values/qov_source_layout.go": {
		// restoreQOVRecordLayout writes `record.Legs` on a PARAMETER and recurses
		// into `record.Fields[i].FieldType`. It is safe only because its single
		// EXTERNAL caller (physicalFlowedRecordType) passes a graph from thaw()
		// rather than thawShared() — the recursion inherits that provenance. That
		// is a one-caller invariant; if a second caller appears, this entry is the
		// thing that has to be re-justified.
		"restoreQOVRecordLayout": "writes a parameter; sole external caller passes a private thaw()",
	},
	"pkg/recordlayer/query/plan/cascades/values/ordinal_layout_test.go": {
		// This test's whole subject is that a layout SNAPSHOTS its inputs: it hands
		// the constructor a set of graphs, then mutates every one of them, and
		// requires the layout to be unmoved. The mutation is the measurement. The
		// graphs come from a fixture helper that builds them fresh, which the
		// analyzer cannot see through — and the test is not rewritten to build them
		// inline because the fixture is shared with the sibling assertions that
		// need the SAME graphs.
		//
		// Note what this entry does NOT cover: the same test's getter half was
		// rewritten rather than allowlisted, because those graphs come from
		// FlowedType and are genuinely shared now. Input mutation is a contract;
		// getter mutation was a bug waiting for RFC-234 to arm it.
		"TestOrdinalLayoutSnapshotsEveryMutableInputAndGetter": "mutates its own fixture inputs to prove the snapshot is isolated",
	},
	"pkg/recordlayer/query/plan/cascades/plan_leg_concat_layout_test.go": {
		// The mutation IS the measurement here, in the strongest form: this test
		// exists to prove PlanLegConcatLayout does NOT hand out the plan's own
		// Fields slice, and the only way to observe that is to write to what it
		// returned and check the plan is unmoved. Removing the write removes the
		// test.
		//
		// It is the shape this analyzer refuses everywhere else, which is the
		// reason it is allowlisted BY NAME rather than by relaxing a rule: a
		// blanket exemption for "writes to a call result inside a test" would have
		// covered the two genuinely latent accessor mutations this gate found.
		"TestPlanLegConcatLayout_ReturnsADefensiveCopy": "writes the returned layout to prove it is a copy; the write is the assertion",
	},
	"pkg/relational/core/query/inline_values_translation_test.go": {
		// This test SIMULATES external drift: it mutates the collection's ordinary
		// Type graph after construction and requires the translation to DECLINE,
		// because the logical leaf holds a frozen schema snapshot that the drifted
		// graph no longer matches. The mutation is the input to the behaviour under
		// test, not a violation of the invariant.
		//
		// It is outside RFC-234's sharing: ArrayConstructorValue.ElementType is an
		// ordinary graph a logical operator carries, with no interned handle behind
		// it. If that ever changes, this entry is what has to be re-justified.
		"TestInlineValuesTranslationRejectsCollectionTypeDrift": "mutates a collection graph to simulate drift; declining is the assertion",
	},
	"pkg/recordlayer/query/plan/cascades/values/proto_field_exact_type_test.go": {
		// This test exists to VERIFY that FieldTypeForProtoField hands back an
		// independent graph on every call, so mutating a returned graph is the
		// measurement rather than a violation of it. FieldTypeForProtoField
		// derives from a protobuf descriptor and holds no interned handle, so it
		// is outside RFC-234's sharing entirely — and if that ever changes, this
		// test is the thing that must fail, which is why it is allowlisted rather
		// than rewritten to stop mutating.
		"TestFieldTypeForProtoFieldReturnsIndependentTypeGraphs": "asserts the callee's freshness contract; the mutation IS the test",
	},
}

// Analyzer is the nogo entry point.
var Analyzer = newAnalyzer(Allowlist)

func newAnalyzer(allow map[string]map[string]string) *analysis.Analyzer {
	r := &runner{allow: allow}
	return &analysis.Analyzer{
		Name: "typeimmutable",
		Doc:  "flags writes into a values.Type graph the writing function did not construct (RFC-234)",
		Run:  r.run,
	}
}

type runner struct {
	allow map[string]map[string]string
}

func (r *runner) run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		path := pass.Fset.Position(file.Pos()).Filename
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			r.checkFunc(pass, path, fn)
			return true
		})
	}
	return nil, nil
}

// checkFunc walks one function, first recording which locals it CONSTRUCTS and
// then checking every write. Construction is collected first because Go permits
// a write textually before the construction it depends on only through control
// flow this analyzer does not model; collecting first is the permissive
// direction and keeps the gate from reddening on loop-carried builders.
func (r *runner) checkFunc(pass *analysis.Pass, path string, fn *ast.FuncDecl) {
	built := constructedLocals(fn)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		var targets []ast.Expr
		var shape string
		switch s := n.(type) {
		case *ast.AssignStmt:
			targets, shape = s.Lhs, "assigned"
		case *ast.IncDecStmt:
			targets, shape = []ast.Expr{s.X}, "incremented"
		case *ast.UnaryExpr:
			if s.Op != token.AND {
				return true
			}
			targets, shape = []ast.Expr{s.X}, "had its address taken"
		default:
			return true
		}
		for _, target := range targets {
			sel, ok := target.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			owner, isGuarded := guardedOwner(pass, sel)
			if !isGuarded {
				continue
			}
			root, viaReference, rootExpr, resolved := rootOfChain(sel.X)
			if !resolved {
				continue // an expression this analyzer cannot attribute; see the not-covered list
			}
			// A POINTER-typed root is itself a reference step, even with no index
			// or deref written out: `p.Field = x` on a `p *RecordType` writes
			// through to whatever p points at. Without this, a graph taken from an
			// accessor and written directly — `r := v.ResultType().(*PrimitiveType);
			// r.TypeCode = …` — reads as a local write and passes. That shape is
			// live in this repo's tests and is precisely what the gate is for.
			if isPointer(pass, rootExpr) {
				viaReference = true
			}
			how, local := built[root]
			switch {
			case !local:
				r.report(pass, path, fn, sel, owner, shape,
					"the graph was not constructed here (`"+root+"` is a parameter, a call "+
						"result, a receiver field, or a package-level value)")
			case viaReference && !how.freshReference:
				r.report(pass, path, fn, sel, owner, shape,
					"the write goes THROUGH a reference (a slice index or a pointer deref) that "+
						"`"+root+"` is not known to own. `"+root+"` is defined here, but not by a "+
						"form that allocates — a shallow copy (`x := *p`) aliases its source's "+
						"slices, an `append` onto an existing slice may reuse its array, and a "+
						"value from a call carries whatever provenance the callee had")
			}
		}
		return true
	})
}

func (r *runner) report(pass *analysis.Pass, path string, fn *ast.FuncDecl, sel *ast.SelectorExpr, owner, shape, why string) {
	if fns, ok := r.allow[matchedAllowKey(r.allow, path)]; ok {
		if _, permitted := fns[fn.Name.Name]; permitted {
			return
		}
	}
	pass.Reportf(sel.Pos(),
		"%s.%s is %s here, but %s. A values.Type graph is SHARED — it is cached on an "+
			"interned handle, so this does not modify a private copy, it modifies every value "+
			"flowing that shape. Build the graph you intend to write (RFC-234); if this write "+
			"is a deliberate contract, add %s to typeimmutable.Allowlist and state the "+
			"invariant at the site.",
		owner, sel.Sel.Name, shape, why, fn.Name.Name)
}

func matchedAllowKey(allow map[string]map[string]string, path string) string {
	for key := range allow {
		if strings.HasSuffix(path, key) {
			return key
		}
	}
	return ""
}

// guardedOwner reports the guarded struct name behind a selector, if any.
func guardedOwner(pass *analysis.Pass, sel *ast.SelectorExpr) (string, bool) {
	tv, ok := pass.TypesInfo.Types[sel.X]
	if !ok {
		return "", false
	}
	owner := tv.Type
	if ptr, isPtr := owner.(*types.Pointer); isPtr {
		owner = ptr.Elem()
	}
	named, isNamed := owner.(*types.Named)
	if !isNamed {
		return "", false
	}
	obj := named.Obj()
	if obj.Pkg() == nil || !strings.HasSuffix(obj.Pkg().Path(), valuesPkgSuffix) {
		return "", false
	}
	if !guardedTypes[obj.Name()] {
		return "", false
	}
	return obj.Name(), true
}

// provenance records how a local came to exist.
type provenance struct {
	// freshReference is true when the local IS a reference (slice/pointer) that
	// this function allocated, so writing through it cannot reach anything else.
	freshReference bool
}

// rootOfChain walks a selector chain inward to its root identifier, reporting
// whether the walk passed through a REFERENCE step — an index or a pointer
// dereference — which is what distinguishes writing a local copy's own field
// from writing through a slice the copy merely shares.
func rootOfChain(expr ast.Expr) (root string, viaReference bool, rootExpr ast.Expr, resolved bool) {
	for {
		switch e := expr.(type) {
		case *ast.Ident:
			return e.Name, viaReference, e, true
		case *ast.SelectorExpr:
			expr = e.X
		case *ast.IndexExpr:
			viaReference = true
			expr = e.X
		case *ast.StarExpr:
			viaReference = true
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.TypeAssertExpr:
			expr = e.X
		default:
			return "", viaReference, nil, false
		}
	}
}

// isPointer reports whether an expression's static type is a pointer, which
// makes a field write on it a write THROUGH a reference rather than to a local
// copy. Unknown types answer true: the analyzer would rather over-report a shape
// it cannot classify than let the write it exists to catch pass as local.
func isPointer(pass *analysis.Pass, expr ast.Expr) bool {
	tv, ok := pass.TypesInfo.Types[expr]
	if !ok {
		return true
	}
	_, isPtr := tv.Type.Underlying().(*types.Pointer)
	return isPtr
}

// constructedLocals collects the names this function BUILDS, with enough detail
// to tell a freshly allocated slice from a shallow struct copy.
func constructedLocals(fn *ast.FuncDecl) map[string]provenance {
	built := map[string]provenance{}
	// Closures declared in this same function are resolved, because a test
	// fixture builder three lines above the write is not the "provenance from
	// another package" case the gate is about — its body is right there. This is
	// the ONLY interprocedural step taken, and it is bounded to a func literal
	// bound to a local name in this function.
	localBuilders := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, isIdent := as.Lhs[0].(*ast.Ident)
		lit, isFunc := as.Rhs[0].(*ast.FuncLit)
		if isIdent && isFunc && returnsAFreshReference(lit.Body) {
			localBuilders[id.Name] = true
		}
		return true
	})
	freshCall := func(rhs ast.Expr) bool {
		call, ok := rhs.(*ast.CallExpr)
		return ok && localBuilders[calleeName(call.Fun)]
	}
	// A range variable is a COPY of the element, so writing its fields cannot
	// reach the container. The container itself is not thereby local.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.RangeStmt:
			if id, ok := s.Value.(*ast.Ident); ok && id.Name != "_" {
				built[id.Name] = provenance{}
			}
		case *ast.DeclStmt:
			gd, ok := s.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					var rhs ast.Expr
					if i < len(vs.Values) {
						rhs = vs.Values[i]
					}
					built[name.Name] = provenanceOf(rhs)
				}
			}
		case *ast.AssignStmt:
			if s.Tok != token.DEFINE {
				return true
			}
			for i, l := range s.Lhs {
				id, ok := l.(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}
				var rhs ast.Expr
				if len(s.Rhs) == len(s.Lhs) {
					rhs = s.Rhs[i]
				} else if len(s.Rhs) == 1 {
					// A multi-value call: nothing here is constructed by this
					// function, so it is deliberately NOT recorded as built.
					continue
				}
				if freshCall(rhs) {
					built[id.Name] = provenance{freshReference: true}
					continue
				}
				built[id.Name] = provenanceOf(rhs)
			}
		}
		return true
	})
	return built
}

// returnsAFreshReference reports whether every return in a closure body hands
// back something this function allocated. EVERY return, not any: a builder with
// one allocating path and one that forwards a caller's slice is not a builder.
func returnsAFreshReference(body *ast.BlockStmt) bool {
	returns, fresh := 0, 0
	ast.Inspect(body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return true
		}
		returns++
		if provenanceOf(ret.Results[0]).freshReference {
			fresh++
			return true
		}
		// A closure that builds into a local and returns it: resolve that local
		// within the closure, one level, which is the shape of every fixture
		// builder in this repo (`x := make(...); copy(x, src); return x`).
		if id, isIdent := ret.Results[0].(*ast.Ident); isIdent {
			inner := constructedLocalsIn(body)
			if inner[id.Name].freshReference {
				fresh++
			}
		}
		return true
	})
	return returns > 0 && returns == fresh
}

// constructedLocalsIn is constructedLocals over a bare block, for the closure
// case. It deliberately does NOT recurse into further closures: one level is
// what the fixture shape needs, and an unbounded walk would quietly become the
// interprocedural analysis this gate declines to do.
func constructedLocalsIn(body *ast.BlockStmt) map[string]provenance {
	built := map[string]provenance{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE {
			return true
		}
		for i, l := range as.Lhs {
			id, isIdent := l.(*ast.Ident)
			if !isIdent || id.Name == "_" || len(as.Rhs) != len(as.Lhs) {
				continue
			}
			built[id.Name] = provenanceOf(as.Rhs[i])
		}
		return true
	})
	return built
}

// provenanceOf classifies a right-hand side. Anything it does not recognise is
// still recorded as local — the name IS defined here — but without
// freshReference, so a write through it is refused. That is the safe direction:
// an unrecognised builder over-reports rather than passing silently.
func provenanceOf(rhs ast.Expr) provenance {
	switch e := rhs.(type) {
	case nil:
		// `var x T` — a zero value this function owns outright.
		return provenance{freshReference: true}
	case *ast.CompositeLit:
		return provenance{freshReference: true}
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return provenanceOf(e.X)
		}
	case *ast.CallExpr:
		if freshTypeConstructors[calleeName(e.Fun)] {
			return provenance{freshReference: true}
		}
		if id, ok := e.Fun.(*ast.Ident); ok {
			switch id.Name {
			case "make":
				return provenance{freshReference: true}
			case "append":
				// ONLY the copy idioms. `append([]T(nil), src…)` and
				// `append([]T{}, …)` allocate a new backing array, so writing
				// through the result reaches nothing else. `append(rt.Fields, x)`
				// may reuse `rt.Fields`' array and a write to element 0 would land
				// in the Type — treating that as fresh would put the analyzer's
				// central hazard on the ALLOWED side, which is worse than having no
				// analyzer, because it would read as checked.
				if len(e.Args) > 0 && allocatesFreshSlice(e.Args[0]) {
					return provenance{freshReference: true}
				}
				return provenance{}
			}
		}
	case *ast.StarExpr:
		// `x := *p` — a SHALLOW copy. Its own fields are private; its slices are
		// not. This is the distinction the whole analyzer exists to draw.
		return provenance{freshReference: false}
	}
	return provenance{}
}

// allocatesFreshSlice reports whether an append's FIRST argument guarantees a
// new backing array: a nil conversion (`[]T(nil)`) or an empty slice literal
// (`[]T{}`). Anything else may share the array it was given.
func allocatesFreshSlice(arg ast.Expr) bool {
	switch a := arg.(type) {
	case *ast.CompositeLit:
		_, isSlice := a.Type.(*ast.ArrayType)
		return isSlice && len(a.Elts) == 0
	case *ast.CallExpr:
		// A conversion `[]T(nil)` parses as a call whose Fun is the slice type.
		if _, isSlice := a.Fun.(*ast.ArrayType); !isSlice {
			return false
		}
		if len(a.Args) != 1 {
			return false
		}
		id, ok := a.Args[0].(*ast.Ident)
		return ok && id.Name == "nil"
	}
	return false
}

// freshTypeConstructors are the package `values` functions that ALLOCATE a Type
// graph and return it, so a caller owns what comes back and may write to it.
//
// It is an explicit list and not a naming convention, because the distinction it
// draws is the entire gate. These allocate:
//
//	NewRecordType, NewPrimitiveType, NewArrayType, NewEnumType,
//	NewRelationType, NewAnyRecordType
//
// each of whose body is a single composite-literal return — verified by
// TestFreshConstructorsReallyAllocate, which parses them rather than trusting
// this comment.
//
// These do NOT, and must never be added: Type(), FlowedType(), ResultType(),
// FieldType(), SharedFlowedType, SharedExactType. They hand back the graph
// cached on an INTERNED handle. A caller writing to one of those does not
// modify a private copy — it modifies every value in the process that flows that
// shape. That is the failure RFC-234 traded the defensive copy for, and the
// reason the list is spelled out here rather than derived from a `New` prefix:
// a prefix rule would admit a future `NewSharedX` on sight.
var freshTypeConstructors = map[string]bool{
	"NewRecordType":    true,
	"NewPrimitiveType": true,
	"NewArrayType":     true,
	"NewEnumType":      true,
	"NewRelationType":  true,
	"NewAnyRecordType": true,
}

// calleeName renders a call's function as a bare identifier, so `NewRecordType`
// and `values.NewRecordType` both resolve to the same key.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
