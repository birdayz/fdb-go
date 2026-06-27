package recordlayer

import (
	"context"
	"errors"
	"sync"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("UniqueIndexConcurrent", func() {
	ctx := context.Background()

	buildMD := func() (*RecordMetaData, *Index) {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewIndex("unique_price", Field("price"))
		idx.SetUnique()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md, idx
	}

	It("rejects concurrent inserts with same unique key", func() {
		ks := specSubspace()
		md, _ := buildMD()

		// Two concurrent transactions inserting records with the same price (unique index).
		// Exactly one must fail — either with a uniqueness violation or FDB conflict.
		var wg sync.WaitGroup
		errs := make([]error, 2)

		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					if err != nil {
						return nil, err
					}
					// Different order IDs, same price → uniqueness conflict
					order := &gen.Order{OrderId: proto.Int64(int64(100 + idx)), Price: proto.Int32(42)}
					_, err = store.SaveRecord(order)
					return nil, err
				})
				errs[idx] = err
			}(i)
		}
		wg.Wait()

		// Exactly one must succeed, one must fail
		succeeded := 0
		for _, err := range errs {
			if err == nil {
				succeeded++
			}
		}
		Expect(succeeded).To(Equal(1), "expected exactly one transaction to succeed, got %d (err0=%v, err1=%v)", succeeded, errs[0], errs[1])
	})

	It("allows concurrent inserts with different unique keys", func() {
		ks := specSubspace()
		md, _ := buildMD()

		var wg sync.WaitGroup
		errs := make([]error, 2)

		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					if err != nil {
						return nil, err
					}
					// Different prices → no conflict
					order := &gen.Order{OrderId: proto.Int64(int64(200 + idx)), Price: proto.Int32(int32(10 + idx))}
					_, err = store.SaveRecord(order)
					return nil, err
				})
				errs[idx] = err
			}(i)
		}
		wg.Wait()

		// Both should succeed
		Expect(errs[0]).NotTo(HaveOccurred())
		Expect(errs[1]).NotTo(HaveOccurred())
	})

	It("full range scan covers entire prefix for conflict detection", func() {
		ks := specSubspace()
		md, idx := buildMD()

		// Insert a record with price=42, PK=1
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)}
			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to insert another record with price=42, PK=2 — should fail
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			order := &gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(42)}
			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).To(HaveOccurred())

		// Should be a uniqueness violation, not an FDB conflict
		var uniquenessErr *RecordIndexUniquenessViolationError
		Expect(errors.As(err, &uniquenessErr)).To(BeTrue())

		// Verify only one entry in the index
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
