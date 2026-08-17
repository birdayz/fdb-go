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

// A method that writes its own receiver rewrites the identity of an object the
// memo may already hold.
//
// The types this gate watches are the ones whose identity the Cascades memo
// interns: they define `structuralKey()`, `HashCodeWithoutChildren()` or
// `EqualsWithoutChildren()`. For those, a field is not private state — it is a
// component of the key the memo deduplicates on. Writing one in place does not
// produce a modified plan; it produces a plan that no longer matches the entry
// filed under its old key, while every reference already handed out still
// points at it.
//
// # Why this became lethal rather than merely fragile
//
// The structural-hash memo caches a hash per object and validates the cache by
// comparing the recorded OWNER against the plan asking. That check catches the
// case it was built for — a `cp := *p` copy inheriting the shared cell — because
// the copy is a different pointer.
//
// It cannot catch this one. An in-place write leaves the pointer identical, so
// the owner check passes and the pre-mutation hash is served for post-mutation
// content. The failure is silent and it is a wrong ANSWER: two different plans
// intern as one and whichever arrived first is returned. Identity checks cannot
// see content changes, by construction, so no refinement of the owner check
// would help — the write has to not happen.
//
// # Why a gate and not a convention
//
// The tree reached zero here by conversion, not by design: three in-place
// setters (`SetInValues`, `SetSourceKind`, `SetInSources`) survived because
// every one of their call sites happened to construct, set, then yield. That
// ordering was never stated anywhere and nothing tested it. It was correct by
// accident at five call sites, and the accident held right up until the memo
// turned the shape from latent into live.
//
// A convention that is true by accident is indistinguishable from one that is
// enforced, until it isn't. So the rule is structural: on a memo-identity type,
// a method returns a copy or it returns nothing.
//
//	cp := *p
//	cp.field = v
//	return &cp
//
// which is the same form `copy_method_rebuild_test.go` pushes its sibling shape
// toward — and note the division of labour between the two gates. That one
// polices HOW a copy is built (struct copy, never a field-by-field literal, so
// later fields are not dropped). This one polices THAT a copy is built at all.
// The prescribed form satisfies both; each gate is silent on the other's defect.
//
// # Scope
//
// Non-test, non-generated, git-tracked sources. Test files are excluded for the
// same reason the sibling gate excludes them: an unexported field is
// unreachable from outside the package, and an in-package test that mutates a
// fixture it just built is not the shipped defect. `.claude/worktrees/` is
// excluded by `trackedGoFiles` being git-scoped — an early draft of this census
// walked the filesystem instead and reported 286 findings out of stale agent
// worktrees carrying pre-conversion code.

// identityMethodNames are the methods whose presence means "this type's fields
// are memo identity". Membership is derived from the tree rather than listed,
// so a plan type added later is covered without anyone editing this file.
func isIdentityMethodName(name string) bool {
	switch name {
	case "structuralKey", "HashCodeWithoutChildren", "EqualsWithoutChildren":
		return true
	}
	return false
}

// scanIdentityTypes reports every type in f that defines an identity method.
func scanIdentityTypes(f *ast.File, add func(typeName string)) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		if !isIdentityMethodName(fn.Name.Name) {
			continue
		}
		if recv := receiverTypeName(fn.Recv.List[0].Type); recv != "" {
			add(recv)
		}
	}
}

