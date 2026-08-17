package docscheck

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The ratchet in receiver_write_census_test.go is a claim about what cannot
// reach the tree, and its whole value rests on the detector being able to fire.
// That is not hypothetical here: the FIRST draft of `rootReceiverIdent` did not
// descend through `*ast.SelectorExpr`, so `p.field = v` — the plain, dominant
// shape, and the exact form of all three converted setters — resolved to "" and
// matched nothing. Injecting a real receiver write into a real plan file made
// the census print `0`, unchanged. A gate reporting zero over a live defect is
// strictly worse than no gate, because it is read as coverage.
//
// So the detector is pinned in both directions against synthetic source:
//
//   - RECALL: every way an assignment can be rooted at the receiver must
//     report. Selector, whole-receiver deref, nested selector, index,
//     increment, compound-assign, and the value-receiver variant.
//   - PRECISION: the prescribed fix (`cp := *p`) must stay silent, or the gate
//     leaves no legal way to write a builder and gets read past — the same end
//     state as no gate, reached more expensively.

func scanReceiverWriteSnippet(t *testing.T, keyed []string, body string) []string {
	t.Helper()
	src := "package p\n\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse snippet: %v\n---\n%s", err, src)
	}
	keySet := map[string]bool{}
	for _, k := range keyed {
		keySet[k] = true
	}
	var got []string
	scanReceiverWrites(f, keySet, func(_ token.Pos, recvType, method string) {
		got = append(got, recvType+"."+method)
	})
	return got
}

func TestReceiverWriteDetectorFiresOnEveryRootingShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want string
		body string
	}{
		{
			// The shape the first draft missed, and the shape all three
			// converted setters had. This arm is the reason the file exists.
			name: "plain selector",
			want: "P.SetX",
			body: `type P struct{ x int }
func (p *P) SetX(v int) { p.x = v }`,
		},
		{
			// Overwriting the whole receiver: maximal identity rewrite, and it
			// names no field at all, so a field-name-based detector misses it.
			name: "whole-receiver deref",
			want: "P.Reset",
			body: `type P struct{ x int }
func (p *P) Reset(o P) { *p = o }`,
		},
		{
			name: "nested selector",
			want: "P.SetInner",
			body: `type inner struct{ y int }
type P struct{ in inner }
func (p *P) SetInner(v int) { p.in.y = v }`,
		},
		{
			// Mutating an element of a slice the plan owns is the same defect
			// wearing an index — the slice contents are in the key.
			name: "index into receiver-owned slice",
			want: "P.SetLeg",
			body: `type P struct{ legs []int }
func (p *P) SetLeg(i, v int) { p.legs[i] = v }`,
		},
		{
			name: "increment",
			want: "P.Bump",
			body: `type P struct{ n int }
func (p *P) Bump() { p.n++ }`,
		},
		{
			// Compound assignment is an AssignStmt with a non-"=" token; a
			// detector matching only token.ASSIGN would let this through.
			name: "compound assign",
			want: "P.Add",
			body: `type P struct{ n int }
func (p *P) Add(v int) { p.n += v }`,
		},
		{
			// A VALUE receiver writing itself is a different bug with the same
			// shape — the write is silently discarded rather than corrupting
			// identity. Both are defects; neither should reach the tree.
			name: "value receiver",
			want: "P.SetX",
			body: `type P struct{ x int }
func (p P) SetX(v int) { p.x = v }`,
		},
		{
			// Guarded or conditional writes are still writes. A detector that
			// only looked at top-level body statements would miss this.
			name: "write nested inside control flow",
			want: "P.MaybeSet",
			body: `type P struct{ x int }
func (p *P) MaybeSet(v int) { if v > 0 { for i := 0; i < 2; i++ { p.x = v } } }`,
		},
		{
			// Multi-assign: the receiver target is not in first position.
			name: "receiver in a multi-assign tuple",
			want: "P.SetBoth",
			body: `type P struct{ x int }
func (p *P) SetBoth(v int) { local := 0; local, p.x = v, v; _ = local }`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scanReceiverWriteSnippet(t, []string{"P"}, tc.body)
			if len(got) == 0 {
				t.Fatalf("detector reported nothing; this receiver-write shape would reach "+
					"the tree unnoticed:\n%s", tc.body)
			}
			if !strings.Contains(strings.Join(got, ","), tc.want) {
				t.Errorf("reported %v, want a finding naming %s", got, tc.want)
			}
		})
	}
}

