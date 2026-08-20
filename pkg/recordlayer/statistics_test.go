package recordlayer

// RFC-236: the collector's own tests. Every claim the reader will rest on is
// asserted here against real FDB, because the reader can only be as honest as
// what the collector wrote.

import (
	"context"
	"reflect"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
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

	// RECORDS ARE NOT KV PAIRS, and this is the dimension where a plausible
	// implementation of this collector is wrong.
	//
	// The record layer SPLITS a record over 100KB into chunks stored at
	// suffixes 1, 2, 3… and stores the record version inline at suffix -1. So
	// one logical record can occupy four or five keys, and the ratio depends on
	// the record's size — which means a key count is not even a consistent
	// OVER-estimate, it is a number whose relationship to the row count varies
	// per row.
	//
	// This is not a hypothetical shape to be wary of. Java's own
	// SizeStatisticsResults — the closest existing thing to a statistic in
	// either codebase — reports `keyCount`, "the total number of keys in the
	// requested key range" (SizeStatisticsResults.java:182-185). Anyone
	// reaching for the existing machinery to answer "how many rows" gets keys.
	//
	// The collector counts RECORDS because it iterates the record cursor rather
	// than a key range. A test with only small records cannot tell the two
	// apart: every record is one key, so both implementations agree and the
	// suite is green with the bug fully present.
	It("counts split records once, not once per chunk", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := NewStatisticsSubspace(specSubspace())

		// Splitting is opt-in per store, so this test builds its own metadata
		// with it ON. That is itself worth noticing: a store WITHOUT it simply
		// refuses an oversized record, so the split layout only exists where an
		// operator asked for it — and a collector tested only against the
		// default metadata never meets the layout at all.
		splitBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		splitBuilder.SetSplitLongRecords(true)
		splitBuilder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		splitBuilder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		splitBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		splitMeta, mErr := splitBuilder.Build()
		Expect(mErr).NotTo(HaveOccurred())
		splitBuilderFor := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(splitMeta).SetSubspace(sub).CreateOrOpen()
		}

		// 250KB each: over the 100KB split threshold, so each of these becomes
		// several chunks. Two big records and one small one, so the KV count
		// and the record count differ by a factor that is neither 1 nor
		// constant.
		big := make([]byte, 250*1024)
		for i := range big {
			big[i] = byte(i)
		}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(splitMeta).SetSubspace(sub).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			for i := 0; i < 2; i++ {
				if _, e := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)), VectorData: big,
				}); e != nil {
					return nil, e
				}
			}
			_, e := store.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(1)})
			return nil, e
		})
		Expect(err).NotTo(HaveOccurred())

		// Count the KEYS in the records subspace, so the assertion below can
		// say what a key-counting collector would have reported instead of
		// merely asserting the right answer.
		var keyCount int
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			recordsSub := sub.Sub(RecordKey)
			begin, end := recordsSub.FDBRangeKeys()
			kvs, kErr := rtx.Transaction().GetRange(
				fdb.KeyRange{Begin: fdb.Key(begin.FDBKey()), End: fdb.Key(end.FDBKey())},
				fdb.RangeOptions{}).GetSliceWithError()
			if kErr != nil {
				return nil, kErr
			}
			keyCount = len(kvs)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		report, err := CollectStatistics(ctx, sharedDB, splitBuilderFor, stats, CollectOptions{BatchSize: 10})
		Expect(err).NotTo(HaveOccurred())

		// The guard that keeps this test from going vacuous: if splitting ever
		// stops happening (a raised threshold, a different serializer), the
		// counts coincide and the assertion below passes for the wrong reason.
		Expect(keyCount).To(BeNumerically(">", 3),
			"the three records occupy only %d keys, so nothing here is split and this "+
				"test can no longer tell a record count from a key count", keyCount)

		Expect(report.Collected["Order"].Count).To(BeEquivalentTo(3),
			"collected %d for 3 records stored across %d keys — a collector counting KEYS "+
				"would report about %d here, and would then tell the planner this table is "+
				"several times larger than it is",
			report.Collected["Order"].Count, keyCount, keyCount)

		// And the stored bytes must agree with the report, since the reader
		// reads the store and not the report.
		stored, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(stored.PerType["Order"].Count).To(BeEquivalentTo(3))
	})

	// THE COLLECTOR MUST NOT MIGRATE THE STORE IT MEASURES.
	//
	// Opening a record store runs checkPossiblyRebuild, which WRITES — a header
	// version bump, index clears, rebuild marks — whenever the metadata handed
	// to it is NEWER than the store header. A job whose entire purpose is to
	// count rows must not do that, and the store it would do it to is the one
	// already mid-migration: metadata evolved, header not yet reconciled. That
	// is a normal transient state for a fleet, not a corrupt one.
	//
	// Both callers therefore open with SetSkipPossiblyRebuild + Open, never
	// CreateOrOpen (embedded/connection.go, core/fleet/statistics.go). This is
	// the pin for that choice, and it needs the VERSION SKEW to exist — which is
	// why it cannot live in the CLI suite, where a schema is created and
	// collected at the same version and both open modes behave identically.
	//
	// The test asserts BOTH directions. Without the guard the store changes
	// (proving the hazard is real and this fixture reaches it); with the guard
	// it does not (proving the guard is what prevents it). Asserting only the
	// second would pass just as happily against a fixture with no skew at all.
	It("does not migrate a store whose metadata has moved ahead of its header", func() {
		ctx := context.Background()

		// v2 is the same schema at a higher metadata version: newer than what
		// the store's header will record, which is the condition
		// checkPossiblyRebuild acts on.
		newerMeta := func() *RecordMetaData {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
			b.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
			b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
			b.SetVersion(metaData.Version() + 1)
			m, e := b.Build()
			Expect(e).NotTo(HaveOccurred())
			return m
		}()

		snapshot := func(sub subspace.Subspace) map[string]string {
			out := map[string]string{}
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				begin, end := sub.FDBRangeKeys()
				kvs, rErr := rtx.ReadTransaction(true).GetRange(
					fdb.KeyRange{Begin: fdb.Key(begin.FDBKey()), End: fdb.Key(end.FDBKey())},
					fdb.RangeOptions{}).GetSliceWithError()
				if rErr != nil {
					return nil, rErr
				}
				for _, kv := range kvs {
					out[string(kv.Key)] = string(kv.Value)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return out
		}

		// run seeds a store at the ORIGINAL metadata version, then collects with
		// the NEWER metadata, and reports whether the store's bytes moved.
		// arm names the subspace, because specSubspace() is keyed on the SPEC and
		// would hand both arms the same store — the second arm would then seed at
		// the original version over a header the first arm had already bumped, and
		// fail as stale metadata rather than measuring anything.
		run := func(arm string, skipRebuild bool) bool {
			sub := specSubspace().Sub(arm)
			stats := NewStatisticsSubspace(specSubspace().Sub(arm + "-stats"))
			seed(ctx, sub, 4, 2)
			before := snapshot(sub)
			Expect(before).NotTo(BeEmpty())

			_, err := CollectStatistics(ctx, sharedDB,
				func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
					b := NewStoreBuilder().SetContext(rtx).
						SetMetaDataProvider(newerMeta).SetSubspace(sub)
					if skipRebuild {
						return b.SetSkipPossiblyRebuild(true).Open()
					}
					return b.CreateOrOpen()
				}, stats, CollectOptions{BatchSize: 100})
			Expect(err).NotTo(HaveOccurred())
			return !reflect.DeepEqual(before, snapshot(sub))
		}

		// The fixture must actually reach the hazard, or the guarded arm below
		// is asserting over a condition that never arises.
		Expect(run("unguarded", false)).To(BeTrue(),
			"opening WITHOUT SetSkipPossiblyRebuild left the store byte-identical, so this "+
				"fixture does not reproduce the metadata-ahead-of-header condition and the "+
				"guarded arm below proves nothing")

		Expect(run("guarded", true)).To(BeFalse(),
			"the collector migrated the store it was only supposed to measure — "+
				"SetSkipPossiblyRebuild + Open is what prevents that, and both call sites "+
				"(embedded/connection.go, core/fleet/statistics.go) depend on it")
	})
})
