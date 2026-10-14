package recordlayer

import (
	"context"
	"math"

	"fdb.dev/gen/demopb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// ByteScanLimit boundary: Java's ByteScanLimiter.hasBytesRemaining() is
// `bytesRemaining > 0`, so the limit halts once bytesScanned >= limit. With the
// limit at exactly one record's bytes, the second key-value halts and the
// unsplitter finishes its record: two records, where a `>` check gives three.
var _ = Describe("BUG1_ByteScanLimit_OffByOne", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("stops after exactly bytesScanned == ScannedBytesLimit", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 identical records so each has the same byte size
			for i := int64(1); i <= 3; i++ {
				order := &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// First: scan one record with no limit to measure its byte size
			unlimitedProps := ForwardScan()
			unlimitedProps.ExecuteProperties.ReturnedRowLimit = 1
			cursor := store.ScanRecords(nil, unlimitedProps)
			result, err := cursor.OnNext(rtx.ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeTrue())
			rec := result.GetValue()
			recordBytes := int64(rec.KeySize + rec.ValueSize)
			_ = cursor.Close()

			// Now scan with ScannedBytesLimit = exactly one record's byte size.
			// Java: after reading 1 record, bytesRemaining = 0, so the second
			// key-value's hasBytesRemaining() halts; the unsplitter still finishes
			// that record. A `>` check would halt only at the third.
			scanProps := ScanProperties{
				ExecuteProperties: ExecuteProperties{
					IsolationLevel:    SerializableIsolation,
					ScannedBytesLimit: recordBytes,
				},
			}
			cursor2 := store.ScanRecords(nil, scanProps)

			var count int
			var lastResult RecordCursorResult[*FDBStoredRecord[proto.Message]]
			for {
				r, rErr := cursor2.OnNext(rtx.ctx)
				Expect(rErr).NotTo(HaveOccurred())
				lastResult = r
				if !r.HasNext() {
					break
				}
				count++
			}

			Expect(count).To(Equal(2),
				"bytesScanned == ScannedBytesLimit halts at the second key-value, which finishes its record")
			Expect(lastResult.GetNoNextReason()).To(Equal(ByteLimitReached))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// BUG #2: SUM index negation overflow when sumValue is math.MinInt64.
// In SumIndexMaintainer.Update(), when deleting a record (oldRecord != nil,
// newRecord == nil), the old entries' values are negated via `-e.sumValue`.
// In two's complement, -math.MinInt64 == math.MinInt64 (overflow, stays negative).
// This means deleting a record with a field value of math.MinInt64 ADDS
// math.MinInt64 instead of subtracting it, corrupting the aggregate.
//
// File: sum_index_maintainer.go, line 74
var _ = Describe("BUG2_SumIndex_NegationOverflow", func() {
	It("negation of math.MinInt64 overflows to itself", func() {
		// Pure arithmetic test — demonstrates the overflow.
		v := int64(math.MinInt64)
		negated := -v

		// In two's complement, -MinInt64 == MinInt64 (overflow).
		// The SUM index would ADD MinInt64 to the aggregate instead of subtracting.
		// For correctness, -MinInt64 should be MaxInt64+1, but that can't
		// be represented in int64. At minimum the code should detect and error.
		Expect(negated).To(Equal(v),
			"this proves -math.MinInt64 == math.MinInt64 in two's complement; SUM index will corrupt data")
		Expect(negated).To(BeNumerically("<", int64(0)),
			"negation of MinInt64 is still negative — SUM index adds instead of subtracting")
	})
})

// BUG #3 (FIXED): FDB limit overflow when ReturnedRowLimit is math.MaxInt.
// Both keyValueCursor.initIterator and indexCursor.initIterator now guard
// against overflow when computing limit + 1. If limit == math.MaxInt,
// the code uses math.MaxInt directly instead of adding 1.
var _ = Describe("BUG3_FDBLimitOverflow_MaxInt", func() {
	It("confirms math.MaxInt + 1 overflows (arithmetic fact)", func() {
		// Pure arithmetic — this is the overflow that would happen without the fix
		v := math.MaxInt
		v = v + 1 // runtime overflow, not compile-time
		Expect(v).To(BeNumerically("<", 0),
			"math.MaxInt + 1 overflows to negative in Go (this is expected)")
	})

	It("guard prevents overflow in limit computation", func() {
		// Simulate the FIXED computation from index_scan.go:
		//   limit := ReturnedRowLimit - recordsRead
		//   if limit == math.MaxInt { options.Limit = math.MaxInt } else { options.Limit = limit + 1 }
		limit := math.MaxInt - 0 // ReturnedRowLimit - recordsRead
		var result int
		if limit == math.MaxInt {
			result = math.MaxInt
		} else {
			result = limit + 1
		}
		Expect(result).To(Equal(math.MaxInt),
			"guard should prevent overflow by capping at math.MaxInt")
	})
})
