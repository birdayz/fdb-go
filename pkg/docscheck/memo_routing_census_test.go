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

// The structural-hash memo is wired into each plan type BY HAND: every
// `HashCodeWithoutChildren` opens with a cache read and closes with a store. There
// are 42 of them, they were applied in one sweep, and nothing held them there.
//
// A plan type added later, or one whose routing a refactor drops, goes silently
// dark. Silent is the operative word — an unrouted type returns a correct hash,
// answers nothing wrongly, and costs only time, so no test fails and no result
// changes. It is a pure performance regression wearing a green suite, which is the
// shape that survives longest.
//
// The memo's own unit tests cannot cover this: they drive ONE plan type
// (`RecordQueryIndexPlan`), because constructing fixtures for 42 is not the point
// of a correctness test. So the routing needs a structural check rather than a
// behavioural one, and it is the same shape as the receiver-write ratchet next
// door.
//
// # Scope is derived, not hardcoded
//
// The population is "every `HashCodeWithoutChildren` in the package that DEFINES
// `cachedStructuralHash`". Naming the directory would be one more thing to update
// when the memo moves; deriving it means the gate follows the mechanism. It also
// keeps the gate off the identity-bearing types in other packages, which have no
// memo cell and must not be required to use one.
//
// # Why the owner argument is checked too
//
// `p.cachedStructuralHash(p)` reads the cache only when the recorded owner is this
// exact plan. Passing anything else — a child, a quantifier, a freshly built
// sibling — compiles, and produces either a permanent miss (harmless, silent) or a
// hit belonging to another object (a wrong identity, silent). Both failure modes
// are invisible at runtime, so the argument is worth pinning at the same moment the
// call is.

// memoHelperDefiner is the function whose defining package sets the scope.
const memoHelperDefiner = "cachedStructuralHash"

const (
	memoReadHelper  = "cachedStructuralHash"
	memoWriteHelper = "storeStructuralHash"
	memoRoutedName  = "HashCodeWithoutChildren"
)

// memoRoutingFinding is one unrouted or mis-routed method.
type memoRoutingFinding struct {
	line   int
	recv   string
	reason string
}

// scanMemoRouting reports every HashCodeWithoutChildren in f that does not read AND
// write the memo with its own receiver as the owner.
func scanMemoRouting(f *ast.File, report func(pos token.Pos, recv, reason string)) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
			continue
		}
		if fn.Name.Name != memoRoutedName {
			continue
		}
		recv := receiverTypeName(fn.Recv.List[0].Type)
		if recv == "" {
			continue
		}
		recvName := ""
		if names := fn.Recv.List[0].Names; len(names) > 0 {
			recvName = names[0].Name
		}

		readsOwner, writesOwner := false, false
		var wrongOwner []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			var isRead bool
			switch sel.Sel.Name {
			case memoReadHelper:
				isRead = true
			case memoWriteHelper:
			default:
				return true
			}
			// The owner is the first argument in both helpers.
			ownerOK := false
			if len(call.Args) > 0 {
				if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == recvName && recvName != "" {
					ownerOK = true
				}
			}
			if !ownerOK {
				wrongOwner = append(wrongOwner, sel.Sel.Name)
				return true
			}
			if isRead {
				readsOwner = true
			} else {
				writesOwner = true
			}
			return true
		})

		switch {
		case len(wrongOwner) > 0:
			report(fn.Pos(), recv, fmt.Sprintf(
				"passes an owner other than the receiver %q to %s",
				recvName, strings.Join(wrongOwner, ", ")))
		case !readsOwner && !writesOwner:
			report(fn.Pos(), recv, "does not use the structural-hash memo at all; it "+
				"recomputes the key on every comparison")
		case !readsOwner:
			report(fn.Pos(), recv, "stores into the memo but never reads it, so it pays the "+
				"cache's cost and takes none of its benefit")
		case !writesOwner:
			report(fn.Pos(), recv, "reads the memo but never populates it, so the cache can "+
				"never warm up")
		}
	}
}

