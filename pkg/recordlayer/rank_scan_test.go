package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

func TestRankScanBoundsValidation(t *testing.T) {
	t.Parallel()
	for _, scanType := range []IndexScanType{IndexScanByValue, IndexScanByRank, IndexScanByGroup, IndexScanByTimeWindow, IndexScanByDistance, ""} {
		for _, include := range []bool{false, true} {
			bounds, err := NewRankScanBounds(scanType, TupleRangeAll, include)
			valid := scanType == IndexScanByValue || scanType == IndexScanByRank
			if (err == nil) != valid {
				t.Fatalf("scan=%q include=%t err=%v", scanType, include, err)
			}
			if !valid {
				var argument *RecordCoreArgumentError
				if !errors.As(err, &argument) || argument.ScanType != scanType {
					t.Fatalf("wrong error context: %v", err)
				}
			}
			if bounds.IncludeRankAsValue != include || bounds.ScanType != scanType {
				t.Fatal("constructor lost options")
			}
		}
	}
}

var _ = Describe("Rank-valued scans", func() {
	for _, include := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			It(fmt.Sprintf("keeps snapshot score lookup serializable only when enriched=%t missing=%t", include, missing), func() {
				b := baseBuilder()
				index := NewRankIndex("rank", Ungrouped(Field("price")))
				b.AddIndex("Order", index)
				md, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				ss := specSubspace()
				ctx := context.Background()
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
					Expect(err).NotTo(HaveOccurred())
					if missing {
						maintainer, err := store.getIndexMaintainer(index)
						Expect(err).NotTo(HaveOccurred())
						rank := maintainer.(*rankIndexMaintainer)
						rs := newRankedSet(rank.secondarySubspace, rank.rankedSetConfig)
						removed, err := rs.Remove(rtx.Transaction(), tuple.Tuple{int64(100)}.Pack())
						Expect(err).NotTo(HaveOccurred())
						Expect(removed).To(BeTrue())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				rtx := sharedDB.NewRecordContext(tx)
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				Expect(err).NotTo(HaveOccurred())
				props := ForwardScan()
				props.ExecuteProperties.IsolationLevel = SnapshotIsolation
				entries, err := AsList(ctx, store.ScanRankIndex(index, RankScanBounds{ScanType: IndexScanByValue, RankRange: TupleRangeAll, IncludeRankAsValue: include}, nil, props))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				if include {
					want := tuple.Tuple{int64(0)}
					if missing {
						want = tuple.Tuple{nil}
					}
					Expect(entries[0].Value).To(Equal(want))
				} else {
					Expect(entries[0].Value).To(BeEmpty())
				}
				_, err = sharedDB.Run(ctx, func(writer *FDBRecordContext) (any, error) {
					other, err := NewStoreBuilder().SetContext(writer).SetMetaDataProvider(md).SetSubspace(ss).Open()
					Expect(err).NotTo(HaveOccurred())
					maintainer, err := other.getIndexMaintainer(index)
					Expect(err).NotTo(HaveOccurred())
					rank := maintainer.(*rankIndexMaintainer)
					rs := newRankedSet(rank.secondarySubspace, rank.rankedSetConfig)
					var removed bool
					if missing {
						// The missing score's point read must not conflict with an
						// unrelated score; cache preloading is snapshot in Java.
						removed, err = rs.Add(writer.Transaction(), tuple.Tuple{int64(200)}.Pack())
					} else {
						removed, err = rs.Remove(writer.Transaction(), tuple.Tuple{int64(100)}.Pack())
					}
					Expect(err).NotTo(HaveOccurred())
					Expect(removed).To(BeTrue())
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				tx.Set(ss.Pack(tuple.Tuple{"rank-read-sentinel"}), []byte("force conflict validation"))
				err = rtx.Commit()
				if include && !missing {
					var conflict fdb.Error
					Expect(errors.As(err, &conflict)).To(BeTrue())
					Expect(conflict.Code).To(Equal(1020))
				} else {
					Expect(err).NotTo(HaveOccurred(), "plain scans and missing-score preloads must not add unrelated secondary conflicts")
				}
			})
		}
	}
	It("propagates cancellation from a rank lookup without mutating the entry", func() {
		b := baseBuilder()
		index := NewRankIndex("rank", Ungrouped(Field("price")))
		b.AddIndex("Order", index)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		rtx := sharedDB.NewRecordContext(tx)
		store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		maintainer, err := store.getIndexMaintainer(index)
		Expect(err).NotTo(HaveOccurred())
		entry := &IndexEntry{Index: index, Key: tuple.Tuple{int64(100), int64(1)}, Value: tuple.Tuple{}}
		rtx.Cancel()
		result, err := maintainer.(*rankIndexMaintainer).entryWithRank(entry)
		Expect(err).To(HaveOccurred())
		var cancelled fdb.Error
		Expect(errors.As(err, &cancelled)).To(BeTrue())
		Expect(cancelled.Code).To(Equal(1025))
		Expect(result).To(BeNil())
		Expect(entry.Value).To(BeEmpty())
	})
	It("validates dispatch and state and preserves null ranks for missing scores", func() {
		b := baseBuilder()
		index := NewRankIndex("rank", Ungrouped(Field("price")))
		ordinary := NewIndex("ordinary", Field("quantity"))
		b.AddIndex("Order", index)
		b.AddIndex("Order", ordinary)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		ctx := context.Background()
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			bounds := RankScanBounds{ScanType: IndexScanByValue, RankRange: TupleRangeAll, IncludeRankAsValue: true}
			entries, err := AsList(ctx, store.ScanRankIndex(index, bounds, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())
			for _, invalidIndex := range []*Index{nil, ordinary} {
				_, err := AsList(ctx, store.ScanRankIndex(invalidIndex, bounds, nil, ForwardScan()))
				var argument *RecordCoreArgumentError
				Expect(errors.As(err, &argument)).To(BeTrue())
				Expect(argument.ScanType).To(Equal(IndexScanByValue))
			}
			for _, include := range []bool{false, true} {
				invalid := RankScanBounds{ScanType: IndexScanByGroup, IncludeRankAsValue: include}
				_, err := AsList(ctx, store.ScanRankIndex(index, invalid, nil, ForwardScan()))
				var argument *RecordCoreArgumentError
				Expect(errors.As(err, &argument)).To(BeTrue())
				Expect(argument.ScanType).To(Equal(IndexScanByGroup))
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())
			maintainer, err := store.getIndexMaintainer(index)
			Expect(err).NotTo(HaveOccurred())
			rank := maintainer.(*rankIndexMaintainer)
			_, err = rank.entryWithRank(&IndexEntry{Index: index})
			var argument *RecordCoreArgumentError
			Expect(errors.As(err, &argument)).To(BeTrue())
			Expect(argument.IndexName).To(Equal(index.Name))
			raw, err := AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).To(HaveLen(1))
			enriched, err := rank.entryWithRank(raw[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(enriched.Value).To(Equal(tuple.Tuple{int64(0)}))
			Expect(raw[0].Value).To(BeEmpty())
			// Leave the B-tree entry intact while removing its secondary score.
			// Java's nullIfMissing lookup represents this as tuple(null).
			rs := newRankedSet(rank.secondarySubspace, rank.rankedSetConfig)
			removed, err := rs.Remove(rtx.Transaction(), tuple.Tuple{int64(100)}.Pack())
			Expect(err).NotTo(HaveOccurred())
			Expect(removed).To(BeTrue())
			entries, err = AsList(ctx, store.ScanRankIndex(index, bounds, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{nil}))
			Expect(entries[0].Value.Pack()).To(Equal([]byte{0}))
			Expect(entries[0].Key).To(Equal(raw[0].Key))
			Expect(entries[0].PrimaryKey()).To(Equal(raw[0].PrimaryKey()))
			bounds.IncludeRankAsValue = false
			entries, err = AsList(ctx, store.ScanRankIndex(index, bounds, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(BeEmpty())
			_, err = store.MarkIndexDisabled(index.Name)
			Expect(err).NotTo(HaveOccurred())
			for _, include := range []bool{false, true} {
				bounds.IncludeRankAsValue = include
				_, err := AsList(ctx, store.ScanRankIndex(index, bounds, nil, ForwardScan()))
				var unreadable *IndexNotReadableError
				Expect(errors.As(err, &unreadable)).To(BeTrue())
				Expect(unreadable.IndexName).To(Equal(index.Name))
				Expect(unreadable.CurrentState).To(Equal(IndexStateDisabled))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	for _, duplicates := range []bool{false, true} {
		for _, overlap := range []bool{false, true} {
			It(fmt.Sprintf("preserves grouped composite score ranks with ties, duplicates=%t overlap=%t", duplicates, overlap), func() {
				b := baseBuilder()
				if overlap {
					b.GetRecordType("Order").SetPrimaryKey(Concat(Field("price"), Field("order_id")))
				}
				index := NewRankIndex("rank", GroupBy(Concat(Field("price"), Field("quantity")), Field("coord_x")))
				if duplicates {
					index.Options[IndexOptionRankCountDuplicates] = "true"
				}
				b.AddIndex("Order", index)
				md, err := b.Build()
				Expect(err).NotTo(HaveOccurred())
				ss := specSubspace()
				ctx := context.Background()
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					for i, score := range [][3]int64{{0, 100, 1}, {0, 100, 1}, {0, 100, 2}, {0, 200, 1}, {1, 300, 1}} {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), CoordX: proto.Int64(score[0]), Price: proto.Int32(int32(score[1])), Quantity: proto.Int32(int32(score[2]))})
						Expect(err).NotTo(HaveOccurred())
					}
					maintainer, err := store.getIndexMaintainer(index)
					Expect(err).NotTo(HaveOccurred())
					rankMaintainer := maintainer.(*rankIndexMaintainer)
					rs := newRankedSet(rankMaintainer.secondarySubspace.Sub(int64(0)), rankMaintainer.rankedSetConfig)
					count, err := rs.Count(rtx.Transaction(), tuple.Tuple{int64(100), int64(1)}.Pack())
					Expect(err).NotTo(HaveOccurred())
					expectedCount := int64(1)
					if duplicates {
						expectedCount = 2
					}
					Expect(count).To(Equal(expectedCount), "count before commit")
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					Expect(err).NotTo(HaveOccurred())
					maintainer, err := store.getIndexMaintainer(index)
					Expect(err).NotTo(HaveOccurred())
					rankMaintainer := maintainer.(*rankIndexMaintainer)
					Expect(rankMaintainer.rankedSetConfig.CountDuplicates).To(Equal(duplicates))
					rs := newRankedSet(rankMaintainer.secondarySubspace.Sub(int64(0)), rankMaintainer.rankedSetConfig)
					count, err := rs.Count(rtx.Transaction(), tuple.Tuple{int64(100), int64(1)}.Pack())
					Expect(err).NotTo(HaveOccurred())
					expectedCount := int64(1)
					if duplicates {
						expectedCount = 2
					}
					Expect(count).To(Equal(expectedCount))
					want := []int64{0, 0, 1, 2}
					if duplicates {
						want = []int64{0, 0, 2, 3}
					}
					for _, kind := range []IndexScanType{IndexScanByValue, IndexScanByRank} {
						scanRange := TupleRangeAllOf(tuple.Tuple{int64(0)})
						if kind == IndexScanByRank {
							scanRange = TupleRange{Low: tuple.Tuple{int64(0), int64(0)}, High: tuple.Tuple{int64(0), int64(10)}, LowEndpoint: EndpointTypeRangeInclusive, HighEndpoint: EndpointTypeRangeExclusive}
						}
						for _, byteLimit := range []bool{false, true} {
							for _, reverse := range []bool{false, true} {
								props := ForwardScan()
								props.Reverse = reverse
								reason := ReturnLimitReached
								if byteLimit {
									props.ExecuteProperties.ScannedBytesLimit = 1
									reason = ByteLimitReached
								} else {
									props.ExecuteProperties.ReturnedRowLimit = 1
								}
								timer := NewStoreTimer()
								rtx.SetTimer(timer)
								var continuation []byte
								for position := 0; position < 4; position++ {
									plain := store.ScanRankIndex(index, RankScanBounds{ScanType: kind, RankRange: scanRange}, continuation, props)
									enriched := store.ScanRankIndex(index, RankScanBounds{ScanType: kind, RankRange: scanRange, IncludeRankAsValue: true}, continuation, props)
									for step := 0; step < 3; step++ {
										before := timer.GetCount(EventScanIndex)
										left, err := plain.OnNext(ctx)
										Expect(err).NotTo(HaveOccurred())
										right, err := enriched.OnNext(ctx)
										Expect(err).NotTo(HaveOccurred())
										Expect(timer.GetCount(EventScanIndex) - before).To(Equal(int64(2)))
										Expect(left.HasNext()).To(Equal(step == 0))
										Expect(right.HasNext()).To(Equal(step == 0))
										leftBytes, err := left.GetContinuation().ToBytes()
										Expect(err).NotTo(HaveOccurred())
										rightBytes, err := right.GetContinuation().ToBytes()
										Expect(err).NotTo(HaveOccurred())
										Expect(rightBytes).To(Equal(leftBytes))
										end := !byteLimit && position == 3 && step > 0
										Expect(left.GetContinuation().IsEnd()).To(Equal(end))
										Expect(right.GetContinuation().IsEnd()).To(Equal(end))
										if step == 0 {
											i := position
											if reverse {
												i = 3 - position
											}
											Expect(left.GetValue().Value).To(BeEmpty())
											Expect(right.GetValue().Value).To(Equal(tuple.Tuple{want[i]}))
											Expect(right.GetValue().Key).To(Equal(left.GetValue().Key))
										} else {
											wantReason := reason
											if end {
												wantReason = SourceExhausted
											}
											Expect(left.GetNoNextReason()).To(Equal(wantReason))
											Expect(right.GetNoNextReason()).To(Equal(wantReason))
											if step == 2 {
												Expect(leftBytes).To(Equal(continuation))
											}
											continuation = leftBytes
										}
									}
									Expect(plain.Close()).To(Succeed())
									Expect(enriched.Close()).To(Succeed())
								}
								rtx.SetTimer(nil)
							}
						}
						for _, include := range []bool{false, true} {
							bounds, err := NewRankScanBounds(kind, scanRange, include)
							Expect(err).NotTo(HaveOccurred())
							entries, err := AsList(ctx, store.ScanRankIndex(index, bounds, nil, ForwardScan()))
							Expect(err).NotTo(HaveOccurred())
							Expect(entries).To(HaveLen(4))
							for i, entry := range entries {
								if include {
									Expect(entry.Value).To(Equal(tuple.Tuple{want[i]}))
								} else {
									Expect(entry.Value).To(BeEmpty())
								}
								Expect(entry.Index).To(BeIdenticalTo(index))
								expectedPK := tuple.Tuple{int64(i + 1)}
								if overlap {
									expectedPK = tuple.Tuple{entry.Key[1], int64(i + 1)}
								}
								Expect(entry.PrimaryKey()).To(Equal(expectedPK))
							}
							for _, reverse := range []bool{false, true} {
								props := ForwardScan()
								if reverse {
									props = ReverseScan()
								}
								props.ExecuteProperties.ReturnedRowLimit = 2
								var continuation []byte
								var paged []*IndexEntry
								finished := false
								for page := 0; page < 4; page++ {
									cursor := store.ScanRankIndex(index, bounds, continuation, props)
									for {
										result, err := cursor.OnNext(ctx)
										Expect(err).NotTo(HaveOccurred())
										if !result.HasNext() {
											finished = result.GetContinuation().IsEnd()
											continuation, err = result.GetContinuation().ToBytes()
											Expect(err).NotTo(HaveOccurred())
											break
										}
										paged = append(paged, result.GetValue())
									}
									Expect(cursor.Close()).To(Succeed())
									Expect(cursor.IsClosed()).To(BeTrue())
									if finished {
										break
									}
								}
								Expect(finished).To(BeTrue(), "pagination must terminate")
								Expect(paged).To(HaveLen(4))
								for i, entry := range paged {
									position := i
									if reverse {
										position = 3 - i
									}
									Expect(entry.Key).To(Equal(entries[position].Key))
									Expect(entry.Value).To(Equal(entries[position].Value))
								}
							}
						}
					}
					entries, err := AsList(ctx, store.ScanRankIndex(index, RankScanBounds{ScanType: IndexScanByValue, RankRange: TupleRangeAllOf(tuple.Tuple{int64(1)}), IncludeRankAsValue: true}, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(1))
					Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(0)}))
					raw, err := AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(raw).To(HaveLen(5))
					for _, entry := range raw {
						Expect(entry.Value).To(BeEmpty())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}
})
