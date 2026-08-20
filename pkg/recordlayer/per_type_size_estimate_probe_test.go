package recordlayer

// THE MEASUREMENT THAT KILLED RFC-236's SECOND DESIGN.
//
// That design read per-type cardinality from the RECORDS subspace rather than
// from any index. The idea was sound and sidestepped a real problem: a
// relational table's primary key is prefixed with its record-type key
// (metadata/builder.go), so each table occupies a contiguous range that
// `EstimateRecordsSizeInRange` addresses directly — no per-index/per-type
// mismatch, no trouble with an index that spans types, a FanOut index emitting
// many entries per record, or a type with no index at all.
//
// THE QUESTION THAT DECIDED IT was not accuracy at the top end. It is the
// FLOOR. A sampled estimator returns 0 below its granularity, and the design
// turned a 0 into a refusal. Refusal is safe only if it is rare — and this
// probe found that every table under roughly 100KB refuses, which is exactly
// the small tables whose smallness is the most valuable thing a join-order
// decision could know. An estimator blind precisely where the decision lives
// cannot inform the decision.
//
// So statistics are COLLECTED by scanning instead (RFC-236 §2). The probe stays
// because the conclusion outlives the design: it is the standing evidence, and
// it is what would notice if FDB's estimator ever gained a usable floor.
//
// WHAT THE COLUMNS MEAN, because two of them are not comparable to each other.
// `est_bytes` is what FDB's sampled estimator reports for the range: KEY plus
// VALUE bytes, including the record-type prefix, split-record suffixes and the
// version entry. `kv_bytes` is the same quantity counted exactly, by reading
// the range. Those two are the comparison. `proto_bytes` is the serialized
// record payload only, and is reported because it is what an application
// thinks its data weighs — it is strictly smaller and comparing IT to the
// estimate would understate the estimator.

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
			typeName   string
			wantRows   int
			typeKey    any
			est        int64
			kvBytes    int64
			protoBytes int64
			trueCount  int
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

				typeRange := TupleRangeAllOf(tuple.Tuple{key})
				est, eErr := store.EstimateRecordsSizeInRange(typeRange)
				Expect(eErr).NotTo(HaveOccurred())
				rows[i].est = est

				// EXACT ground truth for what the estimator is estimating: the KEY
				// plus VALUE bytes actually stored in this type's range, read from
				// the same subspace EstimateRecordsSizeInRange addresses. The two
				// numbers then answer the same question. A payload-only figure is
				// strictly smaller and would understate the estimator rather than
				// measure it.
				kvRange := typeRange.ToFDBRange(store.subspace.Sub(RecordKey))
				kvs, kErr := rtx.Transaction().GetRange(kvRange, fdb.RangeOptions{}).GetSliceWithError()
				Expect(kErr).NotTo(HaveOccurred())
				for _, kv := range kvs {
					rows[i].kvBytes += int64(len(kv.Key) + len(kv.Value))
				}

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
				rows[i].trueCount, rows[i].protoBytes = n, bytes
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		fmt.Fprintf(GinkgoWriter, "\nPERTYPE  %-10s %8s %8s %12s %12s %12s\n",
			"type", "want", "scanned", "est_bytes", "kv_bytes", "proto_bytes")
		for _, r := range rows {
			fmt.Fprintf(GinkgoWriter, "PERTYPE  %-10s %8d %8d %12d %12d %12d\n",
				r.typeName, r.wantRows, r.trueCount, r.est, r.kvBytes, r.protoBytes)
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
