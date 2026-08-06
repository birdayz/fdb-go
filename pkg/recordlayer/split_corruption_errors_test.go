package recordlayer

import (
	"context"
	"errors"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Split-record corruption must arrive as a TYPED error naming which corruption
// it is.
//
// Java raises two distinct exceptions from one decision
// (SplitHelper.java:824-834): FoundSplitOutOfOrderException when a start segment
// was already seen and the sequence then breaks, FoundSplitWithoutStartException
// when the lowest segment present is not the start. They describe different
// damage — mis-sequenced versus truncated-at-the-front — and a reader diagnosing
// a corrupt store needs to know which.
//
// Go raised one bare fmt.Errorf for both, at two sites. Nothing could match on
// it, and the without-start case was reported as "out of order", which is
// actively misleading: it says the pieces are all present.
//
// The shapes below are built on the WIRE, by deleting real chunk keys from a
// genuinely split record, because that is the only way the conditions occur.
var _ = Describe("split record corruption errors", func() {
	ctx := context.Background()

	// A value comfortably over splitRecordSize so the record occupies several
	// chunks and a middle one can be removed.
	bigPayload := strings.Repeat("x", 3*splitRecordSize)

	buildMeta := func() *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto).SetSplitLongRecords(true)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	pk := tuple.Tuple{int64(1)}

	// saveSplitRecord writes one record large enough to split, and returns the
	// keyspace it lives in.
	saveSplitRecord := func(md *RecordMetaData) subspace.Subspace {
		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(&gen.Order{
				OrderId:    proto.Int64(1),
				Price:      proto.Int32(500),
				VectorData: []byte(bigPayload),
			})
		})
		Expect(err).NotTo(HaveOccurred())
		return ks
	}

	// deleteChunk removes the split segment at the given suffix, producing the
	// corrupt shape under test.
	deleteChunk := func(md *RecordMetaData, ks subspace.Subspace, suffix int64) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			key := store.recordsSubspace.Pack(appendToTuple(pk, suffix))
			rtx.Transaction().Clear(fdb.Key(key))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// chunkSuffixes reports which split segments currently exist, so a test
	// cannot silently corrupt nothing.
	chunkSuffixes := func(md *RecordMetaData, ks subspace.Subspace) []int64 {
		var out []int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			pkSub := store.recordsSubspace.Sub(pk...)
			begin, end := pkSub.FDBRangeKeys()
			kvs, err := rtx.Transaction().GetRange(
				fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			for _, kv := range kvs {
				t, err := store.recordsSubspace.Unpack(kv.Key)
				if err != nil {
					return nil, err
				}
				if s, ok := t[len(t)-1].(int64); ok {
					out = append(out, s)
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return out
	}

	scanRecords := func(md *RecordMetaData, ks subspace.Subspace) error {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			return AsList(ctx, store.ScanRecords(nil, ForwardScan()))
		})
		return err
	}

	loadRecord := func(md *RecordMetaData, ks subspace.Subspace) error {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			return store.LoadRecord(pk)
		})
		return err
	}

	Describe("a segment missing from the MIDDLE (mis-sequenced, start present)", func() {
		It("is a FoundSplitOutOfOrderError on the scan path", func() {
			md := buildMeta()
			ks := saveSplitRecord(md)
			Expect(chunkSuffixes(md, ks)).To(ContainElements(int64(1), int64(2), int64(3)),
				"the fixture must actually split, or the corruption below removes nothing")

			deleteChunk(md, ks, 2)

			err := scanRecords(md, ks)
			Expect(err).To(HaveOccurred())

			var outOfOrder *FoundSplitOutOfOrderError
			Expect(errors.As(err, &outOfOrder)).To(BeTrue(),
				"expected FoundSplitOutOfOrderError, got %T: %v", err, err)
			Expect(outOfOrder.Expected).To(Equal(int64(2)))
			Expect(outOfOrder.Found).To(Equal(int64(3)))

			// Java's class extends RecordCoreStorageException, so a caller
			// matching the general storage-corruption type must still match.
			var storage *RecordCoreStorageError
			Expect(errors.As(err, &storage)).To(BeTrue(),
				"FoundSplitOutOfOrderError must unwrap to *RecordCoreStorageError — "+
					"Java's FoundSplitOutOfOrderException IS a RecordCoreStorageException")
		})

		It("is a FoundSplitOutOfOrderError on the point-load path", func() {
			md := buildMeta()
			ks := saveSplitRecord(md)
			deleteChunk(md, ks, 2)

			err := loadRecord(md, ks)
			Expect(err).To(HaveOccurred())

			var outOfOrder *FoundSplitOutOfOrderError
			Expect(errors.As(err, &outOfOrder)).To(BeTrue(),
				"expected FoundSplitOutOfOrderError, got %T: %v", err, err)
			Expect(outOfOrder.Expected).To(Equal(int64(2)))
			Expect(outOfOrder.Found).To(Equal(int64(3)))
		})
	})

	Describe("the START segment missing (truncated at the front)", func() {
		// The dimension the single untyped error erased. Java calls this
		// without-start; Go called it "out of order", which asserts the opposite
		// of the truth — that every piece is present.
		It("is a FoundSplitWithoutStartError, NOT an out-of-order error", func() {
			md := buildMeta()
			ks := saveSplitRecord(md)
			deleteChunk(md, ks, 1)

			err := scanRecords(md, ks)
			Expect(err).To(HaveOccurred())

			var withoutStart *FoundSplitWithoutStartError
			Expect(errors.As(err, &withoutStart)).To(BeTrue(),
				"expected FoundSplitWithoutStartError, got %T: %v", err, err)
			Expect(withoutStart.NextIndex).To(Equal(int64(2)),
				"the segment found in place of the start")
			Expect(withoutStart.Reverse).To(BeFalse(), "this fixture scans forward")

			var outOfOrder *FoundSplitOutOfOrderError
			Expect(errors.As(err, &outOfOrder)).To(BeFalse(),
				"a truncated record must NOT report as out-of-order: that says the "+
					"segments are all present and merely mis-sequenced, which is the "+
					"opposite of the damage. Java separates these at "+
					"SplitHelper.java:824-834")
		})

		It("records the scan DIRECTION, which decides how the report reads", func() {
			md := buildMeta()
			ks := saveSplitRecord(md)
			deleteChunk(md, ks, 1)

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				return AsList(ctx, store.ScanRecords(nil, ReverseScan()))
			})
			Expect(err).To(HaveOccurred())

			var withoutStart *FoundSplitWithoutStartError
			Expect(errors.As(err, &withoutStart)).To(BeTrue(),
				"expected FoundSplitWithoutStartError, got %T: %v", err, err)
			Expect(withoutStart.Reverse).To(BeTrue(),
				"Java carries SPLIT_REVERSE because under a reverse scan 'no start yet' "+
					"is the expected intermediate state rather than evidence of damage")
		})
	})
})