// rootReceiverIdent walks an assignment target down to the identifier it is
// ultimately rooted at, descending through SELECTORS as well as pointers,
// indices and parens.
//
// The selector case is the one that matters and the one a first draft omitted:
// `p.reverse = true` is a SelectorExpr, so a walker that stops at unknown node
// types returns "" and the gate reports clean over the exact defect it exists
// to find. That draft was caught by injecting a real receiver write into a real
// plan file and observing the census still print zero — which is why the
// detector below pins the plain-selector arm first.
//
// Descending through indices is deliberate too: `p.legs[i] = x` mutates state
// the plan owns and is in its key, so it is the same defect wearing an index.
func rootReceiverIdent(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.SelectorExpr:
			e = t.X
		case *ast.StarExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// scanReceiverWrites reports every assignment whose target is rooted at the
// receiver, for methods on a type in keyed.
//
// A method CALL through the receiver (`b.hashMemo.state.Store(h)`) is not an
// assignment and does not report — correctly: the memo cell is reached through
// a pointer that is itself written exactly once, in the constructor, so the
// plan's own fields never change. Only assignment rewrites identity.
func scanReceiverWrites(f *ast.File, keyed map[string]bool, report func(pos token.Pos, recvType, method string)) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
			continue
		}
		recvType := receiverTypeName(fn.Recv.List[0].Type)
		if recvType == "" || !keyed[recvType] {
			continue
		}
		names := fn.Recv.List[0].Names
		// An unnamed or blank receiver cannot be written to at all.
		if len(names) == 0 || names[0].Name == "_" || names[0].Name == "" {
			continue
		}
		recvName := names[0].Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
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
				if rootReceiverIdent(target) == recvName {
					report(target.Pos(), recvType, fn.Name.Name)
				}
			}
			return true
		})
	}
}

// identityTypeFloor guards the population, and its DIRECTION is the opposite of
// the findings check below.
//
// The findings count is watched for GROWTH: one is a defect. The type count is
// watched for COLLAPSE: if `structuralKey` is renamed, or the plans package is
// restructured so the identity methods move behind an embedded helper, `keyed`
// empties out and this gate reports zero findings over zero types — passing
// loudly while checking nothing. That is the failure mode a green cannot
// distinguish from success, so it gets its own assertion.
//
// 67 types satisfy the predicate at the time of writing; the floor sits below
// that with room for ordinary churn, because its job is to catch the instrument
// dying rather than to pin an exact census.
const identityTypeFloor = 40

func collectIdentityCensus(t *testing.T) (keyed map[string]bool, findings []string) {
	t.Helper()

	root := sourceTreeRoot(t)
	files := trackedGoFiles(t, root)

	type parsedFile struct {
		rel  string
		fset *token.FileSet
		ast  *ast.File
	}
	var parsed []parsedFile
	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "gen/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // not this gate's job to police syntax
		}
		parsed = append(parsed, parsedFile{rel: rel, fset: fset, ast: f})
	}

	keyed = map[string]bool{}
	for _, pf := range parsed {
		scanIdentityTypes(pf.ast, func(name string) { keyed[name] = true })
	}
	for _, pf := range parsed {
		scanReceiverWrites(pf.ast, keyed, func(pos token.Pos, recvType, method string) {
			findings = append(findings, fmt.Sprintf("%s:%d: %s.%s writes its own receiver",
				pf.rel, pf.fset.Position(pos).Line, recvType, method))
		})
	}
	sort.Strings(findings)
	return keyed, findings
}

// TestMemoIdentityTypesNeverWriteTheirReceiver is the ratchet. Zero, no
// allowlist: every instance found so far converted to a copying builder with
// identical semantics at every call site, and an exceptions list is how the
// class regrows one "just this one" at a time.
func TestMemoIdentityTypesNeverWriteTheirReceiver(t *testing.T) {
	t.Parallel()

	keyed, findings := collectIdentityCensus(t)

	if len(keyed) < identityTypeFloor {
		t.Fatalf("only %d types define an identity method (floor %d).\n"+
			"This gate scans METHODS ON THOSE TYPES, so a shrunken population means it is\n"+
			"checking almost nothing while still reporting green. Either the identity\n"+
			"methods were renamed (update isIdentityMethodName) or the population really\n"+
			"shrank (lower the floor deliberately, in the same change).",
			len(keyed), identityTypeFloor)
	}

	if len(findings) > 0 {
		t.Errorf("%d method(s) on memo-identity types write their own receiver.\n"+
			"An in-place write leaves the pointer identical, so the structural-hash memo's\n"+
			"owner check passes and serves the PRE-mutation hash for POST-mutation content:\n"+
			"two different plans intern as one and the first one filed wins. The owner check\n"+
			"compares identity, not content, so it cannot ever catch this.\n"+
			"Return a copy instead:\n"+
			"\tcp := *p\n\tcp.<field> = <new>\n\treturn &cp\n\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}
