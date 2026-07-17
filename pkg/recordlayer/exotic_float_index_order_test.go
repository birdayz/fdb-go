package recordlayer

import (
	"context"
	"math"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// RFC-180 G4: exotic-float cross-plan agreement, end to end against REAL FDB
// index entries. NaN/±Inf/-0.0 have no SQL literal path (rejected by design),
// so they enter through the record-layer API — exactly how a Java app on the
// shared cluster writes them. The axis the test-suite audit found unprobed:
// does the PHYSICAL index order (what an ordered index scan yields) relate to
// the in-memory comparator order (values.CompareFloat64, backing sort/merge/
// dedup and MIN/MAX) the way the documented contract says?
//
// The contract (pinned generatively by FuzzCompareValues_TupleOrderAgreement;
// pinned here against a real index):
//   - Non-NaN values: index byte order ≡ CompareFloat64 order, including
//     -0.0 < +0.0 (the tuple codec's sign-flip transform preserves the raw
//     IEEE total order).
//   - NaN payloads are the one documented split, and it is Java parity: both
//     tuple codecs pack RAW bits (Go math.Float64bits; Java TupleUtil
//     doubleToRawLongBits), so the INDEX orders sign-set NaNs below -Inf and
//     payload-distinct NaNs apart, while the in-memory comparator collapses
//     all NaNs to one canonical greatest value (Java Double.compare).
var _ = Describe("ExoticFloatIndexOrder", func() {
	ctx := context.Background()

	It("real index entries order exotic doubles by raw-bit total order; in-memory collapses NaNs canonically", func() {
		idx := NewIndex("TypedRecord$val_double", Field("val_double"))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("TypedRecord", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		negNaN := math.Float64frombits(0xFFF8000000000000)   // sign-set canonical-payload NaN
		canonNaN := math.Float64frombits(0x7FF8000000000000) // Java doubleToLongBits canonical
		// Go's math.NaN() is uvnan 0x7FF8000000000001 — one bit ABOVE the
		// Java canonical, so it is itself a "payload" NaN; payloadNaN takes
		// the next bit up so the three sign-clear NaNs are pairwise DISTINCT
		// (a canonicalizing write path would collapse them and fail below).
		goNaN := math.NaN()
		payloadNaN := math.Float64frombits(0x7FF8000000000002)
		writeOrder := []float64{ // deliberately shuffled
			1.5, math.Inf(-1), goNaN, math.Copysign(0, -1),
			negNaN, math.Inf(1), 0.0, canonNaN, -1.5, payloadNaN,
		}
		// Physical index expectation: IEEE-754 raw-bit total order (the
		// tuple codec's order-preserving transform of the RAW bits).
		wantIndexOrder := []float64{
			negNaN, math.Inf(-1), -1.5, math.Copysign(0, -1),
			0.0, 1.5, math.Inf(1), canonNaN, goNaN, payloadNaN,
		}

		ks := specSubspace()
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, v := range writeOrder {
				_, err = store.SaveRecord(&gen.TypedRecord{
					Id: proto.Int64(int64(i + 1)), ValDouble: proto.Float64(v),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// (a) The REAL index entries, in FDB byte order.
			idxSubspace := store.subspace.Sub(IndexKey, idx.SubspaceTupleKey())
			begin, end := idxSubspace.FDBRangeKeys()
			kvs, err := store.context.Transaction().GetRange(
				fdb.KeyRange{Begin: begin, End: end},
				fdb.RangeOptions{},
			).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(kvs).To(HaveLen(len(writeOrder)))
			got := make([]float64, len(kvs))
			for i, kv := range kvs {
				tup, err := idxSubspace.Unpack(kv.Key)
				Expect(err).NotTo(HaveOccurred())
				got[i] = tup[0].(float64)
			}
			for i := range wantIndexOrder {
				Expect(math.Float64bits(got[i])).To(Equal(math.Float64bits(wantIndexOrder[i])),
					"index slot %d: got bits %#x want %#x", i, math.Float64bits(got[i]), math.Float64bits(wantIndexOrder[i]))
			}

			// (b) The in-memory comparator over the same set: identical
			// order for non-NaN values (including -0.0 < +0.0 by BITS),
			// with ALL NaNs collapsed equal and greatest (Double.compare).
			mem := append([]float64(nil), writeOrder...)
			sort.SliceStable(mem, func(i, j int) bool {
				return values.CompareFloat64(mem[i], mem[j]) < 0
			})
			// Non-NaN prefix must match bit for bit (negNaN, which the
			// INDEX puts below -Inf, moves to the NaN tail in-memory).
			wantMemPrefix := []float64{
				math.Inf(-1), -1.5, math.Copysign(0, -1), 0.0, 1.5, math.Inf(1),
			}
			Expect(mem).To(HaveLen(len(writeOrder)))
			for i, w := range wantMemPrefix {
				Expect(math.Float64bits(mem[i])).To(Equal(math.Float64bits(w)),
					"in-memory slot %d", i)
			}
			// The four NaNs (canonical, uvnan, payload, sign-set) sort
			// greatest and mutually equal in-memory.
			for i := len(wantMemPrefix); i < len(mem); i++ {
				Expect(math.IsNaN(mem[i])).To(BeTrue(), "in-memory tail slot %d must be NaN", i)
				Expect(values.CompareFloat64(mem[i], mem[len(wantMemPrefix)])).To(Equal(0))
			}

			// (c) EVALUATED MIN/MAX are a DIFFERENT float semantic than the
			// comparator order asserted in (b): Java Math.min/max
			// (NumericAggregationValue MIN_D/MAX_D, Go's aggMinMax via
			// javaMinF64/javaMaxF64) propagate NaN into BOTH extremes, so
			// the evaluated MIN over this set is NaN — never -Inf, which is
			// what a comparator-based MIN would produce. NOTE: Go's raw
			// math.Min/math.Max resolve Min(-Inf,NaN)=-Inf (infinity checks
			// first) — the divergence this very pin surfaced; the executor
			// unit test TestAggMinMax_NaNPropagatesOverInf pins the real
			// aggregate function, this fold mirrors its Java semantics.
			evalMin, evalMax := writeOrder[0], writeOrder[0]
			for _, v := range writeOrder[1:] {
				if math.IsNaN(v) || math.IsNaN(evalMin) {
					evalMin, evalMax = math.NaN(), math.NaN()
					continue
				}
				evalMin, evalMax = math.Min(evalMin, v), math.Max(evalMax, v)
			}
			Expect(math.IsNaN(evalMin)).To(BeTrue(), "evaluated MIN propagates NaN (Java Math.min)")
			Expect(math.IsNaN(evalMax)).To(BeTrue(), "evaluated MAX propagates NaN (Java Math.max)")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
