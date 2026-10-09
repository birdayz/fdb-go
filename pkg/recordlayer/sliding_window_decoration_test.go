package recordlayer

import (
	"bytes"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("SlidingWindowIndex decoration", func() {
	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("does not decorate an index whose window is reachable only under an OR", func() {
		// A NEGATIVE RESULT, pinned because it is what makes the placement check
		// above narrower than it looks. Java's
		// SlidingWindowIndexMaintainerFactory.findRowNumberWindowPredicate
		// recurses through AND and NOT through OR, so OR(rowWindow, TRUE) is not
		// a sliding window index at all: the factory hands back the undecorated
		// vector factory, the validator never runs, and the metadata is accepted.
		//
		// Go matches that rather than refusing, because refusing would make a
		// Java-authored store unopenable. If findRowNumberWindowPredicateProto
		// is ever widened to recurse through OR, this spec fails — and that
		// failure is the signal that the placement check has become reachable
		// for this shape and the two must be reconciled.
		idx := NewVectorIndex("sw_or_only", Concat(Field("coord_x"), Field("coord_y")), 2)
		rn := &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"},
			Size:          proto.Int32(2),
			Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
		}}
		trueArm := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		}}
		Expect(idx.SetPredicateProto(&gen.Predicate{OrPredicate: &gen.OrPredicate{
			Children: []*gen.Predicate{rn, trueArm},
		}})).To(Succeed())

		Expect(idx.HasRowNumberWindowPredicate()).To(BeFalse())
		Expect(isSlidingWindowIndex(idx)).To(BeFalse())

		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not decorate a non-vector index that carries a window predicate", func() {
		// Java's registry hands back the undecorated factory here, so the index
		// holds every record. Go must not refuse the metadata — refusing would
		// make a Java-authored store unopenable — and must not decorate either.
		idx := NewIndex("value_with_window", Field("price"))
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"price"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		})).To(Succeed())

		Expect(isSlidingWindowIndex(idx)).To(BeFalse())

		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// It is still classified as a PARTIAL index, so nothing serves it as a
		// full one. That is the property that makes not-refusing safe.
		Expect(idx.HasFilteringPredicate()).To(BeTrue())
	})

	It("does not decorate a SPFresh index that carries a window predicate", func() {
		// Deliberate: keyspace 10's layout is the wire contract for Java's HNSW
		// vector index. SPFresh is a Go-only extension (RFC-094), so pairing the
		// two would write bytes under prefix 10 that no Java engine can read.
		idx := NewIndex("spfresh_with_window", KeyWithValue(Field("vector_data"), 0))
		idx.Type = IndexTypeVectorSPFresh
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"price"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		})).To(Succeed())

		Expect(idx.HasRowNumberWindowPredicate()).To(BeTrue())
		Expect(isSlidingWindowIndex(idx)).To(BeFalse(),
			"SPFresh must never be decorated: prefix 10's layout is Java's, and a "+
				"Go-only pairing would write bytes Java cannot interpret")
	})
})

