package docscheck

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The ratchet in copy_method_rebuild_test.go is a claim about what cannot
// reach the tree, and a ratchet that matches nothing is green forever while
// the class regrows underneath it. So the detector is pinned against synthetic
// source in BOTH directions:
//
//   - RECALL: every shape the class actually took must be reported. The 17
//     converted sites were not uniform — pointer and value literals, literals
//     behind an early-return guard, a generic receiver, a value receiver, and
//     four different method names. A detector tuned to `return &T{` alone
//     would have found some and silently blessed the rest.
//   - PRECISION: a copy method that builds a Quantifier, a slice, or a child
//     of a DIFFERENT type is doing ordinary work. If those report, the gate
//     becomes noise, and a noisy gate is read past — the same end state as no
//     gate, reached more expensively.
//
// The precision half matters more than it looks. The struct-copy form this
// gate pushes people toward (`cp := *e`) is only reachable if the gate stays
// silent on it; a detector that flagged the fix as well as the defect would
// leave no legal way to write the method.
func scanCopySnippet(t *testing.T, body string) []string {
	t.Helper()
	src := "package p\n\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse snippet: %v\n---\n%s", err, src)
	}
	var got []string
	scanCopyMethodRebuilds(f, func(pos token.Pos, method, recv, form string) {
		got = append(got, recv+"."+method+": "+form)
	})
	return got
}

func TestCopyRebuildDetectorFiresOnEveryShapeThatShipped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want string // substring of the reported finding
		body string
	}{
		{
			// The plain form: 15 of the 17 converted sites looked like this.
			name: "pointer literal returned directly",
			want: "E.WithQuantifiers",
			body: `type E struct{ inner Q; keep int }
func (e *E) WithQuantifiers(qs []Q) any { return &E{inner: qs[0], keep: e.keep} }`,
		},
		{
			// A value literal is the same defect; only the address-of differs.
			name: "value literal",
			want: "E.WithQuantifiers",
			body: `type E struct{ inner Q }
func (e *E) WithQuantifiers(qs []Q) any { v := E{inner: qs[0]}; return &v }`,
		},
		{
			// GroupByExpression and the plans layer guard arity first. The
			// literal is then not the first statement, so a detector that only
			// looked at the return statement would miss it.
			name: "literal behind an early-return guard",
			want: "E.WithQuantifiers",
			body: `type E struct{ inner Q; keep int }
func (e *E) WithQuantifiers(qs []Q) any {
	if len(qs) == 0 {
		return e
	}
	return &E{inner: qs[0], keep: e.keep}
}`,
		},
		{
			// BiMap.Copy: generic receiver, so the receiver type is an
			// IndexListExpr and the literal names the same base identifier.
			name: "generic receiver",
			want: "M.Copy",
			body: `type M[K any, V any] struct{ m map[string]K; f func(V) string }
func (b *M[K, V]) Copy() *M[K, V] { return &M[K, V]{m: b.m, f: b.f} }`,
		},
		{
			name: "single type parameter",
			want: "Box.Clone",
			body: `type Box[T any] struct{ v T; n int }
func (b *Box[T]) Clone() *Box[T] { return &Box[T]{v: b.v, n: b.n} }`,
		},
		{
			// Nothing forces these methods to take a pointer receiver.
			name: "value receiver",
			want: "E.Copy",
			body: `type E struct{ a int; b int }
func (e E) Copy() E { return E{a: e.a, b: e.b} }`,
		},
		{
			name: "WithChildren",
			want: "V.WithChildren",
			body: `type V struct{ K int }
func (v *V) WithChildren(c []int) *V { return &V{K: c[0]} }`,
		},
		{
			name: "Clone",
			want: "S.Clone",
			body: `type S struct{ a int }
func (s *S) Clone() *S { return &S{a: s.a} }`,
		},
		{
			name: "Duplicate",
			want: "S.Duplicate",
			body: `type S struct{ a int }
func (s *S) Duplicate() *S { return &S{a: s.a} }`,
		},
		{
			name: "Rebind",
			want: "S.Rebind",
			body: `type S struct{ a int }
func (s *S) Rebind(x int) *S { return &S{a: x} }`,
		},
		{
			name: "WithNewChildren",
			want: "S.WithNewChildren",
			body: `type S struct{ a int }
func (s *S) WithNewChildren(x int) *S { return &S{a: x} }`,
		},
		{
			// A rebuild hidden inside a closure is still a rebuild, and it is
			// the shape most likely to be reached for when someone wants the
			// literal back.
			name: "literal inside a closure",
			want: "E.Copy",
			body: `type E struct{ a int }
func (e *E) Copy() func() *E { return func() *E { return &E{a: e.a} } }`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scanCopySnippet(t, tc.body)
			if len(got) == 0 {
				t.Fatalf("detector reported NOTHING for a rebuild it must catch.\nsource:\n%s", tc.body)
			}
			for _, g := range got {
				if strings.Contains(g, tc.want) {
					return
				}
			}
			t.Fatalf("detector reported %v, want a finding containing %q\nsource:\n%s", got, tc.want, tc.body)
		})
	}
}

