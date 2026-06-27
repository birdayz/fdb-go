package recordlayer

import (
	"context"
	"iter"
	"slices"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// mustSeq drains a cursor into a value-only iter.Seq[T], failing the surrounding
// Ginkgo test if the cursor surfaces an error (the error-aware replacement for the
// deleted Seq helper, used to feed the value-only Filter/Limit combinators).
func mustSeq[T any](cursor RecordCursor[T], ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		for v, err := range Seq2(cursor, ctx) {
			Expect(err).NotTo(HaveOccurred())
			if !yield(v) {
				return
			}
		}
	}
}

var _ = Describe("CursorSeqInterface", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		metaData, buildErr = builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	// saveTestOrders saves the standard test orders into the store and returns the store.
	saveTestOrders := func(rtx *FDBRecordContext) *FDBRecordStore {
		store, err := NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(metaData).
			SetSubspace(specSubspace()).
			CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())

		testOrders := []*gen.Order{
			{
				OrderId: proto.Int64(1001),
				Price:   proto.Int32(10),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			},
			{
				OrderId: proto.Int64(1002),
				Price:   proto.Int32(25),
				Flower:  &gen.Flower{Type: proto.String("Tulip"), Color: gen.Color_YELLOW.Enum()},
			},
			{
				OrderId: proto.Int64(1003),
				Price:   proto.Int32(50),
				Flower:  &gen.Flower{Type: proto.String("Lily"), Color: gen.Color_BLUE.Enum()},
			},
		}

		for _, order := range testOrders {
			_, err := store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())
		}

		return store
	}

	It("BasicSeq", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := saveTestOrders(rtx)
			scanCtx := context.Background()

			cursor := store.ScanRecords(nil, ForwardScan())

			var orderIDs []int64
			for record, iterErr := range Seq2(cursor, scanCtx) {
				Expect(iterErr).NotTo(HaveOccurred())
				order := record.Record.(*gen.Order)
				orderIDs = append(orderIDs, *order.OrderId)
			}

			Expect(orderIDs).To(HaveLen(3))
			Expect(orderIDs).To(Equal([]int64{1001, 1002, 1003}))
			GinkgoWriter.Printf("Basic Seq iteration found orders: %v\n", orderIDs)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Seq2WithErrors", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := saveTestOrders(rtx)
			scanCtx := context.Background()

			cursor := store.ScanRecords(nil, ForwardScan())

			var orderIDs []int64
			for record, err := range Seq2(cursor, scanCtx) {
				Expect(err).NotTo(HaveOccurred())
				order := record.Record.(*gen.Order)
				orderIDs = append(orderIDs, *order.OrderId)
			}

			Expect(orderIDs).To(HaveLen(3))
			GinkgoWriter.Printf("Seq2 iteration found orders: %v\n", orderIDs)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("StdlibIntegration", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := saveTestOrders(rtx)
			scanCtx := context.Background()

			// Test slices.Collect (Go 1.23+)
			cursor := store.ScanRecords(nil, ForwardScan())
			allRecords, allErr := AsList(scanCtx, cursor)
			Expect(allErr).NotTo(HaveOccurred())
			Expect(allRecords).To(HaveLen(3))

			// Test manual counting
			cursor2 := store.ScanRecords(nil, ForwardScan())
			count := 0
			for _, iterErr := range Seq2(cursor2, scanCtx) {
				Expect(iterErr).NotTo(HaveOccurred())
				count++
			}
			Expect(count).To(Equal(3))

			// Test getting first record
			cursor3 := store.ScanRecords(nil, ForwardScan())
			var firstRecord *FDBStoredRecord[proto.Message]
			var found bool
			for record, iterErr := range Seq2(cursor3, scanCtx) {
				Expect(iterErr).NotTo(HaveOccurred())
				firstRecord = record
				found = true
				break
			}
			Expect(found).To(BeTrue(), "no records found")
			firstOrder := firstRecord.Record.(*gen.Order)
			Expect(*firstOrder.OrderId).To(Equal(int64(1001)))

			GinkgoWriter.Printf("Standard library integration works: count=%d, first=%d\n", count, *firstOrder.OrderId)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ChainingOperations", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := saveTestOrders(rtx)
			scanCtx := context.Background()

			cursor := store.ScanRecords(nil, ForwardScan())

			expensiveOrders := Filter(
				mustSeq(cursor, scanCtx),
				func(record *FDBStoredRecord[proto.Message]) bool {
					order := record.Record.(*gen.Order)
					return *order.Price > 20
				},
			)

			expensiveOrderIDs := slices.Collect(
				Map(expensiveOrders, func(record *FDBStoredRecord[proto.Message]) int64 {
					order := record.Record.(*gen.Order)
					return *order.OrderId
				}),
			)

			Expect(expensiveOrderIDs).To(Equal([]int64{1002, 1003}))
			GinkgoWriter.Printf("Chained filter+map found expensive orders: %v\n", expensiveOrderIDs)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LimitFunction", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := saveTestOrders(rtx)
			scanCtx := context.Background()

			cursor := store.ScanRecords(nil, ForwardScan())

			limitedOrders := slices.Collect(
				Limit(mustSeq(cursor, scanCtx), 2),
			)

			Expect(limitedOrders).To(HaveLen(2))

			firstOrder := limitedOrders[0].Record.(*gen.Order)
			secondOrder := limitedOrders[1].Record.(*gen.Order)

			Expect(*firstOrder.OrderId).To(Equal(int64(1001)))
			Expect(*secondOrder.OrderId).To(Equal(int64(1002)))

			GinkgoWriter.Printf("LimitSeq correctly limited to first 2 orders: %d, %d\n",
				*firstOrder.OrderId, *secondOrder.OrderId)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
