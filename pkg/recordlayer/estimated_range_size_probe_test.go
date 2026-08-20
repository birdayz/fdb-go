package recordlayer

// THE MEASUREMENT THAT KILLED RFC-236's FIRST DESIGN.
//
// That design had each index maintainer answer selectivity questions from
// `GetEstimatedRangeSizeBytes`, refusing (`ok=false`) below the range width
// where the estimate stops tracking — a width this probe was written to
// OUTPUT rather than have somebody pick.
//
// It printed a number that ended the design instead. `GetEstimatedRangeSizeBytes`
// is served from storage-server SAMPLING, and below roughly 100KB it does not
// degrade gracefully: it reports 0 for ranges that are demonstrably not empty,
// and quantizes everything above. There is no width at which it starts
// discriminating usefully for the question being asked, because a join-order
// decision turns on exactly the small-versus-large distinction that lives
// entirely below the floor.
//
// The probe stays because the conclusion outlives the design. It is the standing
// evidence for why statistics are COLLECTED by scanning rather than estimated at
// plan time (RFC-236 §2), and it is what would notice if FDB's estimator ever
// gained a usable floor — which would reopen the cheaper design.

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("EstimatedRangeSizeProbe", func() {
	It("measures where the sampled estimator stops discriminating", func() {
		ctx := context.Background()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		// A VALUE index on price: the shape a selectivity question is asked of.
		priceIdx := NewIndex("order_price", Field("price"))
		builder.AddIndex("Order", priceIdx)
		metaData, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		sub := specSubspace()

		// totalRows is deliberately modest: the question is the estimator's
		// FLOOR behaviour, which shows up at small ranges, not at scale. The
		// price distribution is what creates the selectivity ladder — price p
		// appears with multiplicity, so a prefix on price selects a known slice.
		const totalRows = 4000
		const batch = 500
		for start := 0; start < totalRows; start += batch {
			s := start
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := s; i < s+batch && i < totalRows; i++ {
					// price buckets: 0 gets half the rows, then geometric-ish
					// slices, down to a bucket with a single row.
					var price int32
					switch {
					case i < totalRows/2:
						price = 0
					case i < totalRows/2+totalRows/4:
						price = 1
					case i < totalRows/2+totalRows/4+totalRows/10:
						price = 2
					case i < totalRows-1:
						price = 3
					default:
						price = 4 // exactly one row
					}
					if _, err := store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(int64(i)),
						Price:   proto.Int32(price),
					}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}

		type row struct {
			label     string
			prefix    tuple.Tuple
			trueCount int
			bytes     int64
		}
		var rows []row

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			idxSub := store.IndexSubspace(priceIdx)

			measure := func(label string, prefix tuple.Tuple) {
				var begin, end fdb.Key
				target := idxSub
				if len(prefix) > 0 {
					target = idxSub.Sub(prefix...)
				}
				b, e := target.FDBRangeKeys()
				begin, end = fdb.Key(b.FDBKey()), fdb.Key(e.FDBKey())
				// SNAPSHOT: a planner read must never add a conflict range.
				sz, szErr := rtx.ReadTransaction(true).
					GetEstimatedRangeSizeBytes(fdb.KeyRange{Begin: begin, End: end}).Get()
				Expect(szErr).NotTo(HaveOccurred())

				// Ground truth: count the entries actually in that range.
				kvs, rErr := rtx.ReadTransaction(true).GetRange(
					fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				Expect(rErr).NotTo(HaveOccurred())

				rows = append(rows, row{label: label, prefix: prefix, trueCount: len(kvs), bytes: sz})
			}

			measure("whole index", nil)
			measure("price=0 (~50%)", tuple.Tuple{int64(0)})
			measure("price=1 (~25%)", tuple.Tuple{int64(1)})
			measure("price=2 (~10%)", tuple.Tuple{int64(2)})
			measure("price=3 (small)", tuple.Tuple{int64(3)})
			measure("price=4 (1 row)", tuple.Tuple{int64(4)})
			measure("price=99 (empty)", tuple.Tuple{int64(99)})
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		fmt.Fprintf(GinkgoWriter, "\nESTPROBE  %-18s %10s %14s %14s\n", "range", "entries", "est_bytes", "bytes/entry")
		for _, r := range rows {
			per := "n/a"
			if r.trueCount > 0 {
				per = fmt.Sprintf("%.1f", float64(r.bytes)/float64(r.trueCount))
			}
			fmt.Fprintf(GinkgoWriter, "ESTPROBE  %-18s %10d %14d %14s\n", r.label, r.trueCount, r.bytes, per)
		}

		// The probe's VERDICT is its output, so the only hard assertion is the
		// one that would make the output meaningless: the population must exist.
		Expect(rows[0].trueCount).To(Equal(totalRows),
			"the whole-index scan found %d entries, want %d — the fixture did not build, "+
				"so every row below is a measurement of nothing", rows[0].trueCount, totalRows)
	})
})
