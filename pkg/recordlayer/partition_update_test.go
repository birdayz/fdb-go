package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func partitionMoveBuilder(sourceFirst bool) *RecordMetaDataBuilder {
	b := partitionTestBuilder().SetStoreRecordVersions(true).SetSplitLongRecords(true)
	if !sourceFirst {
		b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), PartitionExpr(Field("coord_x"), 10), Field("order_id"), Field("coord_x")))
	}
	b.AddIndex("Order", NewIndex("move_value", Concat(PartitionExpr(Field("coord_x"), 10), Field("price"))))
	b.AddIndex("Order", NewIndex("move_cover", KeyWithValue(Concat(PartitionExpr(Field("coord_x"), 10), Field("order_id"), Nest("flower", Field("type"))), 2)))
	b.AddIndex("Order", NewTextIndex("move_text", GroupBy(Nest("flower", Field("type")), PartitionExpr(Field("coord_x"), 10), Field("order_id"))))
	b.AddIndex("Order", NewIndex("move_unique", Concat(PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("price"))).SetUnique())
	return b
}

func partitionMoveKey(md *RecordMetaData, sourceFirst bool, id, source, coordinate int64) tuple.Tuple {
	key := tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), coordinate}
	if sourceFirst {
		return append(key, source, id)
	}
	return append(key, id, source)
}

func partitionMoveRecord(id, source int64, price int32, name string) *demopb.Order {
	row := partitionOrder(id, source, price)
	row.Flower = &demopb.Flower{Type: proto.String(name)}
	return row
}

func partitionMoveApply(ctx context.Context, store *FDBRecordStore, updates ...PartitionRecordUpdate) ([]*FDBStoredRecord[proto.Message], error) {
	group, err := store.PreparePartitionRecordUpdates(updates)
	if err != nil {
		return nil, err
	}
	return group.Apply(ctx, false)
}

type partitionMoveFilter struct {
	normalMaintenanceFilter
	callback func(*Index, proto.Message)
}

func (filter *partitionMoveFilter) MaintainIndex(index *Index, record proto.Message) IndexValues {
	if filter.callback != nil {
		filter.callback(index, record)
	}
	return IndexValuesAll
}

type partitionMoveTokenizer struct {
	TextTokenizer
	callback func()
}

func (tokenizer *partitionMoveTokenizer) TokenizeToMap(text string, version int, mode TokenizerMode) (map[string][]int, error) {
	tokenizer.callback()
	return tokenizer.TextTokenizer.TokenizeToMap(text, version, mode)
}

