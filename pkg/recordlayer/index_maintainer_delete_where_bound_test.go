package recordlayer

import (
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// CanDeleteWhere is what stands between DeleteRecordsWhere and an index whose
// primary entries get cleared while its secondary structures do not. It is
// asked once per index per delete, from a single call site, so a full-suite run
// exercises only the arms the corpus's index shapes happen to reach — and the
// arms that matter most are the rare ones. Every maintainer's bound is
// therefore driven here directly, from an explicit root expression, with no FDB
// and no store.
//
// The shape of the table is deliberate: for each maintainer a prefix AT the
// bound must be ACCEPTED and a prefix ONE COLUMN PAST it must be REFUSED. A
// test asserting only the refusal passes against a bound of zero, which refuses
// everything and breaks every real delete — the vacuous direction.

// deleteWhereBoundCase is one maintainer's bound, driven at and past its limit.
type deleteWhereBoundCase struct {
	name string
	// maintainer is built from an index alone: none of these bodies read the
	// transaction, the subspace or the store.
	maintainer IndexMaintainer
	// bound is the widest prefix the maintainer must accept.
	bound int
	// index is the row's index, needed to ask what the GENERIC root-expression
	// bound would have answered.
	index *Index
	// overridesGeneric marks a row whose whole purpose is that the maintainer's
	// own bound is NARROWER than the one its root expression implies.
	//
	// Such a row is the only thing that can detect the loss of an override, and
	// a sibling row on the same maintainer cannot stand in for it: on the
	// sibling shape the two bounds COINCIDE, so deleting the override leaves it
	// green. Reusing a discriminating row's name on a coinciding shape would
	// therefore keep every gate green while the protection was gone — so the
	// disagreement is asserted here rather than assumed.
	overridesGeneric bool
}

func TestCanDeleteWhereBoundPerMaintainer(t *testing.T) {
	t.Parallel()

	// (quantity, price) grouped by quantity: whole key 2 columns, grouping
	// count 1. This is the shape that left RANK, the aggregates and TEXT
	// clearable past their secondary structures, because the store's alignment
	// check normalises a grouping expression to its WHOLE key.
	grouped := func(name, typ string) *Index {
		return &Index{
			Name:           name,
			Type:           typ,
			RootExpression: GroupBy(Field("price"), Field("quantity")),
		}
	}
	plain := func(name, typ string) *Index {
		return &Index{
			Name:           name,
			Type:           typ,
			RootExpression: Concat(Field("quantity"), Field("price")),
		}
	}
	stdOf := func(idx *Index) standardIndexMaintainer {
		return standardIndexMaintainer{index: idx}
	}

	// The four roots whose maintainer bound is NARROWER than the one their root
	// expression implies. Named here because each is used twice — once to build
	// the maintainer, once to ask what the generic bound would have said — and
	// the two must be the same index or the disagreement is not the row's.
	mdWrapped := &Index{
		Name: "md_wrapped", Type: IndexTypeMultidimensional,
		RootExpression: KeyWithValue(
			Concat(
				Dimensions(Concat(Field("quantity"), Field("coord_x"), Field("coord_y")), 1, 2),
				Field("price")),
			3),
	}
	permIdx := &Index{
		Name: "perm", Type: IndexTypePermutedMax,
		// Grouping count 2, permutedSize 1 -> one clearable column.
		RootExpression: GroupBy(Field("order_id"), Concat(Field("quantity"), Field("price"))),
	}
	vecPlain := &Index{
		Name: "vec_plain", Type: IndexTypeVector,
		RootExpression: Concat(Field("quantity"), Field("vector_data")),
	}
	spfIdx := plain("spf", IndexTypeVectorSPFresh)

	cases := []deleteWhereBoundCase{
		{
			name:       "VALUE takes the root's own width",
			maintainer: &standardIndexMaintainer{index: plain("v", IndexTypeValue)},
			bound:      2,
		},
		{
			name: "KeyWithValue stops at the split point, not the inner width",
			maintainer: &standardIndexMaintainer{index: &Index{
				Name: "kwv", Type: IndexTypeValue,
				RootExpression: KeyWithValue(
					Concat(Field("quantity"), Field("price"), Field("order_id")), 2),
			}},
			bound: 2,
		},
		{
			name:       "RANK stops at the grouping columns its ranked sets are keyed by",
			maintainer: &rankIndexMaintainer{standardIndexMaintainer: stdOf(grouped("rank", IndexTypeRank))},
			bound:      1,
		},
		{
			name: "TIME_WINDOW_LEADERBOARD stops at the grouping columns",
			maintainer: &timeWindowLeaderboardIndexMaintainer{
				standardIndexMaintainer: stdOf(grouped("lb", IndexTypeTimeWindowLeaderboard)),
			},
			bound: 1,
		},
		{
			name: "MULTIDIMENSIONAL stops at the R-tree prefix, not the whole key",
			maintainer: &multidimensionalIndexMaintainer{
				standardIndexMaintainer: stdOf(&Index{
					Name: "md", Type: IndexTypeMultidimensional,
					RootExpression: Dimensions(
						Concat(Field("quantity"), Field("coord_x"), Field("coord_y")), 1, 2),
				}),
			},
			bound: 1,
		},
		{
			// The dimensions expression may be WRAPPED — extractDimensionsExpression
			// unwraps KeyWithValue and Concat, and the R-trees are still rooted at
			// PrefixSize. The generic bound sees only the KeyWithValue here and
			// would answer with its split point (3), so this row is the one that
			// fails if the maintainer's own override is lost; the unwrapped row
			// above cannot detect that, because there the two bounds coincide.
			name: "MULTIDIMENSIONAL wrapped in KeyWithValue still stops at the R-tree prefix",
			maintainer: &multidimensionalIndexMaintainer{
				standardIndexMaintainer: stdOf(mdWrapped),
			},
			bound:            1,
			index:            mdWrapped,
			overridesGeneric: true,
		},
		{
			name: "PERMUTED subtracts the permuted columns from the grouping count",
			maintainer: &permutedMinMaxIndexMaintainer{
				standardIndexMaintainer: &standardIndexMaintainer{index: permIdx},
				permutedSize:            1,
			},
			bound:            1,
			index:            permIdx,
			overridesGeneric: true,
		},
		{
			name:       "COUNT stops at the grouping columns its aggregate is keyed by",
			maintainer: &atomicMutationIndexMaintainer{index: grouped("cnt", IndexTypeCount)},
			bound:      1,
		},
		{
			name:       "BITMAP_VALUE stops at the grouping columns",
			maintainer: &bitmapValueIndexMaintainer{index: grouped("bmp", IndexTypeBitmapValue)},
			bound:      1,
		},
		{
			name:       "MAX_EVER_VERSION stops at the grouping columns",
			maintainer: &maxEverVersionIndexMaintainer{index: grouped("mev", IndexTypeMaxEverVersion)},
			bound:      1,
		},
		{
			name:       "VERSION takes the root's own width",
			maintainer: &versionIndexMaintainer{index: plain("ver", IndexTypeVersion)},
			bound:      2,
		},
		{
			name:       "TEXT stops at the grouping columns its bunched map is keyed by",
			maintainer: &textIndexMaintainer{index: grouped("txt", IndexTypeText)},
			bound:      1,
		},
		{
			// A NON-KeyWithValue root deliberately: splitPrefixAndVector then
			// reads the whole key as the vector and indexes every record under
			// the EMPTY prefix, so no non-empty prefix names a graph. The
			// inherited bound would be the root's width (2) — so this row is
			// the one that fails if vector's override is ever lost, which a
			// KeyWithValue row cannot detect (there the two bounds coincide,
			// because KeyWithValueExpression.ColumnSize IS the split point).
			name:             "VECTOR on a non-KeyWithValue root has no clearable prefix at all",
			maintainer:       &vectorIndexMaintainer{standardIndexMaintainer: stdOf(vecPlain)},
			bound:            0,
			index:            vecPlain,
			overridesGeneric: true,
		},
		{
			name: "VECTOR stops at the KeyWithValue split point",
			maintainer: &vectorIndexMaintainer{standardIndexMaintainer: stdOf(&Index{
				Name: "vec", Type: IndexTypeVector,
				RootExpression: KeyWithValue(
					Concat(Field("quantity"), Field("vector_data")), 1),
			})},
			bound: 1,
		},
		{
			name:             "SPFRESH accepts only the whole-index clear",
			maintainer:       &spfreshIndexMaintainer{standardIndexMaintainer: stdOf(spfIdx)},
			bound:            0,
			index:            spfIdx,
			overridesGeneric: true,
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
			// AT the bound: must be accepted. Without this half, a bound of
			// zero satisfies the refusal below while breaking every real
			// DeleteRecordsWhere on this index type.
			if err := tc.maintainer.CanDeleteWhere(prefixOf(tc.bound)); err != nil {
				t.Fatalf("a prefix of %d column(s) must be clearable, got: %v", tc.bound, err)
			}
			// ONE PAST it: must be refused, because past this width the
			// maintainer's structures are no longer all named by the prefix.
			if err := tc.maintainer.CanDeleteWhere(prefixOf(tc.bound + 1)); err == nil {
				t.Fatalf("a prefix of %d column(s) must be refused: past the bound the clear "+
					"takes some of the index's structures and misses others", tc.bound+1)
			}
			if !tc.overridesGeneric {
				return
			}
			// This row exists to prove an override is live, which it can only do
			// while the GENERIC bound actually disagrees with it. Reshape the row
			// onto a root where the two coincide and the row keeps passing with
			// the override deleted — it would then be a sentinel guarding
			// nothing, and every gate above it would still be green.
			if err := checkDeleteWhereBound(tc.index, prefixOf(tc.bound+1)); err != nil {
				t.Fatalf("this row is marked as discriminating, but the generic root-expression "+
					"bound REFUSES a prefix of %d column(s) too (%v) — so it cannot detect the "+
					"maintainer's override being removed", tc.bound+1, err)
			}
		})
	}

	// Whether this table COVERS every maintainer is not decided here. It cannot
	// be: any count taken from `cases` is derived from the table, so it goes on
	// agreeing with itself when a maintainer is added and no row is. An earlier
	// `len(cases) != 14` guard here claimed to catch exactly that and could not.
	//
	// The population is derived independently, from the source tree, by
	// TestEveryIndexMaintainerHasADeleteWhereBoundRow in pkg/docscheck: it
	// collects the types declaring a DeleteWhere method and requires each to be
	// constructed here. Adding a maintainer without a row fails THERE.
}

