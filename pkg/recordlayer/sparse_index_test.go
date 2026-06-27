package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

var _ = Describe("Sparse/filtered indexes", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("only indexes records matching predicate", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		priceIndex.SetPredicate(func(msg proto.Message) bool {
			order, ok := msg.(*gen.Order)
			if !ok {
				return false
			}
			return order.GetPrice() >= 500
		})

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ss := specSubspace()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save records with different prices
			for _, p := range []int32{100, 300, 500, 700, 900} {
				id := int64(p)
				price := p
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
			}

			// Scan index — only orders >= 500 should be there
			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(500)}))
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(700)}))
			Expect(entries[2].PrimaryKey()).To(Equal(tuple.Tuple{int64(900)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("predicate nil means all records indexed", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		// No predicate set

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ss := specSubspace()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			for _, p := range []int32{100, 200, 300} {
				id := int64(p)
				price := p
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
			}

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("filtered records are removed from index on update when predicate changes result", func() {
		priceIndex := NewIndex("Order$price", Field("price"))
		priceIndex.SetPredicate(func(msg proto.Message) bool {
			order, ok := msg.(*gen.Order)
			if !ok {
				return false
			}
			return order.GetPrice() >= 500
		})

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ss := specSubspace()

		// Save with price >= 500 (indexed)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			id := int64(1)
			price := int32(600)
			_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Update price to < 500 (should remove from index)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			id := int64(1)
			price := int32(200)
			_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
			if err != nil {
				return nil, err
			}

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Bulk index operations", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("DeleteIndexEntries clears all index entries", func() {
		priceIndex := NewIndex("Order$price", Field("price"))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ss := specSubspace()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			for i := int64(1); i <= 5; i++ {
				price := int32(i * 100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &i, Price: &price})
				if err != nil {
					return nil, err
				}
			}

			// Verify entries exist
			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			// Delete all entries
			store.DeleteIndexEntries(priceIndex)

			// Verify entries gone
			entries, err = AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteIndexEntriesInRange clears matching prefix", func() {
		priceIndex := NewIndex("Order$price", Field("price"))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ss := specSubspace()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save orders: 100, 200, 300
			for i := int64(1); i <= 3; i++ {
				price := int32(i * 100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &i, Price: &price})
				if err != nil {
					return nil, err
				}
			}

			// Delete entries with price = 200
			Expect(store.DeleteIndexEntriesInRange(priceIndex, tuple.Tuple{int64(200)})).To(Succeed())

			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Counter-based subspace keys", func() {
	It("assigns incrementing int64 keys", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.EnableCounterBasedSubspaceKeys()

		idx1 := NewIndex("idx_a", Field("price"))
		idx2 := NewIndex("idx_b", Field("order_id"))
		builder.AddIndex("Order", idx1)
		builder.AddIndex("Order", idx2)

		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		Expect(md.GetIndex("idx_a").SubspaceTupleKey()).To(Equal(int64(1)))
		Expect(md.GetIndex("idx_b").SubspaceTupleKey()).To(Equal(int64(2)))
	})

	It("uses name-based keys by default", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

		idx := NewIndex("idx_a", Field("price"))
		builder.AddIndex("Order", idx)

		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		Expect(md.GetIndex("idx_a").SubspaceTupleKey()).To(Equal("idx_a"))
	})
})

var _ = Describe("Store accessor methods", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("GetMetaData returns metadata", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			Expect(store.GetMetaData()).To(Equal(md))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("GetIndexMaintainer returns maintainer", func() {
		priceIndex := NewIndex("Order$price", Field("price"))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			maintainer, mErr := store.GetIndexMaintainer(priceIndex)
			Expect(mErr).NotTo(HaveOccurred())
			Expect(maintainer).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
