package indexpredicate

import (
	"testing"

	"fdb.dev/pkg/recordlayer/metadatapb"
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

	valueArm := func() *metadatapb.Predicate {
		return &metadatapb.Predicate{ValuePredicate: &metadatapb.ValuePredicate{
			Value: []string{"PRICE"},
			Comparison: &metadatapb.Comparison{
				SimpleComparison: &metadatapb.SimpleComparison{
					Type:    metadatapb.ComparisonType_GREATER_THAN.Enum(),
					Operand: &metadatapb.Value{IntValue: proto.Int32(100)},
				},
			},
		}}
	}
	trueArm := func() *metadatapb.Predicate {
		return &metadatapb.Predicate{ConstantPredicate: &metadatapb.ConstantPredicate{
			Value: metadatapb.ConstantPredicate_TRUE.Enum(),
		}}
	}

	for _, tc := range []struct {
		name  string
		build func() *metadatapb.Predicate
	}{
		{"constant arm", trueArm},
		{"value arm", valueArm},
		{"NOT arm", func() *metadatapb.Predicate {
			return &metadatapb.Predicate{NotPredicate: &metadatapb.NotPredicate{Child: valueArm()}}
		}},
		{"AND that folds to a surviving leaf", func() *metadatapb.Predicate {
			return &metadatapb.Predicate{AndPredicate: &metadatapb.AndPredicate{Children: []*metadatapb.Predicate{
				trueArm(), valueArm(),
			}}}
		}},
		{"AND that keeps several children", func() *metadatapb.Predicate {
			return &metadatapb.Predicate{AndPredicate: &metadatapb.AndPredicate{Children: []*metadatapb.Predicate{
				valueArm(), valueArm(),
			}}}
		}},
		{"OR that keeps several children", func() *metadatapb.Predicate {
			return &metadatapb.Predicate{OrPredicate: &metadatapb.OrPredicate{Children: []*metadatapb.Predicate{
				valueArm(), valueArm(),
			}}}
		}},
		{"singleton OR that collapses", func() *metadatapb.Predicate {
			return &metadatapb.Predicate{OrPredicate: &metadatapb.OrPredicate{Children: []*metadatapb.Predicate{valueArm()}}}
		}},
		{"empty OR handed back unchanged", func() *metadatapb.Predicate {
			return &metadatapb.Predicate{OrPredicate: &metadatapb.OrPredicate{}}
		}},
		{"multi-arm message left alone", func() *metadatapb.Predicate {
			return &metadatapb.Predicate{
				AndPredicate:      &metadatapb.AndPredicate{Children: []*metadatapb.Predicate{valueArm()}},
				ConstantPredicate: &metadatapb.ConstantPredicate{Value: metadatapb.ConstantPredicate_TRUE.Enum()},
			}
		}},
		{"row-number window arm left alone", func() *metadatapb.Predicate {
			return &metadatapb.Predicate{RowNumberWindowPredicate: &metadatapb.RowNumberWindowPredicate{
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
func aliasedNode(a, b *metadatapb.Predicate) bool {
	seen := map[*metadatapb.Predicate]struct{}{}
	var collect func(p *metadatapb.Predicate)
	collect = func(p *metadatapb.Predicate) {
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
	var walk func(p *metadatapb.Predicate)
	walk = func(p *metadatapb.Predicate) {
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
func scribbleOn(p *metadatapb.Predicate) {
	if p == nil {
		return
	}
	if p.ConstantPredicate != nil {
		p.ConstantPredicate.Value = metadatapb.ConstantPredicate_FALSE.Enum()
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
