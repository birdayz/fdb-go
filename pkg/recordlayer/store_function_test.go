package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("EvaluateStoreFunction", func() {
	ctx := context.Background()

	newVersionedMeta := func() *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetStoreRecordVersions(true)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	newUnversionedMeta := func() *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	It("VERSION with complete version", func() {
		metaData := newVersionedMeta()
		ks := specSubspace()

		// Save a versioned record; commit to get a real versionstamp.
		_, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(vs).NotTo(BeNil())

		// In a new transaction, load the record (version is complete) and evaluate.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.Version).NotTo(BeNil())
			Expect(loaded.Version.IsComplete()).To(BeTrue())

			result, err := store.EvaluateStoreFunction(
				&StoreRecordFunction{Name: FunctionNameVersion}, loaded)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())

			version, ok := result.(*FDBRecordVersion)
			Expect(ok).To(BeTrue())
			Expect(version.IsComplete()).To(BeTrue())
			gv, err := version.GetGlobalVersion()
			Expect(err).NotTo(HaveOccurred())
			Expect(gv).To(Equal(vs))
			Expect(version.GetLocalVersion()).To(Equal(0))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("VERSION with incomplete version in same transaction", func() {
		metaData := newVersionedMeta()
		ks := specSubspace()

		_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			if err != nil {
				return nil, err
			}

			// Same transaction: load back, version is incomplete (from local cache).
			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())

			result, err := store.EvaluateStoreFunction(
				&StoreRecordFunction{Name: FunctionNameVersion}, loaded)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())

			version, ok := result.(*FDBRecordVersion)
			Expect(ok).To(BeTrue())
			// Within the same tx, the version is incomplete (no committed versionstamp yet).
			Expect(version.IsComplete()).To(BeFalse())
			Expect(version.GetLocalVersion()).To(Equal(0))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("VERSION with no version stored", func() {
		metaData := newUnversionedMeta()
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

			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())

			result, err := store.EvaluateStoreFunction(
				&StoreRecordFunction{Name: FunctionNameVersion}, loaded)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("unknown function name returns error", func() {
		metaData := newUnversionedMeta()
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

			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.EvaluateStoreFunction(
				&StoreRecordFunction{Name: "bogus"}, loaded)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unknown store function"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("nil function returns error", func() {
		metaData := newUnversionedMeta()
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

			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.EvaluateStoreFunction(nil, loaded)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("store function is nil"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("nil record returns error", func() {
		metaData := newUnversionedMeta()
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			_, err = store.EvaluateStoreFunction(
				&StoreRecordFunction{Name: FunctionNameVersion}, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("record is nil"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
