package indexpredicate

import (
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// TestNormalizeReturnsAFreshTree enforces the contract the
// doc comment promises: the result shares no node with the input, so a caller
// editing either tree cannot reach the other.
//
// The promise is not decoration. A stored index predicate is metadata that
// outlives any one plan, and the normalized form is handed to a candidate that
// caches it for the life of the plan context; an aliased leaf would let a later
// metadata edit silently retune a live candidate's sparseness. The arms that
// are returned UNCHANGED in shape (constant, value, NOT, multi-arm, empty OR)
// are the ones where an identity return is the tempting shortcut, so each is
// checked explicitly rather than through one representative.
func TestNormalizeReturnsAFreshTree(t *testing.T) {
	t.Parallel()

	valueArm := func() *gen.Predicate {
		return &gen.Predicate{ValuePredicate: &gen.ValuePredicate{
			Value: []string{"PRICE"},
			Comparison: &gen.Comparison{
				SimpleComparison: &gen.SimpleComparison{
					Type:    gen.ComparisonType_GREATER_THAN.Enum(),
					Operand: &gen.Value{IntValue: proto.Int32(100)},
				},
			},
		}}
	}
	trueArm := func() *gen.Predicate {
		return &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		}}
	}

	for _, tc := range []struct {
		name  string
		build func() *gen.Predicate
	}{
		{"constant arm", trueArm},
		{"value arm", valueArm},
		{"NOT arm", func() *gen.Predicate {
			return &gen.Predicate{NotPredicate: &gen.NotPredicate{Child: valueArm()}}
		}},
		{"AND that folds to a surviving leaf", func() *gen.Predicate {
			return &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				trueArm(), valueArm(),
			}}}
		}},
		{"AND that keeps several children", func() *gen.Predicate {
			return &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				valueArm(), valueArm(),
			}}}
		}},
		{"OR that keeps several children", func() *gen.Predicate {
			return &gen.Predicate{OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{
				valueArm(), valueArm(),
			}}}
		}},
		{"singleton OR that collapses", func() *gen.Predicate {
			return &gen.Predicate{OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{valueArm()}}}
		}},
		{"empty OR handed back unchanged", func() *gen.Predicate {
			return &gen.Predicate{OrPredicate: &gen.OrPredicate{}}
		}},
		{"multi-arm message left alone", func() *gen.Predicate {
			return &gen.Predicate{
				AndPredicate:      &gen.AndPredicate{Children: []*gen.Predicate{valueArm()}},
				ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()},
			}
		}},
		{"row-number window arm left alone", func() *gen.Predicate {
			return &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				Size: proto.Int32(100),
			}}
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := tc.build()
			pristine := tc.build()
			out := Normalize(input)
			if out == nil {
				t.Fatal("normalizer returned nil for a non-nil predicate")
			}
			if out == input {
				t.Fatal("normalizer returned the INPUT pointer; the contract promises a " +
					"fresh tree, and an aliased root lets a caller mutate metadata through it")
			}
			if aliasedNode(out, input) {
				t.Fatal("normalized tree shares a node with the input; rebuilt AND/OR " +
					"nodes must not carry the original leaves")
			}
			// Editing the result must not reach the input.
			scribbleOn(out)
			if !proto.Equal(input, pristine) {
				t.Fatalf("mutating the normalized tree changed the INPUT:\n got %v\nwant %v", input, pristine)
			}
		})
	}
}

// aliasedNode reports whether any node reachable from a is the same pointer as
// any node reachable from b.
func aliasedNode(a, b *gen.Predicate) bool {
	seen := map[*gen.Predicate]struct{}{}
	var collect func(p *gen.Predicate)
	collect = func(p *gen.Predicate) {
		if p == nil {
			return
		}
		seen[p] = struct{}{}
		if p.AndPredicate != nil {
			for _, c := range p.AndPredicate.Children {
				collect(c)
			}
		}
		if p.OrPredicate != nil {
			for _, c := range p.OrPredicate.Children {
				collect(c)
			}
		}
		if p.NotPredicate != nil {
			collect(p.NotPredicate.Child)
		}
	}
	collect(b)
	found := false
	var walk func(p *gen.Predicate)
	walk = func(p *gen.Predicate) {
		if p == nil || found {
			return
		}
		if _, ok := seen[p]; ok {
			found = true
			return
		}
		if p.AndPredicate != nil {
			for _, c := range p.AndPredicate.Children {
				walk(c)
			}
		}
		if p.OrPredicate != nil {
			for _, c := range p.OrPredicate.Children {
				walk(c)
			}
		}
		if p.NotPredicate != nil {
			walk(p.NotPredicate.Child)
		}
	}
	walk(a)
	return found
}

// scribbleOn edits every reachable leaf of a predicate tree in place.
func scribbleOn(p *gen.Predicate) {
	if p == nil {
		return
	}
	if p.ConstantPredicate != nil {
		p.ConstantPredicate.Value = gen.ConstantPredicate_FALSE.Enum()
	}
	if p.ValuePredicate != nil {
		p.ValuePredicate.Value = []string{"SCRIBBLED"}
	}
	if p.RowNumberWindowPredicate != nil {
		p.RowNumberWindowPredicate.Size = proto.Int32(-1)
	}
	if p.AndPredicate != nil {
		for _, c := range p.AndPredicate.Children {
			scribbleOn(c)
		}
	}
	if p.OrPredicate != nil {
		for _, c := range p.OrPredicate.Children {
			scribbleOn(c)
		}
	}
	if p.NotPredicate != nil {
		scribbleOn(p.NotPredicate.Child)
	}
}
