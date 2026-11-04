package recordlayer

import (
	"context"
	"encoding/hex"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTextLogicalBoundsComeFromTupleEndpoints(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("text_bounds")
	for _, tc := range []struct {
		name   string
		range_ TupleRange
		want   bool
	}{
		{"whole token", TupleRangeAllOf(tuple.Tuple{"tok"}), false},
		{"token prefix", TupleRangePrefixString("tok"), false},
		{"suffix prefix", TupleRangePrefixString("m").Prepend(tuple.Tuple{"tok"}), true},
		{"multiple maps", TupleRangeBetween(tuple.Tuple{"alpha"}, tuple.Tuple{"beta"}), false},
		{"upper only", TupleRange{High: tuple.Tuple{int64(20)}, LowEndpoint: EndpointTypeTreeStart, HighEndpoint: EndpointTypeRangeExclusive}.Prepend(tuple.Tuple{"tok"}), true},
		{"zero", TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(20)}).Prepend(tuple.Tuple{"tok"}), true},
		{"null", TupleRangeBetween(tuple.Tuple{nil}, tuple.Tuple{int64(0)}).Prepend(tuple.Tuple{"tok"}), true},
		{"false", TupleRangeBetween(tuple.Tuple{false}, tuple.Tuple{true}).Prepend(tuple.Tuple{"tok"}), true},
		{"true", TupleRange{Low: tuple.Tuple{true}, LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeTreeEnd}.Prepend(tuple.Tuple{"tok"}), true},
		{"exclusive string", TupleRange{Low: tuple.Tuple{"m"}, High: tuple.Tuple{"q"}, LowEndpoint: EndpointTypeRangeExclusive, HighEndpoint: EndpointTypeRangeExclusive}.Prepend(tuple.Tuple{"tok"}), true},
		{"exclusive zero", TupleRange{Low: tuple.Tuple{int64(0)}, High: tuple.Tuple{int64(20)}, LowEndpoint: EndpointTypeRangeExclusive, HighEndpoint: EndpointTypeRangeExclusive}.Prepend(tuple.Tuple{"tok"}), true},
		{"exclusive 255", TupleRange{Low: tuple.Tuple{int64(255)}, High: tuple.Tuple{int64(300)}, LowEndpoint: EndpointTypeRangeExclusive, HighEndpoint: EndpointTypeRangeExclusive}.Prepend(tuple.Tuple{"tok"}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			splitter := newTextSubspaceSplitterForRange(ss, 1, tc.range_)
			if splitter.logicalBounds != tc.want {
				t.Fatalf("logical bounds=%v, want %v", splitter.logicalBounds, tc.want)
			}
			switch tc.name {
			case "exclusive string", "exclusive zero", "exclusive 255":
				bounds := tc.range_.ToFDBRange(ss)
				if _, err := ss.Unpack(bounds.Begin); err == nil {
					t.Fatal("fixture must exercise a strinc lower bound that is not a packed tuple")
				}
			}
		})
	}
}

func TestTextLogicalBoundsDistinguishGroupedStringPrefixes(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("grouped_text_bounds")
	for _, tc := range []struct {
		name   string
		range_ TupleRange
		want   bool
	}{
		{"token prefix", TupleRangePrefixString("m").Prepend(tuple.Tuple{"g"}), false},
		{"suffix prefix", TupleRangePrefixString("m").Prepend(tuple.Tuple{"g", "tok"}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newTextSubspaceSplitterForRange(ss, 2, tc.range_).logicalBounds; got != tc.want {
				t.Fatalf("logical bounds=%v, want %v", got, tc.want)
			}
		})
	}
}