// memoScopeDir returns the directory of the package that defines the memo helpers,
// so the population follows the mechanism instead of a written-down path.
func memoScopeDir(t *testing.T, root string, files []string) string {
	t.Helper()
	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == memoHelperDefiner && fn.Recv != nil {
				return filepath.Dir(rel)
			}
		}
	}
	t.Fatalf("no package defines %s; the routing gate has no scope and would pass over "+
		"an empty set. Either the memo was removed (delete this gate in the same change) "+
		"or the helper was renamed (update memoHelperDefiner).", memoHelperDefiner)
	return ""
}

// memoRoutingFloor guards the population against COLLAPSE. The findings count below
// is watched for GROWTH — one is a regression — but this number is watched for
// shrinkage: if `HashCodeWithoutChildren` is renamed, or the plans move, the scan
// finds nothing and reports a clean zero over zero methods. 42 route at the time of
// writing; the floor sits below with room for churn.
const memoRoutingFloor = 35

func collectMemoRouting(t *testing.T) (methods int, findings []string) {
	t.Helper()

	root := sourceTreeRoot(t)
	files := trackedGoFiles(t, root)
	scope := memoScopeDir(t, root, files)

	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") || filepath.Dir(rel) != scope {
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
			if ok && fn.Recv != nil && len(fn.Recv.List) > 0 && fn.Name.Name == memoRoutedName {
				methods++
			}
		}
		scanMemoRouting(f, func(pos token.Pos, recv, reason string) {
			findings = append(findings, fmt.Sprintf("%s:%d: %s.%s %s",
				rel, fset.Position(pos).Line, recv, memoRoutedName, reason))
		})
	}
	sort.Strings(findings)
	return methods, findings
}

// TestEveryPlanRoutesItsStructuralHashThroughTheMemo is the ratchet: 42 of 42, no
// allowlist. A plan type that genuinely must not memoize does not exist — the cell
// is on the shared base and the owner check makes participation free of coupling —
// so an exception here would mean the mechanism is wrong, not that this type is
// special.
func TestEveryPlanRoutesItsStructuralHashThroughTheMemo(t *testing.T) {
	t.Parallel()

	methods, findings := collectMemoRouting(t)

	if methods < memoRoutingFloor {
		t.Fatalf("only %d %s methods found (floor %d). The gate scans those methods, so a "+
			"shrunken population means it is checking almost nothing while reporting green.",
			methods, memoRoutedName, memoRoutingFloor)
	}

	if len(findings) > 0 {
		t.Errorf("%d of %d plan types do not route %s through the structural-hash memo.\n"+
			"An unrouted type rebuilds its whole structural key on every comparison, and the\n"+
			"memo's insert path calls this once per member per insert. It returns a CORRECT\n"+
			"hash, so nothing fails and no result changes — it is a pure performance\n"+
			"regression that a green suite cannot see.\n"+
			"The form is:\n"+
			"\tif hash, ok := p.cachedStructuralHash(p); ok {\n\t\treturn hash\n\t}\n"+
			"\thash := p.structuralKey().Hash(\"...|\")\n\tp.storeStructuralHash(p, hash)\n"+
			"\treturn hash\n\n%s",
			len(findings), methods, memoRoutedName, strings.Join(findings, "\n"))
	}
}

// --- detector pins -------------------------------------------------------------
//
// The ratchet claims a zero. A detector that cannot fire makes that zero worthless,
// and this file's sibling shipped exactly that defect once already (a missing
// `ast.SelectorExpr` case made a receiver-write census report clean over a real
// injected write). So each arm is driven against synthetic source.

func scanMemoRoutingSnippet(t *testing.T, body string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", "package p\n\n"+body, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse snippet: %v\n---\n%s", err, body)
	}
	var got []string
	scanMemoRouting(f, func(_ token.Pos, recv, reason string) {
		got = append(got, recv+": "+reason)
	})
	return got
}

