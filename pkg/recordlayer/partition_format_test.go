package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

func partitionOpenFormat(rc *FDBRecordContext, md *RecordMetaData, root subspace.Subspace, version int32) *FDBRecordStore {
	GinkgoHelper()
	store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(version).CreateOrOpen()
	Expect(err).NotTo(HaveOccurred())
	return store
}

var _ = Describe("Partition live format conversion", func() {
	ctx := context.Background()
	seed := func(md *RecordMetaData, root subspace.Subspace) {
		GinkgoHelper()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, md, root, 5)
			_, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(partitionOrder(2, 10, 8))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}

	for _, partitioned := range []bool{false, true} {
		It("moves real committed legacy versions on builder reopen with partitioned="+map[bool]string{true: "true", false: "false"}[partitioned], func() {
			b := baseBuilder().SetStoreRecordVersions(true)
			if partitioned {
				b = partitionTestBuilder().SetStoreRecordVersions(true)
			}
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			seed(md, root)
			var protocol []fdb.KeyValue
			var originals []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionOpenFormat(rc, md, root, 5)
				protocol = partitionProtocolRows(rc, root)
				originals, err = rc.Transaction().GetRange(root.Sub(RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(originals).To(HaveLen(2))
				// The existing setter rewrites StoreInfo even at the same format;
				// only its last_update_time may change, not any data or version bytes.
				expected := partitionWholeStore(rc, store)
				effects := partitionVersionEffectsOf(rc)
				const setterTimestamp = uint64(2_000_000_000_000)
				expectedHeader := store.GetStoreHeader()
				expectedHeader.LastUpdateTime = proto.Uint64(setterTimestamp)
				expectedHeaderBytes, err := expectedHeader.MarshalVT()
				Expect(err).NotTo(HaveOccurred())
				headerRows := 0
				for i := range expected {
					if bytes.Equal(expected[i].Key, root.Pack(tuple.Tuple{StoreInfoKey})) {
						expected[i].Value = expectedHeaderBytes
						headerRows++
					}
				}
				Expect(headerRows).To(Equal(1))
				previousEnv := rc.env
				rc.env = &dst.Env{Clock: dst.NewSimClock(time.UnixMilli(int64(setterTimestamp)))}
				defer func() { rc.env = previousEnv }()
				Expect(store.SetFormatVersion(5)).To(Succeed())
				Expect(store.GetFormatVersion()).To(Equal(int32(5)))
				before := partitionWholeStore(rc, store)
				Expect(before).To(Equal(expected))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				if partitioned {
					var state *PartitionStateError
					Expect(errors.As(store.SetFormatVersion(14), &state)).To(BeTrue())
					Expect(partitionWholeStore(rc, store)).To(Equal(before))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionOpenFormat(rc, md, root, 14)
				Expect(store.useOldVersionFormat()).To(BeFalse())
				Expect(partitionProtocolRows(rc, root)).To(Equal(protocol))
				legacy, err := rc.Transaction().GetRange(root.Sub(RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(legacy).To(BeEmpty())
				for _, old := range originals {
					pk, err := root.Sub(RecordVersionKey).Unpack(old.Key)
					Expect(err).NotTo(HaveOccurred())
					row, err := store.LoadRecord(pk)
					Expect(err).NotTo(HaveOccurred())
					Expect(row.Version.IsComplete()).To(BeTrue())
					Expect(row.Version.ToBytes()).To(Equal(old.Value))
					packed := rc.Transaction().Get(store.versionKey(pk)).MustGet()
					parts, err := tuple.Unpack(packed)
					Expect(err).NotTo(HaveOccurred())
					Expect(parts).To(HaveLen(1))
					Expect(parts[0].(tuple.Versionstamp).Bytes()).To(Equal(old.Value))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("converts mixed plain siblings without imposing partition routing state", func() {
		b := partitionTestBuilder().SetStoreRecordVersions(true)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		seed(md, root)
		var oldVersion []byte
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, md, root, 5)
			_, err := store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1001), Name: proto.String("plain")})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			oldVersion = rc.Transaction().Get(root.Sub(RecordVersionKey).Pack(tuple.Tuple{md.GetRecordType("Customer").GetRecordTypeKey(), int64(1001)})).MustGet()
			Expect(oldVersion).To(HaveLen(VersionBytes))
			store := partitionOpenFormat(rc, md, root, 14)
			row, err := store.LoadRecord(tuple.Tuple{md.GetRecordType("Customer").GetRecordTypeKey(), int64(1001)})
			Expect(err).NotTo(HaveOccurred())
			Expect(row.Record.(*demopb.Customer).GetName()).To(Equal("plain"))
			Expect(row.Version.ToBytes()).To(Equal(oldVersion))
			Expect(partitionProtocolRows(rc, root)).To(HaveLen(6))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, md, root, 14)
			Expect(rc.Transaction().Get(root.Sub(RecordVersionKey).Pack(tuple.Tuple{md.GetRecordType("Customer").GetRecordTypeKey(), int64(1001)})).MustGet()).To(BeNil())
			row, err := store.LoadRecord(tuple.Tuple{md.GetRecordType("Customer").GetRecordTypeKey(), int64(1001)})
			Expect(err).NotTo(HaveOccurred())
			Expect(row.Version.ToBytes()).To(Equal(oldVersion))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps the direct format setter's plain-store behavior", func() {
		md, err := baseBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, md, root, 14)
			Expect(store.SetFormatVersion(13)).To(Succeed())
			Expect(store.GetFormatVersion()).To(Equal(int32(13)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			Expect(partitionOpenFormat(rc, md, root, 13).storeHeader.GetFormatVersion()).To(Equal(int32(13)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, corruption := range []string{"header", "active", "fence", "body"} {
		It("refuses corrupt live conversion before effects and permits deliberate commit: "+corruption, func() {
			md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			seed(md, root)
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionOpenFormat(rc, md, root, 5)
				ss := partitionSubspace(root, md.GetRecordType("Order"))
				if corruption == "header" {
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{"header"}))
				} else if corruption != "body" {
					rc.Transaction().Clear(ss.Pack(tuple.Tuple{corruption, int64(-2)}))
				} else {
					rt := md.GetRecordType("Order")
					data, err := serializeUnionOver(partitionOrder(1, -12, 7), rt, nil)
					Expect(err).NotTo(HaveOccurred())
					rc.Transaction().Set(store.recordsSubspace.Pack(tuple.Tuple{rt.GetRecordTypeKey(), int64(-2), int64(-11), int64(1), int64(0)}), data)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			var before []fdb.KeyValue
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionOpenFormat(rc, md, root, 5)
				before = partitionWholeStore(rc, store)
				effects := partitionVersionEffectsOf(rc)
				_, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(14).Open()
				var state *PartitionStateError
				Expect(errors.As(err, &state)).To(BeTrue(), "%v", err)
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				Expect(partitionWholeStore(rc, partitionOpenFormat(rc, md, root, 5))).To(Equal(before))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("rejects pending legacy conversion without consuming versions or changing format", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, md, root, 5)
			_, err := store.SaveRecord(partitionOrder(1, -11, 7))
			Expect(err).NotTo(HaveOccurred())
			before, effects := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
			Expect(effects.mutations).NotTo(BeEmpty())
			Expect(effects.local).NotTo(BeEmpty())
			_, err = NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(14).Open()
			var state *PartitionStateError
			Expect(errors.As(err, &state)).To(BeTrue())
			Expect(state.Reason).To(Equal("format conversion requires committed legacy versions"))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, md, root, 5)
			Expect(store.storeHeader.GetFormatVersion()).To(Equal(int32(5)))
			legacy, err := rc.Transaction().GetRange(root.Sub(RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(legacy).To(HaveLen(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prevents committing a conversion that fails after an inline write", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		seed(md, root)
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer rc.Cancel()
		before := partitionWholeStore(rc, partitionOpenFormat(rc, md, root, 5))
		rc.txSizeErrorBytes = 1
		_, err = NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(14).Open()
		var size *TransactionSizeExceededError
		Expect(errors.As(err, &size)).To(BeTrue(), "%v", err)
		pk := tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1), int64(-1)}
		Expect(rc.Transaction().Get(root.Sub(RecordKey).Pack(pk)).MustGet()).NotTo(BeNil(), "conversion really staged its first inline version")
		rc.txSizeErrorBytes = 0
		commitErr := rc.Commit()
		var incomplete *PartitionMutationError
		Expect(errors.As(commitErr, &incomplete)).To(BeTrue())
		Expect(errors.Is(commitErr, size)).To(BeTrue())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			Expect(partitionWholeStore(rc, partitionOpenFormat(rc, md, root, 5))).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps reconciliation rollback-only when header publication is not reached after conversion", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		seed(md, root)
		b := partitionTestBuilder().SetStoreRecordVersions(true).SetVersion(md.Version() + 1)
		b.AddIndex("Order", NewIndex("conversion_late_rebuild", Concat(PartitionExpr(Field("coord_x"), 10), Field("price"))))
		next, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer rc.Cancel()
		before := partitionWholeStore(rc, partitionOpenFormat(rc, md, root, 5))
		_, err = NewStoreBuilder().SetContext(rc).SetMetaDataProvider(next).SetSubspace(root).SetFormatVersion(14).SetIndexRebuildPolicy(func(*Index, int64, bool) IndexState { return IndexStateWriteOnlyWithQueue }).Open()
		Expect(err).To(HaveOccurred())
		legacy, readErr := rc.Transaction().GetRange(root.Sub(RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
		Expect(readErr).NotTo(HaveOccurred())
		Expect(legacy).To(BeEmpty(), "conversion completed before the later reconciliation failure")
		var incomplete *PartitionMutationError
		Expect(errors.As(rc.Commit(), &incomplete)).To(BeTrue())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			Expect(partitionWholeStore(rc, partitionOpenFormat(rc, md, root, 5))).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("conflicts a staged conversion with a fence-only revision and preserves neighboring legacy versions", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		seed(md, root)
		a, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer a.Cancel()
		before := partitionWholeStore(a, partitionOpenFormat(a, md, root, 5))
		partitionOpenFormat(a, md, root, 14)
		fenceKey := partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"fence", int64(-2)})
		for i := range before {
			if bytes.Equal(before[i].Key, fenceKey) {
				before[i].Value = tuple.Tuple{int64(2), true}.Pack()
			}
		}
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			rc.Transaction().Set(fenceKey, tuple.Tuple{int64(2), true}.Pack())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		var conflict fdb.Error
		Expect(errors.As(a.Commit(), &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, md, root, 5)
			legacy, err := rc.Transaction().GetRange(root.Sub(RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(legacy).To(HaveLen(2))
			Expect(partitionWholeStore(rc, store)).To(Equal(before), "only the independent fence revision survives; both legacy values and bodies are exact")
			row, err := store.LoadRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(1), int64(10), int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(row.Version.IsComplete()).To(BeTrue())
			Expect(rc.Transaction().Get(root.Sub(RecordKey).Pack(append(append(tuple.Tuple{}, row.PrimaryKey...), int64(-1)))).MustGet()).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