var _ = Describe("TEXT logical suffix boundary regressions", func() {
	It("bounds decoded entries for upper-only, one-byte, exclusive and prefix suffixes on every page", func() {
		ctx := context.Background()
		idx := NewTextIndex("bounds_text", Field("name"))
		builder := baseBuilder().AddIndex("Customer", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		cases := []struct {
			name    string
			bunches [][]any
			bounds  TupleRange
			want    []any
		}{
			{"zero", [][]any{{int64(-1), int64(0), int64(1)}, {int64(2), int64(3), int64(4)}, {int64(5)}}, TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(4)}), []any{int64(0), int64(1), int64(2), int64(3)}},
			{"upper only", [][]any{{int64(-1), int64(0), int64(1)}, {int64(2), int64(3), int64(4)}, {int64(5)}}, TupleRange{High: tuple.Tuple{int64(4)}, LowEndpoint: EndpointTypeTreeStart, HighEndpoint: EndpointTypeRangeExclusive}, []any{int64(-1), int64(0), int64(1), int64(2), int64(3)}},
			{"exclusive zero", [][]any{{int64(-1), int64(0), int64(1)}, {int64(2), int64(3), int64(4)}, {int64(5)}}, TupleRange{Low: tuple.Tuple{int64(0)}, High: tuple.Tuple{int64(4)}, LowEndpoint: EndpointTypeRangeExclusive, HighEndpoint: EndpointTypeRangeExclusive}, []any{int64(1), int64(2), int64(3)}},
			{"exclusive 255", [][]any{{int64(254), int64(255), int64(256)}, {int64(257), int64(258), int64(259)}, {int64(260)}}, TupleRange{Low: tuple.Tuple{int64(255)}, High: tuple.Tuple{int64(259)}, LowEndpoint: EndpointTypeRangeExclusive, HighEndpoint: EndpointTypeRangeExclusive}, []any{int64(256), int64(257), int64(258)}},
			{"null", [][]any{{nil, []byte{1}, "a"}, {int64(0), int64(1)}}, TupleRangeBetween(tuple.Tuple{nil}, tuple.Tuple{int64(0)}), []any{nil, []byte{1}, "a"}},
			{"false", [][]any{{int64(1), false, true}}, TupleRangeBetween(tuple.Tuple{false}, tuple.Tuple{true}), []any{false}},
			{"true", [][]any{{false, true}}, TupleRange{Low: tuple.Tuple{true}, LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeTreeEnd}, []any{true}},
			{"exclusive string", [][]any{{"a", "m", "n"}, {"o", "p", "q"}, {"z"}}, TupleRange{Low: tuple.Tuple{"m"}, High: tuple.Tuple{"q"}, LowEndpoint: EndpointTypeRangeExclusive, HighEndpoint: EndpointTypeRangeExclusive}, []any{"n", "o", "p"}},
			{"suffix prefix", [][]any{{"a", "m", "ma"}, {"mb", "mz", "n"}, {"z"}}, TupleRangePrefixString("m"), []any{"m", "ma", "mb", "mz"}},
		}
		for _, tc := range cases {
			ss := specSubspace().Sub(tc.name)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				index := store.IndexSubspace(idx)
				for _, bunch := range tc.bunches {
					var entries []BunchedEntry[tuple.Tuple, []int]
					for _, key := range bunch {
						entries = append(entries, BunchedEntry[tuple.Tuple, []int]{Key: tuple.Tuple{key}, Value: []int{0}})
					}
					value, err := TextIndexBunchedSerializerInstance().SerializeEntries(entries)
					if err != nil {
						return nil, err
					}
					rtx.Transaction().Set(index.Pack(tuple.Tuple{"tok", bunch[0]}), value)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			for _, reverse := range []bool{false, true} {
				var want []string
				for _, key := range tc.want {
					want = append(want, hex.EncodeToString(tuple.Tuple{"tok", key}.Pack()))
				}
				if reverse {
					for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
						want[i], want[j] = want[j], want[i]
					}
				}
				for _, direct := range []bool{false, true} {
					for _, limit := range []int{0, 1, 2} {
						result, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
							store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
							if err != nil {
								return nil, err
							}
							var got []string
							var continuation []byte
							for page := 0; page < 20; page++ {
								props := ForwardScan().WithReverse(reverse).WithExecuteProperties(DefaultExecuteProperties().WithReturnedRowLimit(limit))
								var cursor RecordCursor[*IndexEntry]
								if direct {
									cursor = store.ScanIndexByType(idx, IndexScanByTextToken, tc.bounds.Prepend(tuple.Tuple{"tok"}), continuation, props)
								} else {
									scan := NewTextScan(idx, nil, NewTextComparison(TextContainsAll, "tok", "", ""), &tc.bounds)
									cursor, err = scan.Scan(store, continuation, props)
									if err != nil {
										return nil, err
									}
								}
								for {
									r, err := cursor.OnNext(ctx)
									if err != nil {
										_ = cursor.Close()
										return nil, err
									}
									if r.HasNext() {
										got = append(got, hex.EncodeToString(r.GetValue().Key.Pack()))
										continue
									}
									continuation, err = r.GetContinuation().ToBytes()
									if err != nil {
										_ = cursor.Close()
										return nil, err
									}
									if continuation == nil {
										Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
									} else {
										Expect(r.GetNoNextReason()).To(Equal(ReturnLimitReached))
									}
									break
								}
								if err := cursor.Close(); err != nil {
									return nil, err
								}
								if continuation == nil {
									return got, nil
								}
							}
							return nil, &RecordCoreError{Message: "bounded text scan did not exhaust"}
						})
						Expect(err).NotTo(HaveOccurred(), "case=%s reverse=%v direct=%v limit=%d", tc.name, reverse, direct, limit)
						Expect(result).To(Equal(want), "case=%s reverse=%v direct=%v limit=%d", tc.name, reverse, direct, limit)
					}
				}
			}
		}
	})

	It("distinguishes suffix prefixes from unbounded token prefixes with and without grouping", func() {
		ctx := context.Background()
		for _, grouped := range []bool{false, true} {
			var root KeyExpression = Field("name")
			var prefix tuple.Tuple
			if grouped {
				root = GroupBy(root, Field("email"))
				prefix = tuple.Tuple{"g"}
			}
			idx := NewTextIndex("prefix_boundary_text", root)
			md, err := baseBuilder().AddIndex("Customer", idx).Build()
			Expect(err).NotTo(HaveOccurred())
			ss := specSubspace().Sub(grouped)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				index := store.IndexSubspace(idx)
				groups := []tuple.Tuple{prefix}
				if grouped {
					groups = append(groups, tuple.Tuple{"h"})
				}
				for _, group := range groups {
					for _, token := range []string{"a", "m", "ma", "mb", "n"} {
						entries := []BunchedEntry[tuple.Tuple, []int]{{Key: tuple.Tuple{int64(1)}, Value: []int{0}}, {Key: tuple.Tuple{int64(2)}, Value: []int{0}}}
						data, err := TextIndexBunchedSerializerInstance().SerializeEntries(entries)
						if err != nil {
							return nil, err
						}
						key := append(append(tuple.Tuple{}, group...), token, int64(1))
						rtx.Transaction().Set(index.Pack(key), data)
					}
					for _, bunch := range [][]string{{"a", "m", "ma"}, {"mb", "mz", "n"}, {"z"}} {
						var entries []BunchedEntry[tuple.Tuple, []int]
						for _, key := range bunch {
							entries = append(entries, BunchedEntry[tuple.Tuple, []int]{Key: tuple.Tuple{key}, Value: []int{0}})
						}
						data, err := TextIndexBunchedSerializerInstance().SerializeEntries(entries)
						if err != nil {
							return nil, err
						}
						key := append(append(tuple.Tuple{}, group...), "tok", bunch[0])
						rtx.Transaction().Set(index.Pack(key), data)
					}
				}
				for _, suffixPrefix := range []bool{false, true} {
					var suffix *TupleRange
					comparison := NewTextComparison(TextContainsPrefix, "m", "", "")
					scanRange := TupleRangePrefixString("m")
					var want []tuple.Tuple
					if suffixPrefix {
						r := TupleRangePrefixString("m")
						suffix = &r
						comparison = NewTextComparison(TextContainsAll, "tok", "", "")
						scanRange = r.Prepend(tuple.Tuple{"tok"})
						for _, key := range []string{"m", "ma", "mb", "mz"} {
							want = append(want, append(append(tuple.Tuple{}, prefix...), "tok", key))
						}
					} else {
						for _, token := range []string{"m", "ma", "mb"} {
							for _, key := range []int64{1, 2} {
								want = append(want, append(append(tuple.Tuple{}, prefix...), token, key))
							}
						}
					}
					if prefix != nil {
						scanRange = scanRange.Prepend(prefix)
					}
					for _, reverse := range []bool{false, true} {
						expected := append([]tuple.Tuple{}, want...)
						if reverse {
							for i, j := 0, len(expected)-1; i < j; i, j = i+1, j-1 {
								expected[i], expected[j] = expected[j], expected[i]
							}
						}
						for _, direct := range []bool{false, true} {
							props := ForwardScan().WithReverse(reverse)
							var cursor RecordCursor[*IndexEntry]
							if direct {
								cursor = store.ScanIndexByType(idx, IndexScanByTextToken, scanRange, nil, props)
							} else {
								cursor, err = NewTextScan(idx, prefix, comparison, suffix).Scan(store, nil, props)
								if err != nil {
									return nil, err
								}
							}
							entries, err := AsList(ctx, cursor)
							_ = cursor.Close()
							if err != nil {
								return nil, err
							}
							var got []tuple.Tuple
							for _, entry := range entries {
								got = append(got, entry.Key)
							}
							Expect(got).To(Equal(expected), "grouped=%v suffixPrefix=%v reverse=%v direct=%v", grouped, suffixPrefix, reverse, direct)
						}
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("exhausts reverse ALL and PHRASE seeks at a foreign predecessor map", func() {
		ctx := context.Background()
		idx := NewTextIndex("foreign_predecessor_text", Field("name"))
		md, err := baseBuilder().AddIndex("Customer", idx).Build()
		Expect(err).NotTo(HaveOccurred())
		ss := specSubspace()
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			index := store.IndexSubspace(idx)
			for i, token := range []string{"alpha", "beta"} {
				first := int64(11 + i)
				entries := []BunchedEntry[tuple.Tuple, []int]{{Key: tuple.Tuple{first}, Value: []int{i}}, {Key: tuple.Tuple{first + 2}, Value: []int{i}}}
				data, err := TextIndexBunchedSerializerInstance().SerializeEntries(entries)
				if err != nil {
					return nil, err
				}
				rtx.Transaction().Set(index.Pack(tuple.Tuple{token, first}), data)
			}
			suffix := TupleRangeBetween(tuple.Tuple{int64(10)}, tuple.Tuple{int64(20)})
			for i, token := range []string{"alpha", "beta"} {
				scan := NewTextScan(idx, nil, NewTextComparison(TextContainsAll, token, "", ""), &suffix)
				cursor, err := scan.Scan(store, nil, ReverseScan())
				if err != nil {
					return nil, err
				}
				entries, err := AsList(ctx, cursor)
				_ = cursor.Close()
				if err != nil {
					return nil, err
				}
				Expect(entries).To(HaveLen(2))
				Expect(entries[0].Key).To(Equal(tuple.Tuple{token, int64(13 + i)}))
				Expect(entries[1].Key).To(Equal(tuple.Tuple{token, int64(11 + i)}))
			}
			for _, cmp := range []*TextComparison{
				NewTextComparison(TextContainsAll, "alpha beta", "", ""),
				NewTextComparison(TextContainsPhrase, "alpha beta", "", ""),
				NewTextWithMaxDistanceComparison("alpha beta", 1, "", ""),
			} {
				scan := NewTextScan(idx, nil, cmp, &suffix)
				cursor, err := scan.Scan(store, nil, ReverseScan())
				if err != nil {
					return nil, err
				}
				result, err := cursor.OnNext(ctx)
				if err != nil {
					_ = cursor.Close()
					return nil, err
				}
				Expect(result.HasNext()).To(BeFalse(), cmp.String())
				Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
				Expect(result.GetContinuation().IsEnd()).To(BeTrue())
				Expect(cursor.Close()).To(Succeed())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
