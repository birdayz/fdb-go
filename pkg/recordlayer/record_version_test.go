package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

var _ = Describe("FDBRecordVersion", func() {
	It("IncompleteVersion", func() {
		v, err := IncompleteVersion(42)
		Expect(err).NotTo(HaveOccurred())
		Expect(v.IsComplete()).To(BeFalse())
		Expect(v.GetLocalVersion()).To(Equal(42))
		b := v.ToBytes()
		Expect(len(b)).To(Equal(VersionBytes))
		for i := 0; i < GlobalVersionBytes; i++ {
			Expect(b[i]).To(Equal(byte(0xFF)))
		}
	})

	It("CompleteVersion", func() {
		global := make([]byte, GlobalVersionBytes)
		global[0] = 0x01
		global[7] = 0x42
		v, err := NewCompleteVersion(global, 7)
		Expect(err).NotTo(HaveOccurred())
		Expect(v.IsComplete()).To(BeTrue())
		Expect(v.GetLocalVersion()).To(Equal(7))
		Expect(v.GetGlobalVersion()).To(Equal(global))
	})

	It("WithCommittedVersion", func() {
		incomplete, _ := IncompleteVersion(3)
		committed := make([]byte, GlobalVersionBytes)
		committed[7] = 0x99
		complete, err := incomplete.WithCommittedVersion(committed)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete.IsComplete()).To(BeTrue())
		Expect(complete.GetLocalVersion()).To(Equal(3))
		Expect(complete.GetGlobalVersion()).To(Equal(committed))
	})

	It("InvalidInputs", func() {
		_, err := IncompleteVersion(-1)
		Expect(err).To(HaveOccurred())
		_, err = IncompleteVersion(0x10000)
		Expect(err).To(HaveOccurred())
		_, err = NewCompleteVersion([]byte{1, 2, 3}, 0)
		Expect(err).To(HaveOccurred())
	})

	It("RoundTrip", func() {
		original, _ := NewCompleteVersion(
			[]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x01},
			256,
		)
		parsed, err := CompleteVersionFromBytes(original.ToBytes())
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed.GetLocalVersion()).To(Equal(original.GetLocalVersion()))
		Expect(parsed.GetGlobalVersion()).To(Equal(original.GetGlobalVersion()))
	})
})

var _ = Describe("RecordVersioning", func() {
	ctx := context.Background()

	newMeta := func() *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.SetStoreRecordVersions(true)
		return builder.Build()
	}

	It("VersionStoredOnSave", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save with RunWithVersionstamp to get committed versionstamp
		_, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			if err != nil {
				return nil, err
			}

			// Same tx: version should be incomplete (from cache)
			version, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			if err != nil {
				return nil, err
			}
			Expect(version).NotTo(BeNil())
			Expect(version.IsComplete()).To(BeFalse())
			Expect(version.GetLocalVersion()).To(Equal(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(vs).NotTo(BeNil())
		Expect(len(vs)).To(Equal(GlobalVersionBytes))

		// Read back in new transaction
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			version, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(version).NotTo(BeNil())
			Expect(version.IsComplete()).To(BeTrue())
			Expect(version.GetLocalVersion()).To(Equal(0))
			Expect(version.GetGlobalVersion()).To(Equal(vs))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MultipleRecordsSequentialLocalVersions", func() {
		metaData := newMeta()
		ks := specSubspace()

		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(0); i < 5; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
				if err != nil {
					return nil, err
				}
			}
			for i := int64(0); i < 5; i++ {
				v, err := store.LoadRecordVersion(tuple.Tuple{i}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).NotTo(BeNil())
				Expect(v.GetLocalVersion()).To(Equal(int(i)))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("VersionClearedOnDelete", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save
		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Delete and verify
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify version gone
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("VersionNotStoredWhenDisabled", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		metaData := builder.Build()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			if err != nil {
				return nil, err
			}
			v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("VersionsClearedByDeleteAllRecords", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save
		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(0); i < 3; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Delete all
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			if err := store.DeleteAllRecords(); err != nil {
				return nil, err
			}
			for i := int64(0); i < 3; i++ {
				v, err := store.LoadRecordVersion(tuple.Tuple{i}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).To(BeNil())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateRecordGetsNewVersion", func() {
		metaData := newMeta()
		ks := specSubspace()

		// Save v1
		_, vs1, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Save v2 (update)
		_, vs2, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(vs1).NotTo(Equal(vs2))

		// Read back — should have latest version
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			v, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).NotTo(BeNil())
			Expect(v.IsComplete()).To(BeTrue())
			Expect(v.GetGlobalVersion()).To(Equal(vs2))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
