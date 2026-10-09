package vectorindex

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// TestVectorCanDeleteWhereBound pins the vector maintainers' DeleteWhere
// bounds, each narrower than the generic root-expression bound a
// StandardIndexMaintainer would answer with (recordlayer's
// TestCanDeleteWhereBoundPerMaintainer covers the core maintainers).
func TestVectorCanDeleteWhereBound(t *testing.T) {
	t.Parallel()
	sub := subspace.FromBytes([]byte{1})
	vecPlain := &recordlayer.Index{
		Name: "vec_plain", Type: recordlayer.IndexTypeVector,
		RootExpression: recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("vector_data")),
		Options:        map[string]string{recordlayer.IndexOptionVectorNumDimensions: "1"},
	}
	vecSplit := &recordlayer.Index{
		Name: "vec", Type: recordlayer.IndexTypeVector,
		RootExpression: recordlayer.KeyWithValue(
			recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("vector_data")), 1),
		Options: map[string]string{recordlayer.IndexOptionVectorNumDimensions: "1"},
	}
	spfIdx := &recordlayer.Index{
		Name: "spf", Type: recordlayer.IndexTypeVectorSPFresh,
		RootExpression: recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")),
		Options:        map[string]string{recordlayer.IndexOptionSPFreshNumDimensions: "1"},
	}
	vectorOf := func(idx *recordlayer.Index) recordlayer.IndexMaintainer {
		m, err := newVectorIndexMaintainer(idx, sub, sub, sub, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	spfresh, err := newSPFreshIndexMaintainer(spfIdx, sub, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		maintainer recordlayer.IndexMaintainer
		bound      int
		// index, when set, is a row whose generic bound must disagree, so the
		// row detects the maintainer's override being lost.
		index *recordlayer.Index
	}{
		{
			// A NON-KeyWithValue root deliberately: splitPrefixAndVector then
			// reads the whole key as the vector and indexes every record under
			// the EMPTY prefix, so no non-empty prefix names a graph. The
			// inherited bound would be the root's width (2) — so this row is
			// the one that fails if vector's override is ever lost, which a
			// KeyWithValue row cannot detect (there the two bounds coincide,
			// because KeyWithValueExpression.ColumnSize IS the split point).
			name:       "VECTOR on a non-KeyWithValue root has no clearable prefix at all",
			maintainer: vectorOf(vecPlain),
			bound:      0,
			index:      vecPlain,
		},
		{
			name:       "VECTOR stops at the KeyWithValue split point",
			maintainer: vectorOf(vecSplit),
			bound:      1,
		},
		{
			name:       "SPFRESH accepts only the whole-index clear",
			maintainer: spfresh,
			bound:      0,
			index:      spfIdx,
		},
	}
	prefixOf := func(n int) tuple.Tuple {
		p := make(tuple.Tuple, n)
		for i := range p {
			p[i] = int64(i)
		}
		return p
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.maintainer.CanDeleteWhere(prefixOf(tc.bound)); err != nil {
				t.Fatalf("a prefix of %d column(s) must be clearable, got: %v", tc.bound, err)
			}
			if err := tc.maintainer.CanDeleteWhere(prefixOf(tc.bound + 1)); err == nil {
				t.Fatalf("a prefix of %d column(s) must be refused: past the bound the clear "+
					"takes some of the index's structures and misses others", tc.bound+1)
			}
			if tc.index == nil {
				return
			}
			generic := recordlayer.NewStandardIndexMaintainer(recordlayer.IndexMaintainerState{Index: tc.index})
			if err := generic.CanDeleteWhere(prefixOf(tc.bound + 1)); err != nil {
				t.Fatalf("this row is marked as discriminating, but the generic root-expression "+
					"bound REFUSES a prefix of %d column(s) too (%v) — so it cannot detect the "+
					"maintainer's override being removed", tc.bound+1, err)
			}
		})
	}
}
