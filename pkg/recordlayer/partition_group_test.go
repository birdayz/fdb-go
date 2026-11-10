package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

func partitionSaveSurface(store *FDBRecordStore, surface string, records []proto.Message, check RecordExistenceCheck) error {
	switch surface {
	case "batch", "legacy":
		_, err := store.SaveRecordBatch(records)
		return err
	case "pipeline":
		_, err := store.SaveRecordsPipelined(records, check)
		return err
	case "typed":
		typed, err := GetTypedRecordStore[*demopb.Order](store, "Order")
		if err != nil {
			return err
		}
		_, err = typed.SaveRecordWithOptions(records[0].(*demopb.Order), check)
		return err
	case "override":
		_, err := store.OverrideLockSaveRecord(records[0], check)
		return err
	default:
		return &RecordCoreArgumentError{Message: "unknown partition save test surface"}
	}
}

func partitionSurfaceStore(rc *FDBRecordContext, md *RecordMetaData, root subspace.Subspace, surface string) *FDBRecordStore {
	GinkgoHelper()
	format := int32(14)
	if surface == "legacy" {
		format = 4
	}
	store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(format).SetPipelineSizer(func(PipelineOperation) int { return 3 }).CreateOrOpen()
	Expect(err).NotTo(HaveOccurred())
	return store
}