func TestReceiverWriteDetectorStaysSilentOnLegitimateShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		keyed []string
		body  string
	}{
		{
			// The prescribed fix. If this reported, there would be no legal way
			// to write a builder on an identity type.
			name:  "struct copy with override",
			keyed: []string{"P"},
			body: `type P struct{ x int }
func (p *P) WithX(v int) *P { cp := *p; cp.x = v; return &cp }`,
		},
		{
			// Not an identity type: ordinary mutable state, e.g. a cursor.
			name:  "receiver write on a non-identity type",
			keyed: []string{"P"},
			body: `type Cursor struct{ n int }
func (c *Cursor) Advance() { c.n++ }`,
		},
		{
			// A method CALL through the receiver is how the structural-hash
			// memo cell is written (`b.hashMemo.state.Store(h)`). The plan's
			// own fields do not change, so this must not report — otherwise the
			// memo itself would trip the gate.
			name:  "method call through the receiver",
			keyed: []string{"P"},
			body: `type cell struct{}
func (c *cell) Store(v int) {}
type P struct{ memo *cell }
func (p *P) Remember(v int) { p.memo.Store(v) }`,
		},
		{
			// Writing a freshly built local of the receiver's own type is the
			// WithQuantifiers rebuild path, which finishes initialising a NEW
			// plan after constructing it.
			name:  "write to a freshly built local",
			keyed: []string{"P"},
			body: `type P struct{ x int }
func (p *P) Rebuild() *P { fresh := &P{}; fresh.x = p.x; return fresh }`,
		},
		{
			name:  "pure read",
			keyed: []string{"P"},
			body: `type P struct{ x int }
func (p *P) X() int { return p.x }`,
		},
		{
			// A blank receiver cannot be written to; it must not be confused
			// with a parameter or local that happens to share a name.
			name:  "blank receiver with a local of the same shape",
			keyed: []string{"P"},
			body: `type P struct{ x int }
func (_ *P) Build() *P { q := &P{}; q.x = 1; return q }`,
		},
		{
			// A parameter of the receiver's own type is a different object.
			name:  "write to a same-typed parameter",
			keyed: []string{"P"},
			body: `type P struct{ x int }
func (p *P) CopyInto(other *P) { other.x = p.x }`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scanReceiverWriteSnippet(t, tc.keyed, tc.body); len(got) > 0 {
				t.Errorf("detector reported %v on a legitimate shape; a gate that flags the "+
					"prescribed fix gets read past, which is no gate at all:\n%s", got, tc.body)
			}
		})
	}
}

// TestIdentityTypeCollectorFindsEachContractMethod pins the POPULATION half.
//
// The census scans methods on types this collector returns, so a collector that
// silently stopped recognising one of the three contract methods would shrink
// the watched set without any finding changing — green, over less. Each method
// name is driven independently for that reason.
func TestIdentityTypeCollectorFindsEachContractMethod(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		body   string
		want   string
		expect bool
	}{
		{
			name:   "structuralKey",
			body:   "type A struct{}\nfunc (a *A) structuralKey() int { return 0 }",
			want:   "A",
			expect: true,
		},
		{
			name:   "HashCodeWithoutChildren",
			body:   "type B struct{}\nfunc (b *B) HashCodeWithoutChildren() uint64 { return 0 }",
			want:   "B",
			expect: true,
		},
		{
			name:   "EqualsWithoutChildren",
			body:   "type C struct{}\nfunc (c *C) EqualsWithoutChildren(o any) bool { return false }",
			want:   "C",
			expect: true,
		},
		{
			name:   "an unrelated method does not enrol a type",
			body:   "type D struct{}\nfunc (d *D) Explain() string { return \"\" }",
			want:   "D",
			expect: false,
		},
		{
			// A plain function, not a method: nothing to enrol.
			name:   "free function with a contract-shaped name",
			body:   "func structuralKey() int { return 0 }",
			want:   "",
			expect: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "snippet.go", "package p\n\n"+tc.body, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse snippet: %v", err)
			}
			found := map[string]bool{}
			scanIdentityTypes(f, func(n string) { found[n] = true })
			if got := found[tc.want]; got != tc.expect {
				t.Errorf("enrolled[%q] = %v, want %v (found set: %v)", tc.want, got, tc.expect, found)
			}
		})
	}
}

// TestIdentityCensusPopulationIsRealAndAboveItsFloor is the vacuity guard for
// the gate as it runs against the actual tree, driven here so a collapse is
// reported by a test that names the cause rather than by the ratchet quietly
// having nothing to scan.
func TestIdentityCensusPopulationIsRealAndAboveItsFloor(t *testing.T) {
	t.Parallel()

	keyed, _ := collectIdentityCensus(t)
	if len(keyed) < identityTypeFloor {
		t.Fatalf("identity-type population is %d, below the floor of %d; the receiver-write "+
			"ratchet is scanning almost nothing", len(keyed), identityTypeFloor)
	}
	// The plan types are the population the gate was built for; their absence
	// would mean the collector is finding a different set than intended.
	for _, want := range []string{
		"RecordQueryInUnionPlan",
		"RecordQueryInJoinPlan",
		"RecordQueryAggregateIndexPlan",
	} {
		if !keyed[want] {
			t.Errorf("%s is not in the identity-type population; the three converted "+
				"setters lived on these types, so the gate must cover them", want)
		}
	}
}