func TestMemoRoutingDetectorFiresOnEveryUnroutedShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want string
		body string
	}{
		{
			// The shape a newly added plan type has before anyone wires it up.
			name: "no memo at all",
			want: "does not use the structural-hash memo",
			body: `type P struct{}
func (p *P) HashCodeWithoutChildren() uint64 { return 7 }`,
		},
		{
			// A refactor that keeps the store and drops the early return: pays the
			// cost, takes no benefit. This is exactly half of the mutation that
			// proved the memo's own census blind, so it must be caught here.
			name: "stores but never reads",
			want: "never reads it",
			body: `type P struct{}
func (p *P) HashCodeWithoutChildren() uint64 { h := uint64(7); p.storeStructuralHash(p, h); return h }`,
		},
		{
			name: "reads but never stores",
			want: "never populates it",
			body: `type P struct{}
func (p *P) HashCodeWithoutChildren() uint64 {
	if h, ok := p.cachedStructuralHash(p); ok { return h }
	return 7
}`,
		},
		{
			// Compiles, and is silent both ways: a permanent miss, or a hit
			// belonging to another object.
			name: "wrong owner passed to the read",
			want: "owner other than the receiver",
			body: `type P struct{ child *P }
func (p *P) HashCodeWithoutChildren() uint64 {
	if h, ok := p.cachedStructuralHash(p.child); ok { return h }
	h := uint64(7); p.storeStructuralHash(p, h); return h
}`,
		},
		{
			name: "wrong owner passed to the store",
			want: "owner other than the receiver",
			body: `type P struct{ child *P }
func (p *P) HashCodeWithoutChildren() uint64 {
	if h, ok := p.cachedStructuralHash(p); ok { return h }
	h := uint64(7); p.storeStructuralHash(p.child, h); return h
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scanMemoRoutingSnippet(t, tc.body)
			if len(got) == 0 {
				t.Fatalf("detector reported nothing; this unrouted shape would reach the tree "+
					"unnoticed:\n%s", tc.body)
			}
			if !strings.Contains(strings.Join(got, " | "), tc.want) {
				t.Errorf("reported %v, want a finding mentioning %q", got, tc.want)
			}
		})
	}
}

func TestMemoRoutingDetectorStaysSilentOnCorrectRouting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{
			// The prescribed form. If this reported there would be no legal way to
			// write the method.
			name: "canonical routing",
			body: `type P struct{}
func (p *P) HashCodeWithoutChildren() uint64 {
	if h, ok := p.cachedStructuralHash(p); ok { return h }
	h := uint64(7)
	p.storeStructuralHash(p, h)
	return h
}`,
		},
		{
			// A different receiver name must not be mistaken for a different owner.
			name: "receiver not named p",
			body: `type Plan struct{}
func (self *Plan) HashCodeWithoutChildren() uint64 {
	if h, ok := self.cachedStructuralHash(self); ok { return h }
	h := uint64(7)
	self.storeStructuralHash(self, h)
	return h
}`,
		},
		{
			// Other methods on the same type are none of this gate's business.
			name: "unrelated method without the memo",
			body: `type P struct{}
func (p *P) Explain() string { return "" }`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scanMemoRoutingSnippet(t, tc.body); len(got) > 0 {
				t.Errorf("detector reported %v on correct routing; a gate that flags the "+
					"prescribed form gets read past:\n%s", got, tc.body)
			}
		})
	}
}

// TestMemoRoutingPopulationIsRealAndAboveItsFloor is the vacuity guard, driven
// separately so a collapse is reported by a test that names the cause.
func TestMemoRoutingPopulationIsRealAndAboveItsFloor(t *testing.T) {
	t.Parallel()

	methods, _ := collectMemoRouting(t)
	if methods < memoRoutingFloor {
		t.Fatalf("%s population is %d, below the floor of %d; the routing ratchet is "+
			"scanning almost nothing", memoRoutedName, methods, memoRoutingFloor)
	}
}