// The sliding window is a DECORATOR, so its answer is not its own: Java's
// canDeleteWhere begins by asking the delegate and only then applies the
// partition bound. A decorator that skipped the delegate would be MORE
// permissive than what it wraps, which is exactly the direction that clears a
// graph's records and leaves the graph.
func TestSlidingWindowCanDeleteWhereNeverExceedsItsDelegate(t *testing.T) {
	t.Parallel()

	// The delegate accepts one column (split point 1); the window partitions on
	// two. Without forwarding, the window would accept two.
	delegate := &vectorIndexMaintainer{standardIndexMaintainer: standardIndexMaintainer{index: &Index{
		Name: "vec", Type: IndexTypeVector,
		RootExpression: KeyWithValue(Concat(Field("quantity"), Field("vector_data")), 1),
	}}}
	m := &slidingWindowIndexMaintainer{
		index:                  &Index{Name: "win", Type: IndexTypeVector},
		delegate:               delegate,
		partitionKeyColumnSize: 2,
	}

	if err := m.CanDeleteWhere(tuple.Tuple{int64(1)}); err != nil {
		t.Fatalf("one column is within both bounds, got: %v", err)
	}
	err := m.CanDeleteWhere(tuple.Tuple{int64(1), int64(2)})
	if err == nil {
		t.Fatal("two columns satisfy the window's partition bound but NOT the delegate's " +
			"split point; the decorator must refuse")
	}
	// It must be the DELEGATE's refusal that surfaces. Asserting only that
	// SOME error came back would pass with the forwarding removed, since the
	// window's own partition bound also refuses at some width.
	if !strings.Contains(err.Error(), "vector index") {
		t.Fatalf("expected the delegate's refusal to surface, got: %s", err)
	}
}