var _ = Describe("Atomic partition record updates", func() {
	ctx := context.Background()
	for _, sourceFirst := range []bool{false, true} {
		for _, format := range []int32{4, 14} {
			It(fmt.Sprintf("moves bodies, split/inline/legacy versions and secondary identities sourceFirst=%t format=%d", sourceFirst, format), func() {
				md, err := partitionMoveBuilder(sourceFirst).Build()
				Expect(err).NotTo(HaveOccurred())
				root := specSubspace()
				open := func(rc *FDBRecordContext) *FDBRecordStore {
					store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(format).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					return store
				}
				// Independent explicit coordinates exercise all three signs, same
				// interval key changes, unchanged self-key, and repeated IDs.
				type move struct{ id, from, oldP, to, newP int64 }
				moves := []move{{1, -11, -2, -12, -2}, {1, 0, 0, 10, 1}, {2, 10, 1, -1, -1}, {3, -1, -1, 0, 0}, {4, 21, 2, 21, 2}, {5, 32, 3, 100, 10}}
				longBody := []byte(strings.Repeat("split ", 20000))
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := open(rc)
					for i, move := range moves {
						record := partitionMoveRecord(move.id, move.from, int32(i+7), "old red")
						if i == 0 {
							record.VectorData = longBody
						}
						_, err := store.SaveRecord(record)
						Expect(err).NotTo(HaveOccurred())
					}
					_, err := store.SaveRecord(partitionMoveRecord(99, 22, 99, "neighbor blue"))
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := open(rc)
					beforeProtocol := partitionProtocolRows(rc, root)
					beforeNeighbor, err := store.LoadRecord(partitionMoveKey(md, sourceFirst, 99, 22, 2))
					Expect(err).NotTo(HaveOccurred())
					Expect(beforeNeighbor.Record.(*demopb.Order).GetFlower().GetType()).To(Equal("neighbor blue"))
					var updates []PartitionRecordUpdate
					for i, move := range moves {
						record := partitionMoveRecord(move.id, move.to, int32(i+17), "new green")
						if i == 0 {
							record.VectorData = append(append([]byte{}, longBody...), 'n')
						}
						updates = append(updates, PartitionRecordUpdate{OldPrimaryKey: partitionMoveKey(md, sourceFirst, move.id, move.from, move.oldP), Record: record})
					}
					// First five destinations are already active: none may rewrite
					// header, epoch, active or fence bytes.
					results, err := partitionMoveApply(ctx, store, updates[:5]...)
					Expect(err).NotTo(HaveOccurred())
					Expect(results).To(HaveLen(5))
					Expect(partitionProtocolRows(rc, root)).To(Equal(beforeProtocol))
					results, err = partitionMoveApply(ctx, store, updates[5])
					Expect(err).NotTo(HaveOccurred())
					Expect(results[0].PrimaryKey).To(Equal(partitionMoveKey(md, sourceFirst, 5, 100, 10)))
					ss := partitionSubspace(root, md.GetRecordType("Order"))
					for _, p := range []int64{-2, -1, 0, 1, 2, 3, 10} {
						Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"active", p})).MustGet()).To(Equal(tuple.Tuple{int64(1)}.Pack()))
						Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"fence", p})).MustGet()).To(Equal(tuple.Tuple{int64(1), true}.Pack()))
					}
					afterNeighbor, err := store.LoadRecord(partitionMoveKey(md, sourceFirst, 99, 22, 2))
					Expect(err).NotTo(HaveOccurred())
					Expect(proto.Equal(afterNeighbor.Record, beforeNeighbor.Record)).To(BeTrue())
					Expect(afterNeighbor.Version.Equal(beforeNeighbor.Version)).To(BeTrue(), "unselected neighbor body and version remain untouched")
					// Old deferred versions are gone, including a pending replacement
					// saved and moved again in this transaction.
					pending, err := store.SaveRecord(partitionMoveRecord(8, -11, 88, "pending red"))
					Expect(err).NotTo(HaveOccurred())
					_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{OldPrimaryKey: pending.PrimaryKey, Record: partitionMoveRecord(8, 111, 89, "pending green")})
					Expect(err).NotTo(HaveOccurred())
					_, local := rc.GetLocalVersion(store.versionKey(pending.PrimaryKey))
					Expect(local).To(BeFalse())
					Expect(partitionVersionEffectsOf(rc).mutations).NotTo(HaveKey(string(store.versionKey(pending.PrimaryKey))))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := open(rc)
					all, err := AsList(ctx, store.ScanRecordsByType("Order", nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(all).To(HaveLen(8))
					for i, move := range moves {
						oldKey := partitionMoveKey(md, sourceFirst, move.id, move.from, move.oldP)
						newKey := partitionMoveKey(md, sourceFirst, move.id, move.to, move.newP)
						if !tuplesEqual(oldKey, newKey) {
							old, err := store.LoadRecord(oldKey)
							Expect(err).NotTo(HaveOccurred())
							Expect(old).To(BeNil())
							body, err := rc.Transaction().GetRange(store.recordsSubspace.Sub(oldKey...), fdb.RangeOptions{}).GetSliceWithError()
							Expect(err).NotTo(HaveOccurred())
							Expect(body).To(BeEmpty(), "old splits and inline versions must all disappear")
							Expect(rc.Transaction().Get(store.versionKey(oldKey)).MustGet()).To(BeNil())
						}
						row, err := store.LoadRecord(newKey)
						Expect(err).NotTo(HaveOccurred())
						Expect(row.PrimaryKey).To(Equal(newKey))
						Expect(row.Record.(*demopb.Order).GetPrice()).To(Equal(int32(i + 17)))
						Expect(row.Version.IsComplete()).To(BeTrue())
						Expect(row.VersionedInline).To(Equal(format >= 6))
						Expect(row.Split).To(Equal(i == 0))
						valueKey := tuple.Tuple{move.newP, int64(i + 17), md.GetRecordType("Order").GetRecordTypeKey()}
						if sourceFirst {
							valueKey = append(valueKey, move.to, move.id)
						} else {
							valueKey = append(valueKey, move.id, move.to)
						}
						Expect(rc.Transaction().Get(store.indexSubspace(md.GetIndex("move_value")).Pack(valueKey)).MustGet()).NotTo(BeNil(), "independent exact NEW VALUE entry")
					}
					for _, name := range []string{"move_value", "move_cover", "move_unique"} {
						index := md.GetIndex(name)
						Expect(index).NotTo(BeNil())
						entries, err := AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
						Expect(err).NotTo(HaveOccurred())
						Expect(entries).To(HaveLen(8))
						for _, entry := range entries {
							row, err := store.LoadRecord(index.EntryPrimaryKey(entry.Key))
							Expect(err).NotTo(HaveOccurred())
							Expect(row).NotTo(BeNil(), "every VALUE/covering entry reconstructs the NEW physical key")
						}
					}
					text := md.GetIndex("move_text")
					Expect(text).NotTo(BeNil())
					maintainer, err := store.getIndexMaintainer(text)
					Expect(err).NotTo(HaveOccurred())
					tm := maintainer.(*textIndexMaintainer)
					for _, move := range moves {
						newKey := partitionMoveKey(md, sourceFirst, move.id, move.to, move.newP)
						version, err := tm.getRecordTokenizerVersion(newKey)
						Expect(err).NotTo(HaveOccurred())
						Expect(version).To(Equal(tm.tokenizerVersion))
						Expect(rc.Transaction().Get(tm.getRecordTokenizerKey(newKey)).MustGet()).To(Equal(tuple.Tuple{int64(tm.tokenizerVersion)}.Pack()), "absence must not masquerade as default tokenizer version zero")
						posting, err := NewTextScan(text, tuple.Tuple{move.newP, move.id}, NewTextComparison(TextContainsAll, "green", "", ""), nil).Scan(store, nil, ForwardScan())
						Expect(err).NotTo(HaveOccurred())
						entries, err := AsList(ctx, posting)
						Expect(err).NotTo(HaveOccurred())
						Expect(entries).To(HaveLen(1))
						Expect(text.EntryPrimaryKey(entries[0].Key)).To(Equal(newKey))
						oldPosting, err := NewTextScan(text, tuple.Tuple{move.oldP, move.id}, NewTextComparison(TextContainsAll, "red", "", ""), nil).Scan(store, nil, ForwardScan())
						Expect(err).NotTo(HaveOccurred())
						oldEntries, err := AsList(ctx, oldPosting)
						Expect(err).NotTo(HaveOccurred())
						Expect(oldEntries).To(BeEmpty(), "old token postings must migrate, not only tokenizer metadata")
						if move.from != move.to {
							oldKey := partitionMoveKey(md, sourceFirst, move.id, move.from, move.oldP)
							Expect(rc.Transaction().Get(tm.getRecordTokenizerKey(oldKey)).MustGet()).To(BeNil())
						}
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}

	for _, collision := range []string{"duplicate old", "duplicate final", "selected to vacate", "swap", "cycle", "unselected"} {
		It("rejects immediate destination collision without effects: "+collision, func() {
			md, err := partitionMoveBuilder(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				for _, source := range []int64{-11, 0, 10} {
					_, err := store.SaveRecord(partitionMoveRecord(1, source, 7, "red"))
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			var before []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				before = partitionWholeStore(rc, store)
				effects := partitionVersionEffectsOf(rc)
				key := func(source, p int64) tuple.Tuple { return partitionMoveKey(md, true, 1, source, p) }
				row := func(from, p, to int64) PartitionRecordUpdate {
					return PartitionRecordUpdate{key(from, p), partitionMoveRecord(1, to, 8, "green")}
				}
				updates := []PartitionRecordUpdate{row(-11, -2, 0)}
				switch collision {
				case "duplicate old":
					updates = []PartitionRecordUpdate{row(-11, -2, -12), row(-11, -2, 20)}
				case "duplicate final":
					updates = []PartitionRecordUpdate{row(-11, -2, 20), row(0, 0, 20)}
				case "selected to vacate":
					updates = append(updates, row(0, 0, 20))
				case "swap":
					updates = append(updates, row(0, 0, -11))
				case "cycle":
					updates = append(updates, row(0, 0, 10), row(10, 1, -11))
				}
				_, err := partitionMoveApply(ctx, store, updates...)
				if collision == "duplicate old" {
					var duplicate *RecordCoreArgumentError
					Expect(errors.As(err, &duplicate)).To(BeTrue())
				} else {
					var occupied *RecordAlreadyExistsError
					Expect(errors.As(err, &occupied)).To(BeTrue())
				}
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				return nil, nil // Deliberately commit every pure first-row refusal.
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				Expect(partitionWholeStore(rc, partitionProtocolStore(rc, md, root))).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("snapshots prepared messages and keys, refuses plain/invalid/reused groups, and previews without version effects", func() {
		md, err := partitionMoveBuilder(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
			Expect(err).NotTo(HaveOccurred())
			old := partitionMoveKey(md, true, 1, -11, -2)
			message := partitionMoveRecord(1, 0, 8, "green")
			group, err := store.PreparePartitionRecordUpdates([]PartitionRecordUpdate{{old, message}})
			Expect(err).NotTo(HaveOccurred())
			Expect(group.RetainedMemoryBytes()).To(BeNumerically(">", 2*proto.Size(message)+len(old.Pack())))
			old[2] = int64(900)
			message.CoordX, message.Price = proto.Int64(900), proto.Int32(99)
			before, effects, local := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
			preview, err := group.Apply(ctx, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(preview).To(HaveLen(1))
			Expect(preview[0].PrimaryKey).To(Equal(partitionMoveKey(md, true, 1, 0, 0)))
			Expect(preview[0].Record.(*demopb.Order).GetPrice()).To(Equal(int32(8)))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			Expect(rc.localVersion.Load()).To(Equal(local))
			_, err = group.Apply(ctx, false)
			Expect(err).To(HaveOccurred())
			var empty PreparedPartitionRecordUpdates
			_, err = empty.Apply(ctx, false)
			Expect(err).To(HaveOccurred())
			for _, invalid := range []PartitionRecordUpdate{
				{Record: message},
				{OldPrimaryKey: tuple.Tuple{md.GetRecordType("Customer").GetRecordTypeKey(), int64(1)}, Record: &demopb.Customer{CustomerId: proto.Int64(1)}},
				{OldPrimaryKey: partitionMoveKey(md, true, 1, -11, 0), Record: message},
			} {
				_, err := store.PreparePartitionRecordUpdates([]PartitionRecordUpdate{invalid})
				Expect(err).To(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("distinguishes incarnation read failures from observed callback mutations without staging effects", func() {
		md, err := partitionMoveBuilder(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before, effects := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
			identity := partitionMutation{store: store}
			Expect(identity.captureIncarnations()).To(Succeed())
			Expect(identity.headers).To(HaveLen(2), "both partitioned types are observed, including an absent header")
			changed, err := identity.validateIncarnations()
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeFalse())
			sentinel := errors.New("incarnation header read refused")
			native := rc.tx
			fault := &lifecycleLegacyFault{
				WritableTransaction: native,
				versionKey:          partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"header"}),
				armed:               true, sentinel: sentinel, onIssue: func() error { return nil },
			}
			rc.tx = fault
			changed, err = identity.validateIncarnations()
			rc.tx = native
			Expect(changed).To(BeFalse(), "a backend error alone does not establish staged callback effects")
			Expect(errors.Is(err, sentinel)).To(BeTrue())
			var state *PartitionStateError
			Expect(errors.As(err, &state)).To(BeTrue())
			Expect(state.Reason).To(Equal("cannot revalidate partition incarnation"))
			Expect(fault.future).NotTo(BeNil())
			Expect(fault.future.gets.Load()).To(Equal(int64(1)))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			return nil, nil // Deliberate Commit remains legal for a read-only refusal.
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, reset := range []string{"DeleteAllRecords", "DeleteStore"} {
		It("refuses a prepared group's replaced incarnation after "+reset+" and accepts a fresh-context retry", func() {
			md, err := partitionMoveBuilder(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			oldKey := partitionMoveKey(md, true, 1, -11, -2)
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				_, err := store.SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
				Expect(err).NotTo(HaveOccurred())
				group, err := store.PreparePartitionRecordUpdates([]PartitionRecordUpdate{{oldKey, partitionMoveRecord(1, 10, 8, "green")}})
				Expect(err).NotTo(HaveOccurred())
				if reset == "DeleteAllRecords" {
					err = store.DeleteAllRecords()
				} else {
					err = DeleteStore(rc, root)
					store = partitionProtocolStore(rc, md, root)
				}
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
				Expect(err).NotTo(HaveOccurred())
				before, effects := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
				_, err = group.Apply(ctx, false)
				var changed *PartitionStateError
				Expect(errors.As(err, &changed)).To(BeTrue())
				Expect(changed.Reason).To(Equal("table incarnation changed during partition mutation"))
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				_, err = group.Apply(ctx, false)
				Expect(err).To(HaveOccurred(), "even a refused application consumes the group")
				return nil, nil // The completed reset is legal; the stale move staged nothing.
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				_, err := partitionMoveApply(ctx, partitionProtocolStore(rc, md, root), PartitionRecordUpdate{oldKey, partitionMoveRecord(1, 10, 8, "green")})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("binds a prepared group to its original context and consumes a closed-context refusal", func() {
		md, err := partitionMoveBuilder(true).Build()
		Expect(err).NotTo(HaveOccurred())
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		store := partitionProtocolStore(rc, md, specSubspace())
		group, err := store.PreparePartitionRecordUpdates([]PartitionRecordUpdate{{partitionMoveKey(md, true, 1, -11, -2), partitionMoveRecord(1, 10, 8, "green")}})
		Expect(err).NotTo(HaveOccurred())
		rc.Cancel()
		_, err = group.Apply(ctx, false)
		Expect(err).To(HaveOccurred())
		_, err = group.Apply(ctx, false)
		Expect(err).To(MatchError("partition update group was already applied"))
	})

	It("fences a competing fresh destination activation without publishing the losing move", func() {
		md, err := partitionMoveBuilder(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		winner, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer winner.Cancel()
		loser, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer loser.Cancel()
		_, err = partitionProtocolStore(winner, md, root).SaveRecord(partitionMoveRecord(99, 101, 99, "neighbor blue"))
		Expect(err).NotTo(HaveOccurred())
		oldKey := partitionMoveKey(md, true, 1, -11, -2)
		_, err = partitionMoveApply(ctx, partitionProtocolStore(loser, md, root), PartitionRecordUpdate{oldKey, partitionMoveRecord(1, 100, 8, "green")})
		Expect(err).NotTo(HaveOccurred())
		Expect(winner.Commit()).To(Succeed())
		var conflict fdb.Error
		Expect(errors.As(loser.Commit(), &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			old, err := store.LoadRecord(oldKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Record.(*demopb.Order).GetPrice()).To(Equal(int32(7)))
			missing, err := store.LoadRecord(partitionMoveKey(md, true, 1, 100, 10))
			Expect(err).NotTo(HaveOccurred())
			Expect(missing).To(BeNil())
			neighbor, err := store.LoadRecord(partitionMoveKey(md, true, 99, 101, 10))
			Expect(err).NotTo(HaveOccurred())
			Expect(neighbor.Record.(*demopb.Order).GetPrice()).To(Equal(int32(99)))
			before := partitionProtocolRows(rc, root)
			_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{oldKey, partitionMoveRecord(1, 100, 8, "green")})
			Expect(err).NotTo(HaveOccurred())
			Expect(partitionProtocolRows(rc, root)).To(Equal(before), "retry into the now-active destination does not rewrite protocol bytes")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("chooses one real version before serializer reentry and previews the same inline sizes without claims", func() {
		md, err := partitionMoveBuilder(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			other := partitionProtocolStore(rc, md, root)
			oldKey := partitionMoveKey(md, true, 1, -11, -2)
			update := PartitionRecordUpdate{oldKey, partitionMoveRecord(1, -11, 8, "green")}
			group, err := other.PreparePartitionRecordUpdates([]PartitionRecordUpdate{update})
			Expect(err).NotTo(HaveOccurred())
			before, effects, counter := partitionWholeStore(rc, other), partitionVersionEffectsOf(rc), rc.localVersion.Load()
			preview, err := group.Apply(ctx, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(preview[0].Version).NotTo(BeNil())
			Expect(preview[0].Version.GetLocalVersion()).To(Equal(int(counter)))
			Expect(preview[0].VersionedInline).To(BeTrue())
			Expect(rc.localVersion.Load()).To(Equal(counter))
			Expect(partitionWholeStore(rc, other)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			manager := &lifecycleKeyManager{SerializationKeyManager: NewFixedZeroKeyManager(make([]byte, 16), "", nil)}
			var innerVersion int
			manager.onWrite = func() error {
				Expect(rc.localVersion.Load()).To(Equal(counter+1), "outer version must precede the serializer")
				inner, err := other.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(99), Name: proto.String("unrelated")})
				if err == nil {
					innerVersion = inner.Version.GetLocalVersion()
				}
				return err
			}
			store, err := other.AsBuilder().SetSerializer(lifecycleCodec(manager)).Open()
			Expect(err).NotTo(HaveOccurred())
			stored, err := partitionMoveApply(ctx, store, update)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored[0].Version.GetLocalVersion()).To(Equal(int(counter)))
			Expect(innerVersion).To(Equal(int(counter + 1)))
			Expect(manager.writes).To(Equal(1))
			// Independently count the plain preview's protobuf union framing
			// and 13-byte tuple version, not the production sizing helper.
			Expect(preview[0].ValueSize).To(Equal(protowire.SizeTag(protowire.Number(md.GetRecordType("Order").unionFieldNumber)) + protowire.SizeBytes(proto.Size(update.Record)) + 13))
			Expect(preview[0].KeyCount).To(Equal(2))
			Expect(preview[0].KeySize).To(Equal(len(other.recordsSubspace.Pack(appendToTuple(oldKey, int64(0)))) + len(other.versionKey(oldKey))))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses hidden version dependencies in otherwise admitted metadata and preserves version-exhaustion precedence", func() {
		b := partitionTestBuilder().SetStoreRecordVersions(true)
		b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("order_id"), FunctionExpr(OrderFuncAscNullsFirst, VersionKey())))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred(), "hidden function version dependencies are admitted by declared-version validation")
		Expect(countVersionColumns(md.GetRecordType("Order").PrimaryKey)).To(BeZero())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
			_, err := store.PreparePartitionRecordUpdates([]PartitionRecordUpdate{{tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1), nil}, partitionMoveRecord(1, 0, 8, "green")}})
			var metadata *PartitionMetadataError
			Expect(errors.As(err, &metadata)).To(BeTrue())
			Expect(metadata.Message).To(Equal("partition update primary key cannot depend on a record version"))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			Expect(rc.localVersion.Load()).To(Equal(counter))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		md, err = partitionMoveBuilder(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root = specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			group, err := store.PreparePartitionRecordUpdates([]PartitionRecordUpdate{{partitionMoveKey(md, true, 1, -11, -2), partitionMoveRecord(1, 0, 8, "green")}})
			Expect(err).NotTo(HaveOccurred())
			rc.localVersion.Store(65536)
			before, effects := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
			_, err = group.Apply(ctx, false)
			Expect(err).To(HaveOccurred())
			var absent *RecordDoesNotExistError
			Expect(errors.As(err, &absent)).To(BeFalse(), "version exhaustion precedes OLD absence")
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, stage := range []string{"serializer", "serializer error", "VALUE filter", "TEXT maintenance", "TEXT tokenizer"} {
		for _, interference := range []string{"old data", "old version", "old ABA", "destination", "reset", "delete-store", "delete-store ABA"} {
			It("poisons callback effects from "+stage+" changing "+interference+" and permits a fresh-context retry", func() {
				md, err := partitionMoveBuilder(true).Build()
				Expect(err).NotTo(HaveOccurred())
				root := specSubspace()
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				rc, err := sharedDB.OpenRecordContext(ctx)
				Expect(err).NotTo(HaveOccurred())
				defer rc.Cancel()
				other, err := partitionProtocolStore(rc, md, root).AsBuilder().SetSerializer(lifecycleCodec(NewFixedZeroKeyManager(make([]byte, 16), "", nil))).Open()
				Expect(err).NotTo(HaveOccurred())
				before, effects := partitionWholeStore(rc, other), partitionVersionEffectsOf(rc)
				oldKey := partitionMoveKey(md, true, 1, -11, -2)
				newKey := partitionMoveKey(md, true, 1, 10, 1)
				calls := 0
				var callbackErr error
				callback := func() {
					if calls != 0 {
						return
					}
					calls++
					switch interference {
					case "old data":
						_, callbackErr = other.SaveRecord(partitionMoveRecord(1, -11, 99, "blue"))
					case "old version":
						v, err := IncompleteVersion(rc.ClaimLocalVersion())
						Expect(err).NotTo(HaveOccurred())
						_, callbackErr = other.SaveRecordWithVersion(partitionMoveRecord(1, -11, 7, "red"), RecordExistenceCheckNone, v, VersionstampSaveBehaviorIfPresent)
					case "old ABA":
						_, callbackErr = other.DeleteRecord(oldKey)
						if callbackErr == nil {
							_, callbackErr = other.SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
						}
					case "destination":
						_, callbackErr = other.SaveRecord(partitionMoveRecord(1, 10, 88, "intruder"))
					case "reset":
						callbackErr = other.DeleteAllRecords()
					case "delete-store":
						callbackErr = DeleteStore(rc, root)
					case "delete-store ABA":
						headerKey := partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"header"})
						oldHeader := rc.Transaction().Get(headerKey).MustGet()
						Expect(oldHeader).NotTo(BeEmpty())
						callbackErr = DeleteStore(rc, root)
						if callbackErr == nil {
							// Recreate the table and its active source with a different
							// PK; neither observed OLD nor destination key is rewritten.
							recreated := partitionProtocolStore(rc, md, root)
							_, callbackErr = recreated.SaveRecord(partitionMoveRecord(99, -11, 99, "ABA neighbor"))
							newHeader := rc.Transaction().Get(headerKey).MustGet()
							Expect(newHeader).NotTo(BeEmpty())
							Expect(newHeader).NotTo(Equal(oldHeader), "a new incarnation really replaced the prior header")
						}
					}
				}
				builder := other.AsBuilder()
				serializerFailure := errors.New("serializer refused after interference")
				var manager *lifecycleKeyManager
				if strings.HasPrefix(stage, "serializer") {
					manager = &lifecycleKeyManager{SerializationKeyManager: NewFixedZeroKeyManager(make([]byte, 16), "", nil), onWrite: func() error {
						callback()
						if stage == "serializer error" {
							return serializerFailure
						}
						return nil
					}}
					builder.SetSerializer(lifecycleCodec(manager))
				} else {
					builder.SetIndexMaintenanceFilter(&partitionMoveFilter{callback: func(index *Index, _ proto.Message) {
						if (stage == "VALUE filter" && index.Name == "move_value") || (stage == "TEXT maintenance" && index.Name == "move_text") {
							callback()
						}
					}})
				}
				store, err := builder.Open()
				Expect(err).NotTo(HaveOccurred())
				if stage == "TEXT tokenizer" {
					maintainer, err := store.getIndexMaintainer(md.GetIndex("move_text"))
					Expect(err).NotTo(HaveOccurred())
					text := maintainer.(*textIndexMaintainer)
					text.tokenizer = &partitionMoveTokenizer{TextTokenizer: text.tokenizer, callback: callback}
				}
				_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{oldKey, partitionMoveRecord(1, 10, 8, "green")})
				Expect(callbackErr).NotTo(HaveOccurred())
				Expect(calls).To(Equal(1), "callback population must be nonempty")
				Expect(err).To(HaveOccurred())
				if stage == "serializer error" {
					Expect(errors.Is(err, serializerFailure)).To(BeTrue(), "identity loss must not replace the exact earlier key-manager error")
				}
				if strings.HasPrefix(interference, "delete-store") && strings.HasPrefix(stage, "TEXT") {
					var identity *PartitionStateError
					Expect(errors.As(err, &identity)).To(BeTrue(), "post-maintenance identity loss must be returned, not only remembered by Commit")
					Expect(identity.Reason).To(Equal("table incarnation changed during partition mutation"))
				}
				if manager != nil {
					Expect(manager.writes).To(Equal(1), "no callback retry/second serialization")
				}
				Expect(partitionWholeStore(rc, other)).NotTo(Equal(before), "callback really staged effects")
				if interference != "reset" && interference != "delete-store" {
					Expect(partitionVersionEffectsOf(rc)).NotTo(Equal(effects))
				}
				var incomplete *PartitionMutationError
				Expect(errors.As(rc.Commit(), &incomplete)).To(BeTrue())
				_, err = sharedDB.Run(ctx, func(fresh *FDBRecordContext) (any, error) {
					store := partitionProtocolStore(fresh, md, root)
					Expect(partitionWholeStore(fresh, store)).To(Equal(before), "deliberate Commit cannot publish callback corruption")
					Expect(partitionVersionEffectsOf(fresh)).To(Equal(effects))
					_, err := partitionMoveApply(ctx, store, PartitionRecordUpdate{oldKey, partitionMoveRecord(1, 10, 8, "green")})
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(fresh *FDBRecordContext) (any, error) {
					store := partitionProtocolStore(fresh, md, root)
					old, err := store.LoadRecord(oldKey)
					Expect(err).NotTo(HaveOccurred())
					Expect(old).To(BeNil())
					newRow, err := store.LoadRecord(newKey)
					Expect(err).NotTo(HaveOccurred())
					Expect(newRow.Record.(*demopb.Order).GetPrice()).To(Equal(int32(8)))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}

	for _, failDecode := range []bool{false, true} {
		It(fmt.Sprintf("poisons destination insertion during OLD deserialization, including an early decode error (fail=%t)", failDecode), func() {
			md, err := partitionMoveBuilder(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			codec := lifecycleCodec(NewFixedZeroKeyManager(make([]byte, 16), "", nil))
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := partitionProtocolStore(rc, md, root).AsBuilder().SetSerializer(codec).Open()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(partitionMoveRecord(1, -11, 7, "red"))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			rc, err := sharedDB.OpenRecordContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer rc.Cancel()
			other, err := partitionProtocolStore(rc, md, root).AsBuilder().SetSerializer(codec).Open()
			Expect(err).NotTo(HaveOccurred())
			before, effects := partitionWholeStore(rc, other), partitionVersionEffectsOf(rc)
			manager := &lifecycleKeyManager{SerializationKeyManager: NewFixedZeroKeyManager(make([]byte, 16), "", nil)}
			manager.onKey = func() error {
				if manager.keys == 1 {
					_, err := other.SaveRecord(partitionMoveRecord(1, 10, 99, "intruder"))
					Expect(err).NotTo(HaveOccurred())
					if failDecode {
						return errors.New("OLD decrypt refused after insertion")
					}
				}
				return nil
			}
			store, err := other.AsBuilder().SetSerializer(lifecycleCodec(manager)).Open()
			Expect(err).NotTo(HaveOccurred())
			oldKey := partitionMoveKey(md, true, 1, -11, -2)
			_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{oldKey, partitionMoveRecord(1, 10, 8, "green")})
			Expect(err).To(HaveOccurred())
			if failDecode {
				var decode *RecordDeserializationError
				Expect(errors.As(err, &decode)).To(BeTrue())
			} else {
				var occupied *RecordAlreadyExistsError
				Expect(errors.As(err, &occupied)).To(BeTrue(), "destination data/error still follows OLD decoding")
			}
			Expect(manager.keys).To(Equal(1))
			Expect(manager.writes).To(BeZero(), "refusal precedes NEW serialization")
			Expect(partitionWholeStore(rc, other)).NotTo(Equal(before))
			var incomplete *PartitionMutationError
			Expect(errors.As(rc.Commit(), &incomplete)).To(BeTrue())
			_, err = sharedDB.Run(ctx, func(fresh *FDBRecordContext) (any, error) {
				store, err := partitionProtocolStore(fresh, md, root).AsBuilder().SetSerializer(codec).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(partitionWholeStore(fresh, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(fresh)).To(Equal(effects))
				_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{oldKey, partitionMoveRecord(1, 10, 8, "green")})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("keeps mutation-free UNIQUE refusals committable, retains WRITE_ONLY annotations and ignores only exact own removals", func() {
		md, err := partitionMoveBuilder(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			for _, record := range []proto.Message{partitionMoveRecord(1, -11, 7, "red"), partitionMoveRecord(2, 0, 8, "blue"), partitionMoveRecord(3, 0, 9, "neighbor")} {
				_, err := store.SaveRecord(record)
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before, effects := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
			_, err := partitionMoveApply(ctx, store, PartitionRecordUpdate{partitionMoveKey(md, true, 1, -11, -2), partitionMoveRecord(1, 0, 8, "conflict")})
			var unique *RecordIndexUniquenessViolationError
			Expect(errors.As(err, &unique)).To(BeTrue())
			Expect(unique.ExistingKey).To(Equal(partitionMoveKey(md, true, 2, 0, 0)))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			// Changing only ID keeps the unique value; permit precisely the
			// old entry removed by this move, not the neighbor sharing SOURCE.
			_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{partitionMoveKey(md, true, 2, 0, 0), partitionMoveRecord(22, 0, 8, "renamed")})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			index := md.GetIndex("move_unique")
			Expect(index).NotTo(BeNil())
			_, err = store.MarkIndexWriteOnly(index.Name)
			Expect(err).NotTo(HaveOccurred())
			_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{partitionMoveKey(md, true, 1, -11, -2), partitionMoveRecord(1, 0, 8, "write only")})
			Expect(err).NotTo(HaveOccurred())
			violations, err := store.ScanUniquenessViolationsForValue(index, tuple.Tuple{int64(0), int64(0), int64(8)})
			Expect(err).NotTo(HaveOccurred())
			Expect(violations).To(HaveLen(2))
			neighbor, err := store.LoadRecord(partitionMoveKey(md, true, 3, 0, 0))
			Expect(err).NotTo(HaveOccurred())
			Expect(neighbor.Record.(*demopb.Order).GetPrice()).To(Equal(int32(9)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prepares VALUE entries once and never annotates WRITE_ONLY conflicts during size preflight", func() {
		b := partitionMoveBuilder(true)
		b.AddIndex("Order", NewIndex("zz_move_size", KeyWithValue(Concat(PartitionExpr(Field("coord_x"), 10), Field("vector_data")), 1)))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			for _, record := range []proto.Message{partitionMoveRecord(1, -11, 7, "red"), partitionMoveRecord(2, 0, 8, "blue")} {
				_, err := store.SaveRecord(record)
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := store.MarkIndexWriteOnly("move_unique")
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			counts := make(map[string]int)
			store, err := partitionProtocolStore(rc, md, root).AsBuilder().SetIndexMaintenanceFilter(&partitionMoveFilter{callback: func(index *Index, row proto.Message) {
				counts[fmt.Sprintf("%s/%d", index.Name, row.(*demopb.Order).GetCoordX())]++
			}}).Open()
			Expect(err).NotTo(HaveOccurred())
			before, effects := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
			oversized := partitionMoveRecord(1, 0, 8, "conflict")
			oversized.VectorData = make([]byte, valueSizeLimit+1)
			_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{partitionMoveKey(md, true, 1, -11, -2), oversized})
			var size *IndexValueSizeError
			Expect(errors.As(err, &size)).To(BeTrue())
			Expect(size.IndexName).To(Equal("zz_move_size"))
			Expect(counts["move_unique/-11"]).To(Equal(1))
			Expect(counts["move_unique/0"]).To(Equal(1), "a real WRITE_ONLY conflicting range was evaluated before the later size refusal")
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			violations, err := store.ScanUniquenessViolationsForValue(md.GetIndex("move_unique"), tuple.Tuple{int64(0), int64(0), int64(8)})
			Expect(err).NotTo(HaveOccurred())
			Expect(violations).To(BeEmpty())
			clear(counts)
			_, err = partitionMoveApply(ctx, store, PartitionRecordUpdate{partitionMoveKey(md, true, 1, -11, -2), partitionMoveRecord(1, 10, 9, "green")})
			Expect(err).NotTo(HaveOccurred())
			for _, index := range []string{"move_value", "move_cover", "move_unique", "zz_move_size"} {
				Expect(counts[index+"/-11"]).To(Equal(1))
				Expect(counts[index+"/10"]).To(Equal(1), "prepared VALUE entries must be reused, not filtered/evaluated again")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps dry-run lock bypass and secondary-UNIQUE omission with exact version-state preservation", func() {
		md, err := partitionMoveBuilder(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			for _, record := range []proto.Message{partitionMoveRecord(1, -11, 7, "red"), partitionMoveRecord(2, 0, 8, "blue")} {
				_, err := store.SaveRecord(record)
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "preview lock control")
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
			group, err := store.PreparePartitionRecordUpdates([]PartitionRecordUpdate{{partitionMoveKey(md, true, 1, -11, -2), partitionMoveRecord(1, 0, 8, "conflict")}})
			Expect(err).NotTo(HaveOccurred())
			preview, err := group.Apply(ctx, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(preview).To(HaveLen(1))
			Expect(preview[0].PrimaryKey).To(Equal(partitionMoveKey(md, true, 1, 0, 0)))
			Expect(preview[0].Version).NotTo(BeNil())
			Expect(rc.localVersion.Load()).To(Equal(counter))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, failure := range []string{"second serializer", "second UNIQUE", "reset between rows", "delete-store between rows"} {
		It("makes a later group failure rollback-only: "+failure, func() {
			md, err := partitionMoveBuilder(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				for _, record := range []proto.Message{partitionMoveRecord(1, -11, 7, "red"), partitionMoveRecord(2, 0, 8, "blue"), partitionMoveRecord(3, 10, 99, "neighbor")} {
					_, err := store.SaveRecord(record)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			rc, err := sharedDB.OpenRecordContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer rc.Cancel()
			other, err := partitionProtocolStore(rc, md, root).AsBuilder().SetSerializer(lifecycleCodec(NewFixedZeroKeyManager(make([]byte, 16), "", nil))).Open()
			Expect(err).NotTo(HaveOccurred())
			before, effects := partitionWholeStore(rc, other), partitionVersionEffectsOf(rc)
			manager := &lifecycleKeyManager{SerializationKeyManager: NewFixedZeroKeyManager(make([]byte, 16), "", nil)}
			firstApplied := false
			manager.onWrite = func() error {
				if manager.writes != 2 {
					return nil
				}
				row, err := other.LoadRecord(partitionMoveKey(md, true, 1, 20, 2))
				Expect(err).NotTo(HaveOccurred())
				firstApplied = row != nil
				if failure == "second serializer" {
					return errors.New("second serializer refused")
				}
				if failure == "reset between rows" {
					return other.DeleteAllRecords()
				}
				if failure == "delete-store between rows" {
					return DeleteStore(rc, root)
				}
				return nil
			}
			store, err := other.AsBuilder().SetSerializer(lifecycleCodec(manager)).Open()
			Expect(err).NotTo(HaveOccurred())
			secondPrice := int32(8)
			if failure == "second UNIQUE" {
				secondPrice = 99
			}
			_, err = partitionMoveApply(ctx, store,
				PartitionRecordUpdate{partitionMoveKey(md, true, 1, -11, -2), partitionMoveRecord(1, 20, 17, "green")},
				PartitionRecordUpdate{partitionMoveKey(md, true, 2, 0, 0), partitionMoveRecord(2, 10, secondPrice, "green")})
			Expect(err).To(HaveOccurred())
			Expect(firstApplied).To(BeTrue(), "failure occurs after a completed first relocation")
			Expect(manager.writes).To(Equal(2))
			var incomplete *PartitionMutationError
			Expect(errors.As(rc.Commit(), &incomplete)).To(BeTrue())
			_, err = sharedDB.Run(ctx, func(fresh *FDBRecordContext) (any, error) {
				Expect(partitionWholeStore(fresh, partitionProtocolStore(fresh, md, root))).To(Equal(before))
				Expect(partitionVersionEffectsOf(fresh)).To(Equal(effects))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})
