package recordlayer

// RFC-236: the collector's own tests. Every claim the reader will rest on is
// asserted here against real FDB, because the reader can only be as honest as
// what the collector wrote.

import (
	"context"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("CollectStatistics", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	// seed writes n orders and m customers, returning the store subspace.
	seed := func(ctx context.Context, sub subspace.Subspace, orders, customers int) {
		const batch = 400
		total := orders + customers
		for start := 0; start < total; start += batch {
			s := start
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := s; i < s+batch && i < total; i++ {
					if i < orders {
						if _, e := store.SaveRecord(&gen.Order{
							OrderId: proto.Int64(int64(i)), Price: proto.Int32(int32(i % 7)),
						}); e != nil {
							return nil, e
						}
						continue
					}
					if _, e := store.SaveRecord(&gen.Customer{
						CustomerId: proto.Int64(int64(i)),
					}); e != nil {
						return nil, e
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	builderFor := func(sub subspace.Subspace) func(*FDBRecordContext) (*FDBRecordStore, error) {
		return func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
		}
	}

	statsRoot := func() StatisticsSubspace {
		// A sibling of the store subspace, never a child: the store's keyspace
		// is Java's (constants.go).
		return NewStatisticsSubspace(subspace.FromBytes(tuple.Tuple{"__stats__", CurrentSpecReport().FullText()}.Pack()))
	}

	It("counts every type exactly, and a scan agrees", func() {
		ctx := context.Background()
		sub := specSubspace()
		const orders, customers = 900, 150
		seed(ctx, sub, orders, customers)

		stats := statsRoot()
		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 100})
		Expect(err).NotTo(HaveOccurred())

		Expect(report.Collected).To(HaveKey("Order"))
		Expect(report.Collected).To(HaveKey("Customer"))
		Expect(report.Collected["Order"].Count).To(Equal(int64(orders)),
			"the collector must count EXACTLY — an approximate count is the thing this "+
				"design exists to avoid, and a wrong count here silently mis-plans every join")
		Expect(report.Collected["Customer"].Count).To(Equal(int64(customers)))
		Expect(report.RecordsScanned).To(Equal(int64(orders + customers)))

		// Round-trip through FDB, which is what the reader will actually see.
		var storeSub subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			st, e := builderFor(sub)(rtx)
			if e != nil {
				return nil, e
			}
			storeSub = st.Subspace()
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		read, ok, err := ReadStatistics(ctx, sharedDB, stats, storeSub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(), "statistics were written but not readable — the reader and "+
			"the writer disagree about the layout, which no amount of correct counting fixes")
		Expect(read.PerType["Order"].Count).To(Equal(int64(orders)))
		Expect(read.PerType["Customer"].Count).To(Equal(int64(customers)))
		Expect(read.CollectedAtUnixNanos).To(BeNumerically(">", 0))
	})

	It("is BATCH-INVARIANT: a store larger than one batch counts the same", func() {
		ctx := context.Background()
		sub := specSubspace()
		const orders, customers = 700, 90
		seed(ctx, sub, orders, customers)
		stats := statsRoot()

		// Batch size 1 forces the continuation path on every record; a batch
		// larger than the store never takes it. If they disagree the
		// continuation handling is dropping or double-counting rows, which a
		// single-batch test cannot see.
		small, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 7})
		Expect(err).NotTo(HaveOccurred())
		big, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 100000})
		Expect(err).NotTo(HaveOccurred())

		Expect(small.Collected["Order"].Count).To(Equal(int64(orders)))
		Expect(small.Collected["Customer"].Count).To(Equal(int64(customers)))
		Expect(big.Collected["Order"].Count).To(Equal(small.Collected["Order"].Count),
			"batch size changed the count: the continuation path drops or repeats records")
		Expect(big.Collected["Customer"].Count).To(Equal(small.Collected["Customer"].Count))
		Expect(big.RecordsScanned).To(Equal(small.RecordsScanned))
	})

	It("records a capped type as ABSENT, never as a partial count", func() {
		ctx := context.Background()
		sub := specSubspace()
		seed(ctx, sub, 500, 20)
		stats := statsRoot()

		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats,
			CollectOptions{BatchSize: 100, MaxRecordsPerType: 100})
		Expect(err).NotTo(HaveOccurred())

		Expect(report.Collected).NotTo(HaveKey("Order"),
			"a type over its cap must be ABSENT. A partial count is a wrong number wearing "+
				"the shape of a right one — the cost model cannot tell it from a small table")
		Expect(report.Skipped).To(HaveKey("Order"))
		Expect(report.Skipped["Order"]).To(ContainSubstring("exceeds MaxRecordsPerType"))
		// The under-cap type is unaffected: capping is per type, not per run.
		Expect(report.Collected).To(HaveKey("Customer"))
		Expect(report.Collected["Customer"].Count).To(Equal(int64(20)))
	})

	It("counts an EMPTY store as no statistics rather than zeros", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()

		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Collected).To(BeEmpty(),
			"a type with no rows must be ABSENT, not Count=0. Absent means 'unknown' and "+
				"falls back to the default; zero would tell the cost model the table is empty, "+
				"which is the most selective claim available and the worst one to get wrong")
		Expect(report.RecordsScanned).To(Equal(int64(0)))
	})

	It("REPLACES a previous run atomically rather than merging with it", func() {
		ctx := context.Background()
		sub := specSubspace()
		seed(ctx, sub, 300, 40)
		stats := statsRoot()

		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 100})
		Expect(err).NotTo(HaveOccurred())

		// Delete every Customer, then re-collect. If the write merged instead of
		// replacing, the stale Customer entry would survive and the reader would
		// plan against a table that no longer has those rows.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, e := builderFor(sub)(rtx)
			if e != nil {
				return nil, e
			}
			for i := 300; i < 340; i++ {
				if _, dErr := store.DeleteRecord(tuple.Tuple{
					metaData.GetRecordType("Customer").GetRecordTypeKey(), int64(i),
				}); dErr != nil {
					return nil, dErr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 100})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Collected).NotTo(HaveKey("Customer"),
			"every Customer was deleted, so the type must vanish from the statistics. A "+
				"surviving entry means the write merged with the previous run, and stale "+
				"counts from two different versions are not comparable")
		Expect(report.Collected["Order"].Count).To(Equal(int64(300)))
	})

	It("keys by STORE, so two stores under one root do not mix", func() {
		ctx := context.Background()
		subA := subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText(), "A"}.Pack())
		subB := subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText(), "B"}.Pack())
		seed(ctx, subA, 250, 10)
		seed(ctx, subB, 60, 30)
		stats := statsRoot()

		ra, err := CollectStatistics(ctx, sharedDB, builderFor(subA), stats, CollectOptions{BatchSize: 100})
		Expect(err).NotTo(HaveOccurred())
		rb, err := CollectStatistics(ctx, sharedDB, builderFor(subB), stats, CollectOptions{BatchSize: 100})
		Expect(err).NotTo(HaveOccurred())

		Expect(ra.Collected["Order"].Count).To(Equal(int64(250)))
		Expect(rb.Collected["Order"].Count).To(Equal(int64(60)),
			"collecting store B overwrote or merged with store A: the key is the store's "+
				"subspace prefix precisely so two stores under one root stay separate")

		// And A survives B's run.
		var storeSubA subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			st, e := builderFor(subA)(rtx)
			if e != nil {
				return nil, e
			}
			storeSubA = st.Subspace()
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		readA, ok, err := ReadStatistics(ctx, sharedDB, stats, storeSubA)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(readA.PerType["Order"].Count).To(Equal(int64(250)))
	})

	It("returns ok=false for a store that was never collected", func() {
		ctx := context.Background()
		sub := specSubspace()
		seed(ctx, sub, 10, 5)
		var storeSub subspace.Subspace
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			st, e := builderFor(sub)(rtx)
			if e != nil {
				return nil, e
			}
			storeSub = st.Subspace()
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, ok, err := ReadStatistics(ctx, sharedDB, statsRoot(), storeSub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(),
			"an uncollected store must read as ABSENT. Anything else invents statistics "+
				"for a store nobody measured")
	})

	It("CLEARS a store's statistics", func() {
		ctx := context.Background()
		sub := specSubspace()
		seed(ctx, sub, 120, 15)
		stats := statsRoot()
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 50})
		Expect(err).NotTo(HaveOccurred())

		var storeSub subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			st, e := builderFor(sub)(rtx)
			if e != nil {
				return nil, e
			}
			storeSub = st.Subspace()
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, ok, err := ReadStatistics(ctx, sharedDB, stats, storeSub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		Expect(ClearStatistics(ctx, sharedDB, stats, storeSub)).To(Succeed())
		_, ok, err = ReadStatistics(ctx, sharedDB, stats, storeSub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "clear left statistics behind")
	})

	It("stamps from the DST SEAM, so a seeded run replays and a reader can expire it", func() {
		ctx := context.Background()
		sub := specSubspace()
		seed(ctx, sub, 30, 5)
		stats := statsRoot()

		before := time.Now().UnixNano()
		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 50})
		Expect(err).NotTo(HaveOccurred())
		after := time.Now().UnixNano()

		// The stamp comes from rtx.Env().Now(), which is nil-safe and falls back
		// to real time in production. Asserting the WINDOW rather than an
		// injected constant is what keeps this honest once a simulated env
		// supplies a different clock: the test pins that a stamp is drawn and
		// persisted, not which clock drew it.
		st := report.Collected["Order"]
		Expect(st.CollectedAtUnixNanos).To(BeNumerically(">=", before))
		Expect(st.CollectedAtUnixNanos).To(BeNumerically("<=", after),
			"the persisted stamp is outside the collection window, so it did not come "+
				"from the run that wrote it")
		Expect(st.CollectedAtVersion).To(BeNumerically(">", 0),
			"a read version must be recorded alongside the wall clock: freshness is "+
				"judged on the VERSION, because clocks can be skewed between the "+
				"collector host and the reader host and versions cannot")
	})
})