var _ = Describe("Independent partition writer paths", func() {
	ctx := context.Background()
	for _, surface := range []string{"batch", "legacy", "pipeline", "typed", "override"} {
		It("refuses corrupted fences without effects through "+surface, func() {
			b := partitionTestBuilder().SetSplitLongRecords(false)
			b.AddIndex("Order", NewIndex("surface_value", Concat(PartitionExpr(Field("coord_x"), 10), Field("price"))))
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionSurfaceStore(rc, md, root, surface)
				_, err := store.SaveRecord(partitionOrder(1, -11, 7))
				Expect(err).NotTo(HaveOccurred())
				rc.Transaction().Clear(partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"fence", int64(-2)}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			var before []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionSurfaceStore(rc, md, root, surface)
				before = partitionWholeStore(rc, store)
				effects := partitionVersionEffectsOf(rc)
				err := partitionSaveSurface(store, surface, []proto.Message{partitionOrder(2, -12, 8)}, RecordExistenceCheckNone)
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				Expect(partitionWholeStore(rc, partitionSurfaceStore(rc, md, root, surface))).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("conflicts a fresh route activation with a fence-only write through "+surface, func() {
			b := partitionTestBuilder().SetSplitLongRecords(false)
			idx := NewIndex("race_value", Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
			b.AddIndex("Order", idx)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				_, err := partitionSurfaceStore(rc, md, root, surface).SaveRecord(partitionOrder(99, 10, 99))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			a, err := sharedDB.OpenRecordContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer a.Cancel()
			store := partitionSurfaceStore(a, md, root, surface)
			ss := partitionSubspace(root, md.GetRecordType("Order"))
			Expect(a.Transaction().Get(ss.Pack(tuple.Tuple{"active", int64(-2)})).MustGet()).To(BeNil())
			Expect(partitionSaveSurface(store, surface, []proto.Message{partitionOrder(1, -11, 7)}, RecordExistenceCheckNone)).To(Succeed())
			Expect(a.Transaction().Get(ss.Pack(tuple.Tuple{"active", int64(-2)})).MustGet()).To(Equal(tuple.Tuple{int64(1)}.Pack()))
			Expect(a.Transaction().Get(ss.Pack(tuple.Tuple{"fence", int64(-2)})).MustGet()).To(Equal(tuple.Tuple{int64(1), true}.Pack()))
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				rc.Transaction().Set(ss.Pack(tuple.Tuple{"fence", int64(-2)}), tuple.Tuple{int64(1), false}.Pack())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			var conflict fdb.Error
			Expect(errors.As(a.Commit(), &conflict)).To(BeTrue())
			Expect(conflict.Code).To(Equal(1020))
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionSurfaceStore(rc, md, root, surface)
				rows, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(1))
				Expect(rows[0].Record.(*demopb.Order).GetOrderId()).To(Equal(int64(99)))
				entries, err := AsList(ctx, store.ScanIndexRecords(idx.Name, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"active", int64(-2)})).MustGet()).To(BeNil())
				Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"epoch"})).MustGet()).To(Equal(tuple.Tuple{int64(1)}.Pack()))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("preserves existence or serializer precedence ahead of routing corruption through "+surface, func() {
			md, err := partitionTestBuilder().SetSplitLongRecords(false).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionSurfaceStore(rc, md, root, surface)
				_, err := store.SaveRecord(partitionOrder(1, -11, 7))
				Expect(err).NotTo(HaveOccurred())
				rc.Transaction().Clear(partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"active", int64(-2)}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionSurfaceStore(rc, md, root, surface)
				before := partitionWholeStore(rc, store)
				record := partitionOrder(1, -11, 8)
				check := RecordExistenceCheckErrorIfExists
				serializerFailure := &RecordCoreError{Message: "first row serializer failure"}
				if surface == "batch" || surface == "legacy" {
					check = RecordExistenceCheckNone
					manager := &lifecycleKeyManager{SerializationKeyManager: NewFixedZeroKeyManager(make([]byte, 16), "", nil), onWrite: func() error { return serializerFailure }}
					store, err = store.AsBuilder().SetSerializer(lifecycleCodec(manager)).Open()
					Expect(err).NotTo(HaveOccurred())
				}
				err := partitionSaveSurface(store, surface, []proto.Message{record}, check)
				if surface == "batch" || surface == "legacy" {
					Expect(errors.Is(err, serializerFailure)).To(BeTrue(), "%v", err)
				} else {
					var exists *RecordAlreadyExistsError
					Expect(errors.As(err, &exists)).To(BeTrue(), "%v", err)
				}
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, surface := range []string{"batch", "legacy", "pipeline"} {
		for _, failure := range []string{"routing", "serializer"} {
			It("rejects deliberate Commit after a later-row "+failure+" failure through "+surface, func() {
				md, err := partitionTestBuilder().SetSplitLongRecords(false).SetStoreRecordVersions(true).Build()
				Expect(err).NotTo(HaveOccurred())
				root := specSubspace()
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := partitionSurfaceStore(rc, md, root, surface)
					_, err := store.SaveRecord(partitionOrder(2, 10, 7))
					Expect(err).NotTo(HaveOccurred())
					rc.Transaction().Clear(partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"fence", int64(1)}))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				rc, err := sharedDB.OpenRecordContext(ctx)
				Expect(err).NotTo(HaveOccurred())
				defer rc.Cancel()
				store := partitionSurfaceStore(rc, md, root, surface)
				before := partitionWholeStore(rc, store)
				serializerFailure := &RecordCoreError{Message: "second row serializer failure"}
				var manager *lifecycleKeyManager
				if failure == "serializer" {
					manager = &lifecycleKeyManager{SerializationKeyManager: NewFixedZeroKeyManager(make([]byte, 16), "", nil)}
					manager.onWrite = func() error {
						if manager.writes == 2 {
							return serializerFailure
						}
						return nil
					}
					store, err = store.AsBuilder().SetSerializer(lifecycleCodec(manager)).Open()
					Expect(err).NotTo(HaveOccurred())
				}
				err = partitionSaveSurface(store, surface, []proto.Message{partitionOrder(1, -21, 8), partitionOrder(2, 10, 9)}, RecordExistenceCheckNone)
				if failure == "serializer" {
					Expect(errors.Is(err, serializerFailure)).To(BeTrue(), "%v", err)
				} else {
					var state *PartitionStateError
					Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
				}
				if manager != nil {
					manager.err = nil
				}
				pk := tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-3), int64(-21), int64(1)}
				row, readErr := store.LoadRecord(pk)
				Expect(readErr).NotTo(HaveOccurred())
				Expect(row).NotTo(BeNil(), "first row must really have completed before the later refusal")
				Expect(row.Record.(*demopb.Order).GetPrice()).To(Equal(int32(8)))
				Expect(row.Version.IsComplete()).To(BeFalse())
				Expect(partitionVersionEffectsOf(rc).mutations).NotTo(BeEmpty())
				ss := partitionSubspace(root, md.GetRecordType("Order"))
				Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"active", int64(-3)})).MustGet()).To(Equal(tuple.Tuple{int64(1)}.Pack()))
				var incomplete *PartitionMutationError
				commitErr := rc.Commit()
				Expect(errors.As(commitErr, &incomplete)).To(BeTrue())
				Expect(errors.Is(commitErr, err)).To(BeTrue(), "guard preserves the actual later-row failure")
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					Expect(partitionWholeStore(rc, partitionSurfaceStore(rc, md, root, surface))).To(Equal(before))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
		for _, reset := range []string{"delete-all", "delete-store"} {
			It("rejects a reset between completed group rows through "+surface+"/"+reset, func() {
				md, err := partitionTestBuilder().SetSplitLongRecords(false).Build()
				Expect(err).NotTo(HaveOccurred())
				root := specSubspace()
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) { partitionSurfaceStore(rc, md, root, surface); return nil, nil })
				Expect(err).NotTo(HaveOccurred())
				rc, err := sharedDB.OpenRecordContext(ctx)
				Expect(err).NotTo(HaveOccurred())
				defer rc.Cancel()
				other := partitionSurfaceStore(rc, md, root, surface)
				before := partitionWholeStore(rc, other)
				manager := &lifecycleKeyManager{SerializationKeyManager: NewFixedZeroKeyManager(make([]byte, 16), "", nil)}
				var callbackErr error
				var firstBodyPresent bool
				manager.onWrite = func() error {
					if manager.writes != 2 {
						return nil
					}
					pk := tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)}
					if !other.omitUnsplitRecordSuffix() {
						pk = append(pk, int64(0))
					}
					data, err := rc.Transaction().Get(other.recordsSubspace.Pack(pk)).Get()
					if err != nil {
						return err
					}
					firstBodyPresent = data != nil
					if reset == "delete-all" {
						callbackErr = other.DeleteAllRecords()
					} else {
						callbackErr = DeleteStore(rc, root)
					}
					return callbackErr
				}
				format := int32(14)
				if surface == "legacy" {
					format = 4
				}
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(format).SetSerializer(lifecycleCodec(manager)).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				err = partitionSaveSurface(store, surface, []proto.Message{partitionOrder(1, -11, 7), partitionOrder(2, 10, 8)}, RecordExistenceCheckNone)
				Expect(callbackErr).NotTo(HaveOccurred())
				Expect(firstBodyPresent).To(BeTrue(), "the first row completed before reset")
				Expect(manager.writes).To(Equal(2))
				Expect(err).To(HaveOccurred())
				var incomplete *PartitionMutationError
				Expect(errors.As(rc.Commit(), &incomplete)).To(BeTrue())
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					Expect(partitionWholeStore(rc, partitionSurfaceStore(rc, md, root, surface))).To(Equal(before))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}
})