func TestCopyRebuildDetectorStaysSilentOnLegitimateShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{
			// THE FIX. If this reports, there is no legal way to write the
			// method and the gate is unsatisfiable.
			name: "struct copy with field override",
			body: `type E struct{ inner Q; keep int }
func (e *E) WithQuantifiers(qs []Q) any { cp := *e; cp.inner = qs[0]; return &cp }`,
		},
		{
			// A deliberate reset under the struct-copy form — the shape item 2
			// of the sweep requires for a field a copy must clear. It must be
			// as silent as the plain override, or "deliberate" is unwritable.
			name: "struct copy with a deliberate reset",
			body: `type E struct{ inner Q; cached []int }
func (e *E) WithQuantifiers(qs []Q) any {
	cp := *e
	cp.inner = qs[0]
	cp.cached = nil // deliberate: new children invalidate the derivation
	return &cp
}`,
		},
		{
			name: "returns the receiver unchanged",
			body: `type E struct{ a int }
func (e *E) WithQuantifiers(qs []Q) any { return e }`,
		},
		{
			// Building children/helpers of OTHER types is ordinary work.
			name: "composite literal of a different type",
			body: `type E struct{ inner Q }
type Q struct{ alias string }
func (e *E) WithQuantifiers(qs []Q) any { cp := *e; cp.inner = Q{alias: "x"}; return &cp }`,
		},
		{
			name: "slice and map literals",
			body: `type E struct{ xs []int; m map[string]int }
func (e *E) Copy() *E {
	cp := *e
	cp.xs = []int{1, 2, 3}
	cp.m = map[string]int{"a": 1}
	return &cp
}`,
		},
		{
			// A literal of a type from ANOTHER package cannot be the
			// receiver's own type in the receiver's own package.
			name: "qualified literal of another package's type",
			body: `type E struct{ inner any }
func (e *E) Copy() *E { cp := *e; cp.inner = other.E{}; return &cp }`,
		},
		{
			// Constructors legitimately build the type field-by-field; the
			// gate covers COPY methods, and a constructor has no prior value
			// whose fields could be dropped.
			name: "constructor is not a copy method",
			body: `type E struct{ a int; b int }
func NewE(a, b int) *E { return &E{a: a, b: b} }`,
		},
		{
			// Same-named method on an unrelated type in the same file must not
			// leak: the literal here belongs to the constructor, not the copy.
			name: "non-copy method building the type",
			body: `type E struct{ a int }
func (e *E) Reset() *E { return &E{} }`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scanCopySnippet(t, tc.body); len(got) != 0 {
				t.Fatalf("detector reported %v on a legitimate shape; a noisy gate gets read past\nsource:\n%s", got, tc.body)
			}
		})
	}
}
