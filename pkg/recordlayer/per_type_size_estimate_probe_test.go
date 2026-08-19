package recordlayer

// CQ-88 gating experiment: can a per-RECORD-TYPE size estimate discriminate
// tables, and at what size does it stop?
//
// The design this probe gates reads per-type cardinality from the RECORDS
// subspace rather than from any index: a relational table's primary key is
// prefixed with its record-type key (metadata/builder.go), so each table
// occupies a contiguous range and `EstimateRecordsSizeInRange` addresses it
// directly. That sidesteps the per-index/per-type mismatch entirely — an index
// may span types, a FanOut index emits many entries per record, a type may have
// no index at all, and none of that matters if you ask the records.
//
// THE QUESTION THAT DECIDES IT is not accuracy at the top end — the sibling
// probe already measured ~1.3% there. It is the FLOOR. A sampled estimator
// returns 0 below its granularity, and the design turns a 0 into a refusal.
// Refusal is safe only if it is rare; if every table under some size refuses,
// then statistics are unavailable for exactly the small tables whose smallness
// is the most valuable thing a join-order decision could know.
//
// So: two types, deliberately lopsided, and the small one is sized near where
// the sibling probe found the estimator giving up.

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("PerTypeSizeEstimateProbe", func() {
	It("measures whether a per-type range estimate discriminates a big and a small table", func() {
		ctx := context.Background()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		// Record-type-PREFIXED primary keys: this is what makes each type a
		// contiguous range, and it is what the relational builder does for every
		// SQL table (metadata/builder.go, RecordTypeKey() prepended).
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		metaData, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		sub := specSubspace()

		// Lopsided on purpose: 3000 orders against 150 customers. A join between
		// them should drive from the SMALL side, and that is the decision a
		// per-type size is supposed to inform.
		const orders, customers = 3000, 150
		const batch = 500
		write := func(fn func(store *FDBRecordStore, i int) error, n int) {
			for start := 0; start < n; start += batch {
				s := start
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).
						SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
					if err != nil {
						return nil, err
					}
					for i := s; i < s+batch && i < n; i++ {
						if err := fn(store, i); err != nil {
							return nil, err
						}
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		write(func(store *FDBRecordStore, i int) error {
			_, e := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(int64(i)), Price: proto.Int32(int32(i % 100)),
			})
			return e
		}, orders)
		write(func(store *FDBRecordStore, i int) error {
			_, e := store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(int64(i))})
			return e
		}, customers)

		type row struct {
			typeName  string
			wantRows  int
			typeKey   any
			est       int64
			trueBytes int64
			trueCount int
		}
		rows := []row{
			{typeName: "Order", wantRows: orders},
			{typeName: "Customer", wantRows: customers},
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := range rows {
				rt := store.metaData.GetRecordType(rows[i].typeName)
				Expect(rt).NotTo(BeNil())
				key := rt.GetRecordTypeKey()
				rows[i].typeKey = key

				est, eErr := store.EstimateRecordsSizeInRange(TupleRangeAllOf(tuple.Tuple{key}))
				Expect(eErr).NotTo(HaveOccurred())
				rows[i].est = est

				// Ground truth by scanning the same range.
				cur := store.ScanRecordsByType(rows[i].typeName, nil, ScanProperties{})
				n, bytes := 0, int64(0)
				for {
					res, cErr := cur.OnNext(ctx)
					Expect(cErr).NotTo(HaveOccurred())
					if !res.HasNext() {
						break
					}
					n++
					if rec := res.GetValue(); rec != nil {
						bytes += int64(proto.Size(rec.Record))
					}
				}
				_ = cur.Close()
				rows[i].trueCount, rows[i].trueBytes = n, bytes
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		fmt.Fprintf(GinkgoWriter, "\nPERTYPE  %-10s %8s %8s %12s %12s\n",
			"type", "want", "scanned", "est_bytes", "true_proto")
		for _, r := range rows {
			fmt.Fprintf(GinkgoWriter, "PERTYPE  %-10s %8d %8d %12d %12d\n",
				r.typeName, r.wantRows, r.trueCount, r.est, r.trueBytes)
		}
		big, small := rows[0], rows[1]
		fmt.Fprintf(GinkgoWriter, "PERTYPE  VERDICT discriminates=%t  big/small=%s\n",
			big.est > 0 && small.est > 0 && big.est > small.est,
			func() string {
				if small.est == 0 {
					return "N/A (small table estimated ZERO — refusal, not a ratio)"
				}
				return fmt.Sprintf("%.1fx", float64(big.est)/float64(small.est))
			}())

		// Only the fixture is asserted: the probe's verdict is its output.
		Expect(big.trueCount).To(Equal(orders))
		Expect(small.trueCount).To(Equal(customers))
	})
})