var _ = Describe("SlidingWindow comparators", func() {
	// A unit pin that drives EVERY arm of the comparator with explicit state,
	// rather than relying on whichever arms the FDB corpus above happens to
	// reach. MIN and MAX are mirror images, so an arm exercised only in one
	// direction is an untested arm in the other.
	It("orders entry keys in both directions", func() {
		lo := tuple.Tuple{int64(10), int64(1)}
		hi := tuple.Tuple{int64(20), int64(1)}
		same := tuple.Tuple{int64(10), int64(1)}

		Expect(slidingWindowMin.isBetter(lo, hi)).To(BeTrue())
		Expect(slidingWindowMin.isBetter(hi, lo)).To(BeFalse())
		Expect(slidingWindowMin.isBetter(lo, same)).To(BeFalse(), "isBetter is STRICT")

		Expect(slidingWindowMax.isBetter(hi, lo)).To(BeTrue())
		Expect(slidingWindowMax.isBetter(lo, hi)).To(BeFalse())
		Expect(slidingWindowMax.isBetter(hi, hi)).To(BeFalse())

		// isInWindow is inclusive of the boundary itself, in both directions.
		Expect(slidingWindowMin.isInWindow(lo, hi)).To(BeTrue())
		Expect(slidingWindowMin.isInWindow(hi, hi)).To(BeTrue())
		Expect(slidingWindowMin.isInWindow(hi, lo)).To(BeFalse())

		Expect(slidingWindowMax.isInWindow(hi, lo)).To(BeTrue())
		Expect(slidingWindowMax.isInWindow(lo, lo)).To(BeTrue())
		Expect(slidingWindowMax.isInWindow(lo, hi)).To(BeFalse())

		// isWorseOrEqual is the negation of isBetter — the equality case is the
		// one that decides whether a tying entry becomes the new boundary while
		// the window is still filling.
		Expect(slidingWindowMin.isWorseOrEqual(hi, lo)).To(BeTrue())
		Expect(slidingWindowMin.isWorseOrEqual(lo, same)).To(BeTrue())
		Expect(slidingWindowMin.isWorseOrEqual(lo, hi)).To(BeFalse())

		Expect(slidingWindowMax.isWorseOrEqual(lo, hi)).To(BeTrue())
		Expect(slidingWindowMax.isWorseOrEqual(hi, hi)).To(BeTrue())
		Expect(slidingWindowMax.isWorseOrEqual(hi, lo)).To(BeFalse())
	})

	It("orders negative and mixed-width integers by tuple encoding, not by byte length", func() {
		// The comparator compares PACKED bytes, which is only sound because the
		// FDB tuple encoding is order-preserving. Negative integers are the case
		// where a naive byte comparison of the VALUES would disagree.
		neg := tuple.Tuple{int64(-100), int64(1)}
		zero := tuple.Tuple{int64(0), int64(1)}
		big := tuple.Tuple{int64(1 << 40), int64(1)}

		Expect(slidingWindowMin.isBetter(neg, zero)).To(BeTrue())
		Expect(slidingWindowMin.isBetter(zero, big)).To(BeTrue())
		Expect(slidingWindowMin.isBetter(neg, big)).To(BeTrue())
		Expect(slidingWindowMax.isBetter(big, neg)).To(BeTrue())

		// ...and the packed order matches the physical key order FDB scans in.
		Expect(bytes.Compare(neg.Pack(), zero.Pack())).To(BeNumerically("<", 0))
		Expect(bytes.Compare(zero.Pack(), big.Pack())).To(BeNumerically("<", 0))
	})

	It("stores the count the way Java reads it", func() {
		// Java: Tuple.from(value).pack() / Tuple.fromBytes(bytes).getLong(0).
		// A raw little-endian int64 here would be silently unreadable by Java.
		for _, v := range []int64{0, 1, 42, 1 << 40, -1} {
			encoded := encodeSlidingWindowLong(v)
			Expect(encoded).To(Equal(tuple.Tuple{v}.Pack()))
			decoded, err := decodeSlidingWindowLong(encoded)
			Expect(err).NotTo(HaveOccurred())
			Expect(decoded).To(Equal(v))
		}
		// An absent key decodes as zero, matching Java's `counterBytes == null`.
		zero, err := decodeSlidingWindowLong(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(zero).To(Equal(int64(0)))
	})

	It("keyAfter is the immediate successor", func() {
		Expect(slidingWindowKeyAfter([]byte{0x01, 0x02})).To(Equal([]byte{0x01, 0x02, 0x00}))
		Expect(slidingWindowKeyAfter(nil)).To(Equal([]byte{0x00}))
		// It must not mutate its input — the caller still holds the boundary key.
		orig := []byte{0xff}
		_ = slidingWindowKeyAfter(orig)
		Expect(orig).To(Equal([]byte{0xff}))
	})
})
