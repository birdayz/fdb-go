package recordlayer

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("RFC 019 conformance audit", func() {
	ctx := context.Background()

	// Bug 1: Proto float field (FloatKind) is encoded as float64 (FDB type code 0x21)
	// instead of float32 (FDB type code 0x20). Java's tuple layer encodes float as
	// 4-byte IEEE 754 (type code 0x20). scalarToInterface returns value.Float()
	// (always float64) for both FloatKind and DoubleKind, losing the type distinction.
	// This breaks cross-language index compatibility: Java writes 0x20, Go writes 0x21
	// for the same proto float field.
	It("float32 proto field encodes as FDB float (type code 0x20), not double (0x21)", func() {
		idx := NewIndex("TypedRecord$val_float", Field("val_float"))

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("TypedRecord", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{
				Id:       proto.Int64(1),
				ValFloat: proto.Float32(3.14),
			})
			Expect(err).NotTo(HaveOccurred())

			// Read the raw index entry and check the FDB tuple type code.
			// float32 must encode as 0x20 (floatCode), not 0x21 (doubleCode).
			idxSubspace := store.subspace.Sub(IndexKey, idx.SubspaceTupleKey())
			begin, end := idxSubspace.FDBRangeKeys()
			kvs, err := store.context.Transaction().GetRange(
				fdb.KeyRange{Begin: begin, End: end},
				fdb.RangeOptions{},
			).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(kvs).To(HaveLen(1))

			// Unpack the index key and verify the value is float32, not float64.
			t, err := idxSubspace.Unpack(kvs[0].Key)
			Expect(err).NotTo(HaveOccurred())
			// The first element is the indexed value. For a proto float field,
			// it MUST be float32 to match Java's encoding.
			Expect(t[0]).To(BeAssignableToTypeOf(float32(0)),
				"proto FloatKind field must encode as float32 in FDB tuple, not float64")

			// Also verify at the raw byte level: first byte after subspace prefix
			// must be 0x20 (floatCode), not 0x21 (doubleCode).
			rawKey := kvs[0].Key[len(idxSubspace.FDBKey()):]
			Expect(rawKey[0]).To(Equal(byte(0x20)),
				"FDB tuple type code for float32 must be 0x20, got 0x21 (double)")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// Bug 2: Reverse scan + continuation in ScanRecordsByType prefix scan.
	// ScanRecordsByType always applies the continuation token to the LOW endpoint
	// (line 1171-1178 in store.go), but reverse scans iterate from high to low,
	// so the continuation must narrow the HIGH endpoint. Without this fix, the
	// second page of a reverse scan returns duplicates of the first page.
	It("reverse ScanRecordsByType with continuation returns distinct pages", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ks := specSubspace()
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save 10 orders and 5 customers to ensure prefix scan is needed
			for i := int64(1); i <= 10; i++ {
				if _, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
				}); err != nil {
					return nil, err
				}
			}
			for i := int64(1); i <= 5; i++ {
				if _, err := store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(i),
					Name:       proto.String(fmt.Sprintf("Cust%d", i)),
				}); err != nil {
					return nil, err
				}
			}

			// Reverse scan with limit 3: should return orders 10, 9, 8
			sp1 := ReverseScan()
			sp1.ExecuteProperties = sp1.ExecuteProperties.WithReturnedRowLimit(3)

			page1, cont1, err := AsListWithContinuation(
				context.Background(),
				store.ScanRecordsByType("Order", nil, sp1),
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(3), "first page should have 3 records")
			Expect(cont1).NotTo(BeEmpty(), "continuation should be non-empty when more records exist")

			// Verify first page is descending (10, 9, 8)
			for _, r := range page1 {
				Expect(r.RecordType.Name).To(Equal("Order"))
			}
			page1IDs := make([]int64, len(page1))
			for i, r := range page1 {
				page1IDs[i] = r.PrimaryKey[1].(int64)
			}
			Expect(page1IDs).To(Equal([]int64{10, 9, 8}),
				"first reverse page should be orders 10, 9, 8")

			// Resume with continuation: should return orders 7, 6, 5
			sp2 := ReverseScan()
			sp2.ExecuteProperties = sp2.ExecuteProperties.WithReturnedRowLimit(3)

			page2, cont2, err := AsListWithContinuation(
				context.Background(),
				store.ScanRecordsByType("Order", cont1, sp2),
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(page2).To(HaveLen(3), "second page should have 3 records")
			Expect(cont2).NotTo(BeEmpty(), "continuation should be non-empty when more records exist")

			page2IDs := make([]int64, len(page2))
			for i, r := range page2 {
				page2IDs[i] = r.PrimaryKey[1].(int64)
			}
			Expect(page2IDs).To(Equal([]int64{7, 6, 5}),
				"second reverse page should be orders 7, 6, 5 (not duplicates of first page)")

			// Verify no overlap between pages
			page1IDSet := make(map[int64]bool)
			for _, id := range page1IDs {
				page1IDSet[id] = true
			}
			for _, id := range page2IDs {
				Expect(page1IDSet[id]).To(BeFalse(),
					fmt.Sprintf("order %d appears in both page 1 and page 2", id))
			}

			// Drain remaining pages and verify all 10 orders are visited
			var allIDs []int64
			allIDs = append(allIDs, page1IDs...)
			allIDs = append(allIDs, page2IDs...)
			cont := cont2
			for len(cont) > 0 {
				spN := ReverseScan()
				spN.ExecuteProperties = spN.ExecuteProperties.WithReturnedRowLimit(3)
				pageN, contN, err := AsListWithContinuation(
					context.Background(),
					store.ScanRecordsByType("Order", cont, spN),
				)
				Expect(err).NotTo(HaveOccurred())
				for _, r := range pageN {
					allIDs = append(allIDs, r.PrimaryKey[1].(int64))
				}
				cont = contN
			}
			Expect(allIDs).To(HaveLen(10), "all 10 orders must be visited across all pages")
			// Verify strict descending order
			for i := 0; i < len(allIDs)-1; i++ {
				Expect(allIDs[i]).To(BeNumerically(">", allIDs[i+1]),
					fmt.Sprintf("expected strictly descending order, but allIDs[%d]=%d <= allIDs[%d]=%d",
						i, allIDs[i], i+1, allIDs[i+1]))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// Bug 3: ScanRecordsByType uses int64(recordType.RecordTypeIndex) instead of
	// recordType.GetRecordTypeKey(). When an explicit record type key is set via
	// SetRecordTypeKey(), the RecordTypeKeyExpression correctly uses the explicit
	// key (e.g., 42) when saving records, but ScanRecordsByType constructs the
	// scan range using RecordTypeIndex (the proto field number, e.g., 1).
	// This means records are saved under PK prefix 42 but scanned under prefix 1.
	It("ScanRecordsByType respects explicit record type key from SetRecordTypeKey", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		// Set an explicit record type key of 42 (differs from auto-derived index which is 1)
		builder.GetRecordType("Order").
			SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id"))).
			SetRecordTypeKey(int64(42))
		builder.GetRecordType("Customer").
			SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id"))).
			SetRecordTypeKey(int64(99))
		builder.GetRecordType("TypedRecord").
			SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Verify that GetRecordTypeKey returns the explicit key, not the index
		orderType := md.GetRecordType("Order")
		Expect(orderType.GetRecordTypeKey()).To(Equal(int64(42)),
			"explicit key should be 42, not RecordTypeIndex")
		Expect(orderType.RecordTypeIndex).To(Equal(1),
			"RecordTypeIndex should be 1 (proto field number)")

		ks := specSubspace()
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save 5 orders. RecordTypeKeyExpression evaluates to 42 (explicit key),
			// so PKs will be (42, 1), (42, 2), ..., (42, 5).
			for i := int64(1); i <= 5; i++ {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				})
				if err != nil {
					return nil, err
				}
			}

			// Save 3 customers under explicit key 99
			for i := int64(1); i <= 3; i++ {
				_, err := store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(i),
					Name:       proto.String(fmt.Sprintf("Cust%d", i)),
				})
				if err != nil {
					return nil, err
				}
			}

			// Verify records were saved with correct PK prefix
			loaded, err := store.LoadRecord(tuple.Tuple{int64(42), int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil(), "record at PK (42, 1) should exist")

			// ScanRecordsByType must use 42 (from GetRecordTypeKey), not 1 (RecordTypeIndex)
			orders, err := AsList(context.Background(),
				store.ScanRecordsByType("Order", nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(orders).To(HaveLen(5),
				"ScanRecordsByType should find all 5 orders using explicit key 42, "+
					"not miss them by scanning with RecordTypeIndex=1")

			for _, r := range orders {
				Expect(r.RecordType.Name).To(Equal("Order"))
				Expect(r.PrimaryKey[0]).To(Equal(int64(42)),
					"PK prefix should be explicit key 42")
			}

			// Also verify customers scan with their explicit key 99
			custs, err := AsList(context.Background(),
				store.ScanRecordsByType("Customer", nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(custs).To(HaveLen(3),
				"ScanRecordsByType should find all 3 customers using explicit key 99")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
