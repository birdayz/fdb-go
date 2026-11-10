package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Partition writer participation", func() {
	ctx := context.Background()

	for _, format := range []int32{5, 14} {
		It(fmt.Sprintf("routes every save surface and cleans pending/committed versions (format=%d)", format), func() {
			b := partitionTestBuilder().SetStoreRecordVersions(true).SetSplitLongRecords(true)
			idx := NewIndex("writer_value", Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
			b.AddIndex("Order", idx)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			open := func(rc *FDBRecordContext) *FDBRecordStore {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(format).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				return store
			}
			var pks []tuple.Tuple
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(format).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				typed, err := GetTypedRecordStore[*demopb.Order](store, "Order")
				Expect(err).NotTo(HaveOccurred())
				for mode := 0; mode < 10; mode++ {
					source, coordinate := int64(-11-10*mode), int64(-2-mode)
					record := partitionOrder(int64(mode+1), source, int32(mode))
					rt := md.GetRecordType("Order")
					ss := partitionSubspace(root, rt)
					Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"active", coordinate})).MustGet()).To(BeNil())
					Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"fence", coordinate})).MustGet()).To(BeNil())
					switch mode {
					case 0:
						_, err = store.SaveRecord(record)
					case 1:
						_, err = store.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfExists)
					case 2:
						_, err = store.SaveRecordWithVersion(record, RecordExistenceCheckNone, nil, VersionstampSaveBehaviorWithVersion)
					case 3:
						_, err = store.OverrideLockSaveRecord(record, RecordExistenceCheckNone)
					case 4:
						_, err = store.OverrideLockSaveRecordWithVersion(record, RecordExistenceCheckNone, nil, VersionstampSaveBehaviorWithVersion)
					case 5:
						_, err = typed.SaveRecord(record)
					case 6:
						_, err = typed.SaveRecordWithVersion(record, RecordExistenceCheckNone, nil, VersionstampSaveBehaviorWithVersion)
					case 7:
						_, err = store.SaveRecordsPipelined([]proto.Message{record}, RecordExistenceCheckNone)
					case 8:
						_, err = store.SaveRecordBatch([]proto.Message{record})
					case 9:
						_, err = store.SaveRecordBatchWithOptions([]proto.Message{record}, BatchSaveOptions{CoalesceIndexWriteConflicts: true})
					}
					Expect(err).NotTo(HaveOccurred(), "save mode=%d", mode)
					pk := tuple.Tuple{rt.GetRecordTypeKey(), coordinate, source, int64(mode + 1)}
					pks = append(pks, pk)
					Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"active", coordinate})).MustGet()).To(Equal((tuple.Tuple{int64(1)}).Pack()))
					Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"fence", coordinate})).MustGet()).To(Equal((tuple.Tuple{int64(1), true}).Pack()))
					loaded, err := store.LoadRecord(pk)
					Expect(err).NotTo(HaveOccurred())
					Expect(proto.Equal(loaded.Record, record)).To(BeTrue())
					Expect(loaded.Version).NotTo(BeNil())
					Expect(rc.Transaction().Get(root.Sub(RecordKey).Pack(append(append(tuple.Tuple{}, pk...), int64(0)))).MustGet()).NotTo(BeNil())
					indexKey := tuple.Tuple{coordinate, int64(mode), rt.GetRecordTypeKey(), source, int64(mode + 1)}
					Expect(rc.Transaction().Get(store.indexSubspace(idx).Pack(indexKey)).MustGet()).To(Equal([]byte{}))
					entries, err := AsList(ctx, store.ScanIndexRecords(idx.Name, TupleRangeAllOf(tuple.Tuple{coordinate}), nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(1))
					Expect(entries[0].Record.PrimaryKey).To(Equal(pk))
				}
				Expect(partitionProtocolRows(rc, root)).To(HaveLen(22))
				Expect(rc.HasVersionMutations()).To(BeTrue())
				_, err = store.SaveRecord(partitionOrder(20, 10, 20))
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := open(rc)
				for _, pk := range pks {
					record, err := store.LoadRecord(pk)
					Expect(err).NotTo(HaveOccurred())
					Expect(record).NotTo(BeNil())
					Expect(record.Version).NotTo(BeNil())
					Expect(record.Version.IsComplete()).To(BeTrue())
				}
				before := partitionProtocolRows(rc, root)
				// Include new, buffered versions in the same interval as committed ones.
				_, err := store.SaveRecord(partitionOrder(30, -12, 30))
				Expect(err).NotTo(HaveOccurred())
				Expect(rc.HasVersionMutations()).To(BeTrue())
				for mode := 0; mode < 10; mode++ {
					Expect(store.DeleteRecordsWhere(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2 - mode)})).To(Succeed())
				}
				Expect(partitionProtocolRows(rc, root)).To(Equal(before), "prefix deletion retains active existence")
				for _, key := range []int{RecordKey, RecordVersionKey} {
					r, err := fdb.PrefixRange(root.Sub(key).Pack(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2)}))
					Expect(err).NotTo(HaveOccurred())
					rows, err := rc.Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(BeEmpty())
					Expect(rc.HasVersionMutationsInRange(r.Begin.FDBKey(), r.End.FDBKey())).To(BeFalse())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := open(rc)
				records, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(records).To(HaveLen(1))
				Expect(records[0].Record.(*demopb.Order).GetOrderId()).To(Equal(int64(20)))
				Expect(records[0].Version).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("routes the legacy bare-key batch fallback without losing duplicate order or versions", func() {
		md, err := partitionTestBuilder().SetSplitLongRecords(false).SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		open := func(rc *FDBRecordContext) *FDBRecordStore {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(4).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.omitUnsplitRecordSuffix()).To(BeTrue())
			return store
		}
		var pk tuple.Tuple
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc)
			rows, err := store.SaveRecordBatchWithOptions([]proto.Message{partitionOrder(1, -11, 7), partitionOrder(1, -11, 8)}, BatchSaveOptions{CoalesceIndexWriteConflicts: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(2))
			pk = rows[1].PrimaryKey
			Expect(rows[1].Record.(*demopb.Order).GetPrice()).To(Equal(int32(8)))
			Expect(rc.Transaction().Get(root.Sub(RecordKey).Pack(pk)).MustGet()).NotTo(BeNil())
			Expect(rc.Transaction().Get(root.Sub(RecordKey).Pack(append(append(tuple.Tuple{}, pk...), int64(0)))).MustGet()).To(BeNil())
			Expect(partitionProtocolRows(rc, root)).To(HaveLen(4))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc)
			row, err := store.LoadRecord(pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.Record.(*demopb.Order).GetPrice()).To(Equal(int32(8)))
			Expect(row.Version.IsComplete()).To(BeTrue())
			before := partitionProtocolRows(rc, root)
			Expect(store.DeleteRecordsWhere(pk[:2])).To(Succeed())
			Expect(partitionProtocolRows(rc, root)).To(Equal(before))
			rows, err := rc.Transaction().GetRange(root.Sub(RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, batch := range []bool{false, true} {
		It(fmt.Sprintf("preserves duplicate-key input order and rolls back late group failures (batch=%v)", batch), func() {
			b := partitionTestBuilder().SetStoreRecordVersions(true)
			idx := NewIndex("group_unique", Concat(PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("price"))).SetUnique()
			b.AddIndex("Order", idx)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				input := []proto.Message{partitionOrder(1, -11, 7), partitionOrder(1, -11, 8), partitionOrder(2, -12, 9)}
				var rows []*FDBStoredRecord[proto.Message]
				if batch {
					rows, err = store.SaveRecordBatch(input)
				} else {
					rows, err = store.SaveRecordsPipelined(input, RecordExistenceCheckNone)
				}
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(3))
				Expect(rows[0].Record.(*demopb.Order).GetPrice()).To(Equal(int32(7)))
				Expect(rows[1].Record.(*demopb.Order).GetPrice()).To(Equal(int32(8)))
				loaded, err := store.LoadRecord(rows[1].PrimaryKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded.Record.(*demopb.Order).GetPrice()).To(Equal(int32(8)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			rc, err := sharedDB.OpenRecordContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer rc.Cancel()
			store := partitionProtocolStore(rc, md, root)
			before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			input := []proto.Message{partitionOrder(10, 10, 1), partitionOrder(11, 10, 1)}
			if batch {
				_, err = store.SaveRecordBatch(input)
			} else {
				_, err = store.SaveRecordsPipelined(input, RecordExistenceCheckNone)
			}
			Expect(err).To(HaveOccurred())
			Expect(rc.Transaction().Get(root.Sub(RecordKey).Pack(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(1), int64(10), int64(10), int64(0)})).MustGet()).NotTo(BeNil(), "first row really staged before failure")
			var incomplete *PartitionMutationError
			Expect(errors.As(rc.Commit(), &incomplete)).To(BeTrue())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("keeps dry-run, invalid-route and batch preflight failures committable without extension keys", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			preview, err := store.DryRunSaveRecord(partitionOrder(1, -11, 7), RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())
			Expect(preview.PrimaryKey[1]).To(Equal(int64(-2)))
			found, err := store.DryRunDeleteRecord(preview.PrimaryKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
			_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1)})
			var routing *PartitionRoutingError
			Expect(errors.As(err, &routing)).To(BeTrue())
			_, err = store.SaveRecordBatch([]proto.Message{partitionOrder(1, -11, 7), &demopb.Order{OrderId: proto.Int64(2)}})
			Expect(errors.As(err, &routing)).To(BeTrue())
			func() {
				op, err := store.beginPartitionMutation()
				Expect(err).NotTo(HaveOccurred())
				defer op.finish()
				rt := md.GetRecordType("Order")
				err = op.prepareRoute(rt, tuple.Tuple{rt.GetRecordTypeKey(), int64(math.MaxInt64), int64(0), int64(1)}, nil, false)
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue())
				Expect(state.Reason).To(Equal("coordinate is outside the source domain"))
			}()
			Expect(partitionProtocolRows(rc, root)).To(BeEmpty())
			after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			return nil, nil // The transaction deliberately commits after every refusal.
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, corruption := range []string{"header missing", "all routing state missing", "active missing", "active and fence missing", "fence missing", "generation mismatch", "malformed header", "unsupported version", "layout mismatch", "malformed fence", "inactive with records", "malformed active", "epoch missing", "epoch overflow", "generation overflow"} {
		It("fails closed and remains committable before effects: "+corruption, func() {
			md, err := partitionTestBuilder().Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				_, err := store.SaveRecord(partitionOrder(1, -11, 7))
				Expect(err).NotTo(HaveOccurred())
				ss := partitionSubspace(root, md.GetRecordType("Order"))
				switch corruption {
				case "header missing":
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"header"}))
				case "all routing state missing":
					rc.Transaction().ClearRange(ss)
				case "active missing":
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"active", int64(-2)}))
				case "active and fence missing":
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"active", int64(-2)}))
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"fence", int64(-2)}))
				case "fence missing":
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"fence", int64(-2)}))
				case "generation mismatch":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"active", int64(-2)}), (tuple.Tuple{int64(2)}).Pack())
				case "malformed header":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"header"}), []byte{0xff})
				case "unsupported version":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"header"}), (tuple.Tuple{int64(2), int64(6), int64(3), int64(10), make([]byte, 16)}).Pack())
				case "layout mismatch":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"header"}), (tuple.Tuple{int64(1), int64(6), int64(3), int64(11), make([]byte, 16)}).Pack())
				case "malformed fence":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"fence", int64(-2)}), (tuple.Tuple{int64(1), "active"}).Pack())
				case "inactive with records":
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"active", int64(-2)}))
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"fence", int64(-2)}), (tuple.Tuple{int64(1), false}).Pack())
				case "malformed active":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"active", int64(-2)}), []byte{0xff})
				case "epoch missing":
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"epoch"}))
				case "epoch overflow":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"epoch"}), (tuple.Tuple{int64(math.MaxInt64)}).Pack())
				case "generation overflow":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"fence", int64(1)}), (tuple.Tuple{int64(math.MaxInt64), false}).Pack())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				source := int64(-11)
				if corruption == "epoch missing" || corruption == "epoch overflow" || corruption == "generation overflow" {
					source = 10
				}
				_, err = store.SaveRecord(partitionOrder(2, source, 8))
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
				Expect(state.RecordType).To(Equal("Order"))
				after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, orphan := range []string{"committed legacy", "pending version", "local version cache"} {
		It("does not bootstrap over orphaned "+orphan+" data", func() {
			md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			legacyKey := root.Sub(RecordVersionKey).Pack(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)})
			if orphan == "committed legacy" {
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := partitionProtocolStore(rc, md, root)
					version, err := IncompleteVersion(rc.ClaimLocalVersion())
					Expect(err).NotTo(HaveOccurred())
					return nil, store.saveVersionWithOldFormat(legacyKey, version)
				})
				Expect(err).NotTo(HaveOccurred())
			}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				switch orphan {
				case "committed legacy":
					Expect(rc.Transaction().Get(legacyKey).MustGet()).NotTo(BeNil())
				case "pending version":
					version, err := IncompleteVersion(rc.ClaimLocalVersion())
					Expect(err).NotTo(HaveOccurred())
					Expect(store.saveVersionWithOldFormat(legacyKey, version)).To(Succeed())
					rc.RemoveLocalVersion(legacyKey)
					Expect(rc.HasVersionMutations()).To(BeTrue())
				case "local version cache":
					rc.AddToLocalVersionCache(legacyKey, 7)
					_, cached := rc.GetLocalVersion(legacyKey)
					Expect(cached).To(BeTrue())
				}
				bodies, err := rc.Transaction().GetRange(root.Sub(RecordKey), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(bodies).To(BeEmpty(), "the version-only branch, not a body probe, must detect corruption")
				before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(partitionOrder(2, -11, 8))
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue())
				Expect(state.Reason).To(Equal("record or version data exist without a routing header"))
				Expect(partitionProtocolRows(rc, root)).To(BeEmpty())
				after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("uses a tombstone's next generation without arithmetic overflow", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			Expect(store.ClearPartitionData("Order", -2)).To(Succeed())
			ss := partitionSubspace(root, md.GetRecordType("Order"))
			rc.Transaction().Clear(ss.Pack(tuple.Tuple{"active", int64(-2)}))
			rc.Transaction().Set(ss.Pack(tuple.Tuple{"fence", int64(-2)}), (tuple.Tuple{int64(8), false}).Pack())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(2, -12, 8))
			Expect(err).NotTo(HaveOccurred())
			ss := partitionSubspace(root, md.GetRecordType("Order"))
			Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"active", int64(-2)})).MustGet()).To(Equal((tuple.Tuple{int64(9)}).Pack()))
			Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"fence", int64(-2)})).MustGet()).To(Equal((tuple.Tuple{int64(9), true}).Pack()))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("fences loaded-record deletion and refuses missing routing state without clearing a body", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		var pk tuple.Tuple
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			saved, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			pk = saved.PrimaryKey
			rc.Transaction().Clear(partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"fence", int64(-2)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			found, err := store.DeleteRecord(pk)
			Expect(found).To(BeFalse())
			var state *PartitionStateError
			Expect(errors.As(err, &state)).To(BeTrue())
			after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("resets both tables with fresh incarnations and removes pending/legacy version state before reinsertion", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(1), ValInt64: proto.Int64(10)})
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(2, -11, 8))
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.HasVersionMutations()).To(BeTrue())
			oldHeaders := make(map[string][]byte)
			for _, name := range []string{"Order", "TypedRecord"} {
				oldHeaders[name] = rc.Transaction().Get(partitionSubspace(root, md.GetRecordType(name)).Pack(tuple.Tuple{"header"})).MustGet()
			}
			Expect(store.DeleteAllRecords()).To(Succeed())
			Expect(rc.HasVersionMutations()).To(BeFalse())
			Expect(rc.localVersionCache).To(BeEmpty())
			Expect(partitionProtocolRows(rc, root)).To(HaveLen(4), "only two immutable headers and two separate epochs survive")
			for _, name := range []string{"Order", "TypedRecord"} {
				ss := partitionSubspace(root, md.GetRecordType(name))
				Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"header"})).MustGet()).NotTo(Equal(oldHeaders[name]))
				Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"epoch"})).MustGet()).To(Equal((tuple.Tuple{int64(1)}).Pack()))
			}
			_, err = store.SaveRecord(partitionOrder(3, -11, 9))
			Expect(err).NotTo(HaveOccurred())
			Expect(partitionProtocolRows(rc, root)).To(HaveLen(6))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			rows, err := AsList(ctx, partitionProtocolStore(rc, md, root).ScanRecords(nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].Record.(*demopb.Order).GetOrderId()).To(Equal(int64(3)))
			Expect(rows[0].Version.IsComplete()).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears real legacy versions and resets first-use state across delete-all and delete-store", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		open := func(rc *FDBRecordContext) *FDBRecordStore {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(5).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			return store
		}
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := open(rc).SaveRecord(partitionOrder(1, -11, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := open(rc)
			versions, err := rc.Transaction().GetRange(root.Sub(RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(versions).To(HaveLen(1), "real committed legacy version")
			_, err = store.SaveRecord(partitionOrder(2, -12, 8))
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.HasVersionMutations()).To(BeTrue())
			Expect(store.DeleteAllRecords()).To(Succeed())
			Expect(rc.HasVersionMutations()).To(BeFalse())
			Expect(rc.localVersionCache).To(BeEmpty())
			versions, err = rc.Transaction().GetRange(root.Sub(RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(versions).To(BeEmpty())
			_, err = store.SaveRecord(partitionOrder(3, -11, 9))
			Expect(err).NotTo(HaveOccurred())
			Expect(DeleteStore(rc, root)).To(Succeed())
			Expect(partitionProtocolRows(rc, root)).To(BeEmpty())
			Expect(rc.HasVersionMutations()).To(BeFalse())
			_, err = open(rc).SaveRecord(partitionOrder(4, -11, 10))
			Expect(err).NotTo(HaveOccurred())
			Expect(partitionProtocolRows(rc, root)).To(HaveLen(4))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			rows, err := AsList(ctx, open(rc).ScanRecords(nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].Record.(*demopb.Order).GetOrderId()).To(Equal(int64(4)))
			Expect(rows[0].Version.IsComplete()).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses unfenced public raw index mutation and leaves plain/nil controls unchanged", func() {
		b := partitionTestBuilder()
		partitioned := NewIndex("raw_partitioned", Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
		plain := NewIndex("raw_plain", Field("name"))
		b.AddIndex("Order", partitioned)
		b.AddIndex("Customer", plain)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			Expect(store.DeleteIndexEntries(nil)).To(Succeed())
			_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("x")})
			Expect(err).NotTo(HaveOccurred())
			plainRows, err := rc.Transaction().GetRange(store.indexSubspace(plain), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(plainRows).To(HaveLen(1))
			Expect(store.DeleteIndexEntriesInRange(plain, tuple.Tuple{"x"})).To(Succeed())
			plainRows, err = rc.Transaction().GetRange(store.indexSubspace(plain), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(plainRows).To(BeEmpty())
			_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("y")})
			Expect(err).NotTo(HaveOccurred())
			plainRows, err = rc.Transaction().GetRange(store.indexSubspace(plain), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(plainRows).To(HaveLen(1))
			Expect(store.DeleteIndexEntries(plain)).To(Succeed())
			plainRows, err = rc.Transaction().GetRange(store.indexSubspace(plain), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(plainRows).To(BeEmpty())
			_, err = store.GetIndexMaintainer(plain)
			Expect(err).NotTo(HaveOccurred())
			before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			_, maintainerErr := store.GetIndexMaintainer(partitioned)
			raw := NewStandardIndexMaintainer(IndexMaintainerState{Store: store, Context: rc, Index: partitioned, IndexSubspace: store.indexSubspace(partitioned), Transaction: rc.Transaction()})
			for _, err := range []error{maintainerErr, store.DeleteIndexEntries(partitioned), store.DeleteIndexEntriesInRange(partitioned, tuple.Tuple{int64(-2)}), raw.Update(nil, &FDBStoredRecord[proto.Message]{}), raw.UpdateWhileWriteOnly(nil, &FDBStoredRecord[proto.Message]{}), raw.DeleteWhere(tuple.Tuple{int64(-2)})} {
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
				Expect(state.IndexName).To(Equal(partitioned.Name))
			}
			after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("routes uniqueness-resolution deletes while retaining the selected survivor and active generation", func() {
		b := partitionTestBuilder()
		idx := NewIndex("resolve_partition_unique", Concat(PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("price"))).SetUnique()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.ClearAndMarkIndexWriteOnly(idx.Name)
			Expect(err).NotTo(HaveOccurred())
			first, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			second, err := store.SaveRecord(partitionOrder(2, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			valueKey := tuple.Tuple{int64(-2), int64(-11), int64(7)}
			violations, err := store.ScanUniquenessViolationsForValue(idx, valueKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(violations).To(HaveLen(2))
			before := partitionProtocolRows(rc, root)
			Expect(store.ResolveUniquenessViolationByDeletion(idx, valueKey, first.PrimaryKey)).To(Succeed())
			Expect(partitionProtocolRows(rc, root)).To(Equal(before))
			retained, err := store.LoadRecord(first.PrimaryKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(retained).NotTo(BeNil())
			deleted, err := store.LoadRecord(second.PrimaryKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("retains active state through caller-excluded cleanup and conflicts a prefix clear with an admitted writer", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			before := partitionProtocolRows(rc, root)
			Expect(store.ClearPartitionData("Order", -2)).To(Succeed())
			Expect(partitionProtocolRows(rc, root)).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		a, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer a.Cancel()
		b, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer b.Cancel()
		storeA, storeB := partitionProtocolStore(a, md, root), partitionProtocolStore(b, md, root)
		Expect(storeA.DeleteRecordsWhere(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey()})).To(Succeed())
		_, err = storeB.SaveRecord(partitionOrder(2, -12, 8))
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Commit()).To(Succeed())
		var conflict fdb.Error
		Expect(errors.As(a.Commit(), &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			rows, err := AsList(ctx, partitionProtocolStore(rc, md, root).ScanRecords(nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(bytes.Equal(rows[0].PrimaryKey.Pack(), tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-12), int64(2)}.Pack())).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("uses a table-wide fence conflict to exclude even an active writer outside a prefix clear", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(partitionOrder(2, 10, 8))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		a, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer a.Cancel()
		b, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer b.Cancel()
		storeA, storeB := partitionProtocolStore(a, md, root), partitionProtocolStore(b, md, root)
		Expect(storeA.DeleteRecordsWhere(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2)})).To(Succeed())
		_, err = storeB.SaveRecord(partitionOrder(3, 10, 9))
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Commit()).To(Succeed())
		var conflict fdb.Error
		Expect(errors.As(b.Commit(), &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
	})
})
