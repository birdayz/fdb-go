package recordlayer

// RFC-236: the collector's own tests. Every claim the reader will rest on is
// asserted here against real FDB, because the reader can only be as honest as
// what the collector wrote.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
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

		// Crossing the cap ABORTS and stores nothing, rather than skipping the
		// type and collecting the rest. Skipping cannot produce a usable outcome:
		// an absent type fails the reader's schema-wide completeness gate, so the
		// other counts are refused with it — a full scan bought for nothing.
		Expect(report.Collected).To(BeEmpty(),
			"a capped run must store nothing. Collecting the other types produces "+
				"counts the completeness gate will refuse anyway, at full scan cost")
		Expect(report.Skipped).To(HaveKey("Order"))
		Expect(report.Skipped["Order"]).To(ContainSubstring("exceeds MaxRecordsPerType"))

		// And nothing reached the store — asserted as NO HEADER specifically.
		// ok==false would hold for any of the eight refusals, so it cannot say
		// that the store is EMPTY rather than holding something unreadable; that
		// is the whole claim here, and it is also the only spec covering the
		// no-header arm.
		_, refusal, _, rErr := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(rErr).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadAbsent,
			"an aborted run must leave the store with NO header. Any other refusal "+
				"means it wrote something and the abort was not clean")
	})

	// A DECLARED TYPE WITH NO ROWS IS AN EXACT ZERO, NOT AN ABSENCE.
	//
	// An earlier revision recorded it as ABSENT, reasoning that "zero tells the
	// cost model the table is empty, which is the most selective claim
	// available". That reasoning was wrong twice over.
	//
	// It is wrong about the danger: NewCollectedStatistics clamps a count below
	// 1 up to 1, so a stored 0 reaches the cost model as a one-row table and
	// cannot collapse the costs above it. And it is wrong about the cost of
	// being careful: the reader requires EVERY declared type to have an entry,
	// so one empty table refused statistics for the entire schema — permanently,
	// until somebody inserted a row. A freshly created schema is mostly empty
	// tables, so the feature was off exactly where it had just been switched on.
	//
	// An exact 0 from a full scan is as trustworthy as an exact 5. ABSENT is
	// reserved for "not counted", which is a capped type, and is a different
	// fact.
	It("records a declared type with no rows as an exact zero", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()

		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.RecordsScanned).To(Equal(int64(0)))

		for _, name := range []string{"Order", "Customer", "TypedRecord"} {
			Expect(report.Collected).To(HaveKey(name),
				"every DECLARED type must get an entry even with no rows, or the reader's "+
					"completeness gate refuses the whole schema forever")
			Expect(report.Collected[name].Count).To(BeZero())
		}
		Expect(report.Skipped).To(BeEmpty(),
			"no row is not the same fact as not counted; only a capped type is skipped")
	})

	// The shape that made the bug above matter, end to end: a schema where SOME
	// tables have rows and one does not. This is the ordinary state of a real
	// schema, and it is the dimension the rest of this file missed — every other
	// case here either populates every type or populates none, and both of those
	// pass with the type-with-no-rows bug fully present.
	It("is usable for a schema where one table is populated and another is empty", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		// Orders only. Customer and TypedRecord are declared and stay empty.
		seed(ctx, sub, 12, 0)

		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Collected["Order"].Count).To(Equal(int64(12)))
		Expect(report.Collected["Customer"].Count).To(BeZero())

		stored, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		// The reader's completeness gate is schema-wide, so this is the
		// assertion that the mixed schema is actually PLANNABLE rather than
		// merely collected.
		for _, name := range []string{"Order", "Customer", "TypedRecord"} {
			Expect(stored.PerType).To(HaveKey(name),
				"a mixed schema must be COMPLETE, or the planner refuses it and the whole "+
					"feature is unreachable for any schema containing an empty table")
		}
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
		// The type is still DECLARED, so it must read as an exact 0 — not vanish,
		// and above all not keep the stale 40. Asserting the VALUE rather than
		// mere absence is the stronger check: absence would also be produced by a
		// bug that simply dropped the key, whereas only a genuine replacement
		// turns 40 into 0.
		Expect(report.Collected).To(HaveKey("Customer"))
		Expect(report.Collected["Customer"].Count).To(BeZero(),
			"every Customer was deleted, so the re-collected count must be 0. A surviving "+
				"40 means the write merged with the previous run, and counts from two "+
				"different versions are not comparable")
		Expect(report.Collected["Order"].Count).To(Equal(int64(300)))

		// And the STORED bytes must agree, since the reader reads those and not
		// the report — a merge would show up here even if the report looked right.
		stored, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(stored.PerType["Customer"].Count).To(BeZero())
		Expect(stored.PerType["Order"].Count).To(Equal(int64(300)))
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

	// A RETRIED BATCH MUST NOT BE COUNTED TWICE.
	//
	// db.Run RETRIES its closure — that is what a transactor is for. A batch
	// that trips transaction_too_old after tallying most of its rows re-runs
	// from the same continuation and re-reads them, so a collector that tallies
	// straight into its durable counters adds those rows twice.
	//
	// The failure direction is the worst available. A retry is likeliest on the
	// LONGEST batches, so the inflation lands preferentially on the biggest
	// tables — precisely the ones a join-order decision is most sensitive to.
	// And it is silent: an inflated count is a well-formed number that every
	// gate downstream passes through to the cost model, which then drives the
	// join from the wrong side because the table it thinks is huge is not.
	//
	// The instrument is a transactor that invokes the closure TWICE and returns
	// the second result, which is what a retry is, minus the timing. That makes
	// the test deterministic rather than dependent on provoking a real 1007 —
	// a fault-injection version would be flaky and would prove less.
	It("does not double-count a batch whose transaction is retried", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		const orders, customers = 500, 20
		seed(ctx, sub, orders, customers)

		// Replay every transaction once. Batches are small, so this exercises
		// the retry path many times over rather than at one convenient point.
		replayer := &replayingTransactor{inner: sharedDB.transactor, replays: 1000}
		replayDB := NewFDBDatabaseWithTransactor(replayer, sharedDB.db)

		report, err := CollectStatistics(ctx, replayDB,
			func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
				return NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
			}, stats, CollectOptions{BatchSize: 50})
		Expect(err).NotTo(HaveOccurred())

		// The guard that stops this from passing vacuously: the replays must
		// actually have happened. Zero replays and the assertions below are a
		// re-run of the ordinary exactness test.
		Expect(replayer.used()).To(BeNumerically(">", 1),
			"the transactor replayed nothing, so no retry was exercised and this test "+
				"proves only what the plain exactness test already does")

		Expect(report.Collected["Order"].Count).To(Equal(int64(orders)),
			"a retried batch was counted more than once — the count is inflated, and it "+
				"inflates the LARGEST tables first because they take the longest batches")
		Expect(report.Collected["Customer"].Count).To(Equal(int64(customers)))
		Expect(report.RecordsScanned).To(Equal(int64(orders + customers)))
	})

	// A BYTE-BOUNDED BATCH MUST STILL COUNT EXACTLY.
	//
	// BatchSize bounds ROWS, which bounds nothing an FDB transaction cares
	// about: a record can be a hundred bytes or, split across KV pairs,
	// hundreds of kilobytes, so a 1000-row batch is anywhere from ~100KB to
	// hundreds of megabytes. The scan therefore also carries a TIME and a BYTES
	// limit, and those are what actually keep a transaction inside FDB's 5s.
	//
	// Adding them buys a new way to be wrong. Reaching a scan limit stops the
	// cursor mid-batch, and if that stop did not hand back a usable
	// continuation, the collector would resume in the wrong place or not at
	// all — and the failure would be a SILENT UNDERCOUNT, which is worse than
	// the oversized transaction it was added to prevent, because an undercount
	// is a well-formed number every downstream gate accepts.
	//
	// So this drives the bytes limit (deterministic, unlike time) hard enough
	// to stop nearly every batch, and requires the total to stay exact.
	It("counts exactly when the byte limit stops batches mid-scan", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		const orders, customers = 500, 20
		seed(ctx, sub, orders, customers)

		counter := &countingTransactor{inner: sharedDB.transactor}
		countingDB := NewFDBDatabaseWithTransactor(counter, sharedDB.db)

		report, err := CollectStatistics(ctx, countingDB,
			func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
				return NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
			}, stats, CollectOptions{
				// A row budget far larger than the store, so ONLY the byte
				// limit can end a batch. If the row limit were doing the work,
				// this would be the ordinary exactness test wearing a new name.
				BatchSize:         1_000_000,
				ScannedBytesLimit: 1024,
			})
		Expect(err).NotTo(HaveOccurred())

		// THE VACUITY GUARD. One scan transaction plus one write means the byte
		// limit never stopped anything and the exactness assertion below is
		// just the plain exactness test again.
		Expect(counter.count()).To(BeNumerically(">", 2),
			"collection took %d transactions, so the 1KB byte limit never stopped a "+
				"batch — this test then proves nothing about resuming from a "+
				"limit-stopped cursor", counter.count())

		Expect(report.Collected["Order"].Count).To(Equal(int64(orders)),
			"a byte-limited batch lost or repeated rows — a scan limit stopped the "+
				"cursor and the continuation did not resume exactly where it stopped")
		Expect(report.Collected["Customer"].Count).To(Equal(int64(customers)))
		Expect(report.RecordsScanned).To(Equal(int64(orders + customers)))
	})

	// CAP AND EMPTY TOGETHER. Seeding every declared type at 0 and capping an
	// oversized one are two rules that meet in the same loop, and each is
	// exercised alone elsewhere in this file — which is precisely the shape that
	// leaves their interaction untested.
	//
	// They must NOT collapse into each other. A capped type is ABSENT, because
	// its true count is unknown; an empty type is an exact 0, because its true
	// count is known and is zero. If seeding overwrote the cap, an over-cap table
	// would read as empty — the most selective claim available, on the table
	// whose size is least known — and the completeness gate would accept the
	// schema instead of refusing it.
	It("keeps a capped type ABSENT while an empty type reads as an exact zero", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		// Orders exceed the cap; Customers and TypedRecords stay empty.
		seed(ctx, sub, 300, 0)

		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats,
			CollectOptions{BatchSize: 100, MaxRecordsPerType: 50})
		Expect(err).NotTo(HaveOccurred())

		// The cap aborts, so NOTHING is stored — including the empty types that
		// would otherwise have read as exact zeros. Seeding and capping do not
		// meet in a stored set any more; the cap wins by ending the run.
		Expect(report.Collected).To(BeEmpty())
		Expect(report.Skipped).To(HaveKey("Order"))

		_, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(),
			"an aborted run must leave the store untouched, so a reader sees "+
				"'not collected' rather than a partial set from a run that gave up")
	})

	// EVERY TRANSACTION IN THIS FILE SURVIVES A RETRY, not just the scan loop.
	//
	// The per-attempt-reset bug was found twice: once in the collector's scan
	// loop, and then again in ReadStatisticsAt, written in the same sitting as
	// the fix for the first. Two instances of one mistake in one file is a
	// statement about the shape being easy to get wrong, so this drives ALL
	// FOUR transactions through a retrying transactor rather than the one that
	// happened to be broken:
	//
	//	CollectStatistics' scan loop  — per-attempt tallies
	//	writeStatistics               — nanos, an idempotent overwrite
	//	ReadStatisticsAt              — out / found / readVersion
	//	ClearStatistics               — captures nothing
	//
	// WHAT THIS ADDS over the double-count test above, which also replays: that
	// one asserts the returned REPORT, and the report is built before
	// writeStatistics runs. So it cannot see a persist corrupted by a retry. This
	// one reads the bytes back, which is the only way writeStatistics enters any
	// assertion at all.
	//
	// It does NOT subsume the read-path test below. Both attempts here observe
	// the same bytes, so unreset read state stays invisible; catching that needs
	// a mutation BETWEEN attempts, which is what that test does.
	It("keeps collect, write, read and clear correct when every transaction retries", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		const orders, customers = 240, 15
		seed(ctx, sub, orders, customers)

		replayer := &replayingTransactor{inner: sharedDB.transactor, replays: 1000}
		replayDB := NewFDBDatabaseWithTransactor(replayer, sharedDB.db)

		_, err := CollectStatistics(ctx, replayDB,
			func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
				return NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
			}, stats, CollectOptions{BatchSize: 40})
		Expect(err).NotTo(HaveOccurred())

		// Read back THROUGH the retrying database too, so ReadStatisticsAt's own
		// closure is exercised under replay rather than only the write path.
		stored, ok, readVersion, err := ReadStatisticsAt(ctx, replayDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(replayer.used()).To(BeNumerically(">", 1),
			"nothing replayed, so this asserts only what the non-retrying tests already do")

		Expect(stored.PerType["Order"].Count).To(Equal(int64(orders)),
			"the PERSISTED count is wrong under retry — a report-only assertion would "+
				"not have seen this, since the report is built before the write")
		Expect(stored.PerType["Customer"].Count).To(Equal(int64(customers)))
		Expect(readVersion).To(BeNumerically(">", 0),
			"a retried read must still yield a cluster version; zero would make the "+
				"freshness gate refuse on every plan")
		Expect(stored.CollectedAtVersion).To(BeNumerically(">", 0))

		// Clear under replay, then confirm it took. A retried ClearRange is
		// idempotent, so the only way this fails is a captured-state bug.
		Expect(ClearStatistics(ctx, replayDB, stats, sub)).To(Succeed())
		_, stillThere, err := ReadStatistics(ctx, replayDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(stillThere).To(BeFalse())
	})

	// THE READ-PATH TWIN, PINNED BY ITS ACTUAL REPRODUCER.
	//
	// ReadStatisticsAt had the same unreset-state bug the scan loop had, and a
	// plain replay does NOT catch it: two identical attempts produce identical
	// results, so the merged-across-attempts state looks correct. The defect
	// only shows when the store CHANGES between attempts, which is exactly what
	// a real retry races.
	//
	// So this clears the statistics between the discarded attempt and the real
	// one. With the reset in place the second attempt sees an empty range and
	// reports found=false. Without it, attempt 1's entries and found=true
	// survive into attempt 2 and get stamped with attempt 2's read version —
	// stale statistics wearing a fresh stamp, which is the single state the
	// freshness gate exists to reject and the one it cannot detect.
	It("does not carry entries across a retry that races a clear", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 30, 5)

		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 10})
		Expect(err).NotTo(HaveOccurred())
		// Precondition: the entries are really there, or "found=false" below
		// would be true for a reason that has nothing to do with the retry.
		_, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		cleared := 0
		replayer := &replayingTransactor{
			inner:   sharedDB.transactor,
			replays: 1,
			beforeReplay: func() {
				// Runs once, between the discarded attempt and the real one.
				Expect(ClearStatistics(ctx, sharedDB, stats, sub)).To(Succeed())
				cleared++
			},
		}
		replayDB := NewFDBDatabaseWithTransactor(replayer, sharedDB.db)

		got, found, _, err := ReadStatisticsAt(ctx, replayDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())

		// Vacuity guards on both halves: the replay happened, and the clear
		// happened. Either at zero makes the assertion below meaningless.
		Expect(replayer.used()).To(BeNumerically(">", 0),
			"nothing replayed — ReadTransact is not exercising the retry path, so this "+
				"asserts only what a plain read already does")
		Expect(cleared).To(Equal(1),
			"the store was not cleared between attempts, so the two attempts saw the same "+
				"bytes and unreset state would be invisible")

		Expect(found).To(BeFalse(),
			"the retry carried the first attempt's entries forward — out/found/readVersion "+
				"must be reset at the top of the RunRead closure, or a retry racing a clear "+
				"yields stale statistics wearing a fresh version stamp")
		Expect(got.PerType).To(BeEmpty())
	})

	// BOTH SWALLOWED READ VERSIONS, PINNED BY FAULT INJECTION.
	//
	// Two call sites took a cluster read version and discarded the error. Both
	// now propagate, and neither propagation had a test — the fix is two lines,
	// which is exactly the kind that reads as obviously right and is therefore
	// never exercised.
	//
	// The write side matters more than the read side. Its version is PERSISTED:
	// a swallow stamps the entry with 0 or with an earlier batch's version, the
	// freshness gate then refuses that schema on every plan, and the operator
	// sees a silent refusal rather than the cluster's error. That is a failure
	// that looks like the feature working as designed.
	It("surfaces a read-version failure instead of stamping a bad version", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 20, 3)

		failing := &grvFailingTransactor{inner: sharedDB.transactor}
		failDB := NewFDBDatabaseWithTransactor(failing, sharedDB.db)

		// WRITE SIDE. A collection whose version read fails must ERROR, not
		// return a report stamped with a version it never obtained.
		report, err := CollectStatistics(ctx, failDB, func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
		}, stats, CollectOptions{BatchSize: 5})
		Expect(err).To(MatchError(errInjectedGRV),
			"a failed GetReadVersion must reach the caller. Swallowed, collection "+
				"succeeds and persists a 0 or stale CollectedAtVersion, and the freshness "+
				"gate then refuses this schema on every plan with no error anywhere")
		Expect(report).To(BeNil())
		Expect(failing.hits()).To(BeNumerically(">", 0),
			"no read version was requested, so nothing was injected and this asserts nothing")

		// And nothing may have been persisted from the failed run.
		//
		// On its own this assertion is VACUOUS: stats and sub are fresh per spec,
		// so an untouched keyspace satisfies it and a mis-wired (stats, sub) pair
		// would too. The successful collect below is its positive control — the
		// SAME pair must then read back true, which is what turns the false above
		// into a measurement of the failed run rather than of an empty keyspace.
		_, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(),
			"a collection that could not stamp a version must write nothing at all")

		// READ SIDE. Same fault, through the reader — and the control for the
		// assertion above.
		Expect(CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})).
			Error().NotTo(HaveOccurred())
		_, okAfter, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(okAfter).To(BeTrue(),
			"the same (stats, sub) pair must read back true after a SUCCESSFUL collect, "+
				"or the false above was a statement about an empty keyspace and not about "+
				"the failed run")
		failing2 := &grvFailingTransactor{inner: sharedDB.transactor}
		_, _, _, rErr := ReadStatisticsAt(ctx, NewFDBDatabaseWithTransactor(failing2, sharedDB.db), stats, sub)
		Expect(rErr).To(MatchError(errInjectedGRV))
		Expect(failing2.hits()).To(BeNumerically(">", 0))
	})

	// THE CAP MUST BOUND THE SCAN, NOT JUST THE OUTPUT.
	//
	// MaxRecordsPerType is documented as capping the WORK spent on one type, and
	// for a while it did not: it compared the finished tally, so a million-row
	// type was read and decoded in full and then had its count discarded. The
	// knob limited the report and nothing else, which is the opposite of a
	// safety valve — an operator setting it to bound load got the load anyway.
	//
	// This asserts the observable that distinguishes the two: RecordsScanned.
	// With the cap suppressing only output it equals the whole store; with the
	// cap abandoning the type mid-scan it is far below.
	It("stops scanning a type once it crosses its cap", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		const orders, customers = 400, 5
		seed(ctx, sub, orders, customers)

		report, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats,
			CollectOptions{BatchSize: 25, MaxRecordsPerType: 50})
		Expect(err).NotTo(HaveOccurred())

		Expect(report.Skipped).To(HaveKey("Order"))
		Expect(report.Collected).To(BeEmpty())

		// The measurement. A full scan reads orders+customers records; abandoning
		// Order at its cap must read far fewer. The bound is generous because
		// abandonment is checked per record within a batch, so the overshoot is
		// at most one batch — the point is the ORDER of magnitude, not a fence.
		Expect(report.RecordsScanned).To(BeNumerically("<", int64(orders+customers)/2),
			"scanned %d of %d records with a cap of 50 — the cap is suppressing the "+
				"count without bounding the work, which is what it exists to do",
			report.RecordsScanned, orders+customers)
	})

	// A RECORD TYPE MAY LEGALLY BE NAMED LIKE THE HEADER.
	//
	// The header entry used to be keyed by the string "__header__". Java-authored
	// metadata can declare a record type with exactly that name, whose per-type
	// write then lands on the header's key: the header is overwritten, the type
	// vanishes from the map, and completeness can never pass for that schema —
	// silently, and only for stores that happen to contain the name.
	//
	// The header is now discriminated by tuple ELEMENT TYPE, which no record-type
	// name can produce. This drives the collision directly by writing a per-type
	// entry under that name and requiring both it and the header to survive.
	It("keeps the header distinct from a record type named like it", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 6, 2)
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		// Write a per-type entry under the old reserved name, as a store whose
		// metadata declares that type would -- AND bump the header's entry count
		// to match, because that is what a real collection of such a schema
		// writes. Adding the entry alone builds a set whose entry count disagrees
		// with its header, which no writer produces and which the reader now
		// rejects as torn; the collision this test is about would then never be
		// reached.
		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			hdrKey := target.Pack(statisticsHeaderKey)
			raw, gErr := tx.Get(fdb.Key(hdrKey)).Get()
			if gErr != nil {
				return nil, gErr
			}
			hdr, hOK := unpackStatistic(raw)
			if !hOK {
				return nil, fmt.Errorf("header unreadable, so this test cannot build the state it needs")
			}
			hdr.Count++
			tx.Set(fdb.Key(hdrKey), packStatistic(hdr))
			// Stamped with the HEADER's own version and time, because that is what
			// a real collection of such a schema writes -- writeStatistics puts the
			// header and every entry down in one transaction with identical stamps.
			// An entry stamped otherwise is a set mixed across runs, which the
			// reader now rejects, and the collision this test is about would never
			// be reached. Same lesson as the header COUNT above: a fixture no
			// writer produces tests the rejection, not the thing it was written for.
			tx.Set(target.Pack(tuple.Tuple{"__header__"}),
				packStatistic(RecordTypeStatistic{
					Count:                99,
					CollectedAtVersion:   hdr.CollectedAtVersion,
					CollectedAtUnixNanos: hdr.CollectedAtUnixNanos,
				}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		stored, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(),
			"a record type named __header__ destroyed the header — the two must not "+
				"share a key, or completeness can never pass for such a schema")
		Expect(stored.CollectedAtVersion).To(BeNumerically(">", 1),
			"the header carries the run's own version, not the type entry's")
		Expect(stored.PerType).To(HaveKey("__header__"))
		Expect(stored.PerType["__header__"].Count).To(Equal(int64(99)))
		Expect(stored.PerType).To(HaveKey("Order"))
	})

	// THE TIME LIMIT IS THE OTHER BOUND, AND IT RUNS ON THE SIMULATION CLOCK.
	//
	// Every elapsed-time decision goes through ScanLimiterState.Elapsed, which is
	// env.Since — so a seeded environment makes the TimeLimit arm as drivable as
	// the byte arm, and there is no excuse for leaving it untested. (There WAS
	// one written down: a comment on ScannedBytesLimit claimed it was
	// "deterministic where TimeLimit is not". That was false, and it is exactly
	// the shape where a wrong sentence keeps an arm untested indefinitely.)
	//
	// The clock steps on every read, so the limit trips a few checks into each
	// batch. What must survive is the total: a time-stopped cursor has to resume
	// exactly where it stopped, or the count silently undercounts.
	It("counts exactly when the time limit stops batches mid-scan", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		const orders, customers = 500, 20
		seed(ctx, sub, orders, customers)

		counter := &countingTransactor{inner: sharedDB.transactor}
		steppingDB := NewFDBDatabaseWithTransactor(counter, sharedDB.db).
			SetEnv(&dst.Env{Clock: &steppingClock{now: dst.Epoch, step: time.Second}})

		report, err := CollectStatistics(ctx, steppingDB,
			func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
				return NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(metaData).SetSubspace(sub).CreateOrOpen()
			}, stats, CollectOptions{
				// Row and byte budgets far beyond this store, so ONLY the clock
				// can end a batch. Without this the test would re-prove the byte
				// arm under a new name.
				BatchSize:         1_000_000,
				ScannedBytesLimit: 1 << 40,
				TimeLimit:         3 * time.Second,
			})
		Expect(err).NotTo(HaveOccurred())

		// THE VACUITY GUARD. One scan plus one write means the clock never
		// stopped a batch and this is the plain exactness test again.
		Expect(counter.count()).To(BeNumerically(">", 2),
			"collection took %d transactions, so the time limit never stopped a batch "+
				"— nothing here exercises resuming from a time-stopped cursor",
			counter.count())
		// An upper bound on transactions, catching a batch that returns only a
		// HANDFUL of records before stopping. It does NOT catch zero progress:
		// a batch that stopped before returning any record would loop forever and
		// never reach this line, so nothing here would fail -- the test would
		// hang. What actually prevents that is the free-initial-pass gate in the
		// leaf cursor (a limit cannot trip before the first record), and it is out
		// of this spec's reach to assert.
		Expect(counter.count()).To(BeNumerically("<", orders+customers+50),
			"collection took %d transactions for %d records — batches are stopping "+
				"after only a few records, so the scan is barely progressing",
			counter.count(), orders+customers)

		Expect(report.Collected["Order"].Count).To(Equal(int64(orders)),
			"a time-limited batch lost or repeated rows — the cursor stopped on the "+
				"clock and the continuation did not resume exactly where it stopped")
		Expect(report.Collected["Customer"].Count).To(Equal(int64(customers)))
		Expect(report.RecordsScanned).To(Equal(int64(orders + customers)))
	})

	// AN ENTRY COUNT THAT DISAGREES WITH THE HEADER IS A TORN SET.
	//
	// The header records how many per-type entries the write put down. Header
	// and entries go down in ONE transaction (ClearRange, header, then each
	// entry), so on any consistent read they agree; a disagreement means the
	// read assembled a DIFFERENT SET than was written — a foreign writer, a
	// partial clear, or corruption.
	//
	// That is the same fact a malformed entry carries, and it gets the same
	// answer: nothing, rather than a partial set wearing a complete one's shape.
	// This is also what makes the header's Count field load-bearing instead of
	// decorative — it was previously written to durable bytes and never read,
	// which is the shape that lets a value drift wrong with nothing noticing.
	It("refuses a set whose entry count disagrees with its header", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 6, 2)
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		// Control: readable before the set is torn, or the refusal below is not
		// about tearing.
		before, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(),
			"the set was already unreadable, so the refusal below proves nothing")

		// Add ONE entry without telling the header. This is exactly the state a
		// foreign writer produces, and the one the old header-collision test
		// built by accident.
		// STAMPED WITH THE HEADER'S OWN VERSION AND NANOS, so the stamp check
		// cannot refuse it and only the COUNT check can. Without this the entry
		// carried version 1 and nanos 0 against a header holding the run's real
		// stamps, so the stamp check -- added AFTER this spec -- refused it first.
		// The spec kept passing for a reason it was not written for, and the count
		// check it exists to pin was dead from that commit onward: neutering the
		// count check left the whole suite green.
		//
		// A later gate can silently supersede an earlier gate's test. The original
		// mutation for the count check was honest when it was run; nothing re-ran
		// it after the stamp check landed. Three specs now assert ok == false on
		// this fixture and ok == false has three causes, so the only thing that
		// settles which spec pins which check is the mutation MATRIX: one deletion
		// per check, each reddening exactly its own spec.
		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(target.Pack(tuple.Tuple{"SmuggledType"}),
				packStatistic(RecordTypeStatistic{
					Count:                7,
					CollectedAtVersion:   before.CollectedAtVersion,
					CollectedAtUnixNanos: before.CollectedAtUnixNanos,
				}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		// Asserting the REASON, not ok==false. Seven arms render as ok==false, so
		// that assertion cannot say WHICH one fired -- and this very check was
		// found dead because a later arm refused this fixture first while the
		// spec kept passing.
		expectReadRefusal(refusal, StatisticsReadCountMismatch,
			"a set carrying one more entry than its header recorded must be refused "+
				"for THAT reason — the reader vouched for a set it did not assemble "+
				"from a single consistent write, or refused it for another reason, "+
				"which leaves the count check unpinned")
	})

	// THE WRITER OWNS BOTH STAMPS, NOT ITS CALLERS.
	//
	// The reader requires every entry's version and nanos to equal the header's.
	// writeStatistics used to set only nanos and take the VERSION on trust from
	// the report — which held solely because its single caller pre-stamped every
	// entry with the same value.
	//
	// That is a trap for the next caller, and an invisible one: a report built
	// without pre-stamping produces a set the reader rejects, surfacing to an
	// operator as "not collected" IMMEDIATELY AFTER a collection that reported
	// success. CollectStatistics cannot expose it, because it pre-stamps — so
	// this drives writeStatistics directly with entries whose version is ZERO,
	// which is exactly the report shape a caller who did not know about the
	// invariant would build.
	It("stamps entries itself rather than trusting the report's version", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		const version = int64(4242)

		// Entries deliberately UNSTAMPED. Reaching in at this level is the point:
		// it is the only way to build the report a future caller would.
		report := &CollectionReport{
			Collected: map[string]RecordTypeStatistic{
				"Order":    {Count: 7},
				"Customer": {Count: 3},
			},
			Skipped: map[string]string{},
		}
		_, err := writeStatistics(ctx, sharedDB, stats, sub, report, version, nil)
		Expect(err).NotTo(HaveOccurred())

		stored, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(),
			"the reader refused a set this library had just written — the writer left "+
				"entry stamps to its caller, so a caller that does not pre-stamp writes "+
				"a set rejected as torn, and an operator sees NOT COLLECTED right after "+
				"a successful collection")
		Expect(stored.CollectedAtVersion).To(Equal(version))
		for name, st := range stored.PerType {
			Expect(st.CollectedAtVersion).To(Equal(stored.CollectedAtVersion),
				"entry %q carries a different version than the header", name)
			Expect(st.CollectedAtUnixNanos).To(Equal(stored.CollectedAtUnixNanos),
				"entry %q carries a different timestamp than the header", name)
		}
		Expect(stored.PerType["Order"].Count).To(Equal(int64(7)))
	})

	// AN ENTRY FROM A DIFFERENT COLLECTION RUN POISONS THE SET.
	//
	// The header carries the run's version and time, and the freshness gate
	// reads ONLY the header — so an entry replaced in place by a foreign or
	// manual writer is judged by a stamp that does not belong to it. The count
	// check above cannot see it either: replacing an entry leaves the number of
	// keys unchanged.
	//
	// That is the all-or-nothing contract broken in the one direction every gate
	// above is blind to, because every one of them is looking at the header. So
	// the entries are checked against it: writeStatistics stamps the header and
	// every entry identically in ONE transaction, so any disagreement means at
	// least one entry came from a different run.
	It("refuses a set whose entry stamps disagree with the header", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 6, 2)
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		// Control: readable before the stamp is altered, or the refusal below is
		// not about mixed runs.
		before, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(),
			"the set was already unusable, so the refusal below proves nothing")
		Expect(before.PerType).To(HaveKey("Order"))

		// Replace ONE entry in place, keeping the key count identical, with a
		// count from an older run. This is exactly what a foreign writer does.
		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			stale := before.PerType["Order"]
			stale.Count = 999
			stale.CollectedAtVersion = before.CollectedAtVersion - 1
			rtx.Transaction().Set(target.Pack(tuple.Tuple{"Order"}), packStatistic(stale))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadVersionMismatch,
			"an entry stamped from a different run must be refused for the VERSION "+
				"reason specifically — the freshness gate reads only the header, so it "+
				"would otherwise judge that stale count fresh")
	})

	// THE OTHER HALF OF THE STAMP CHECK.
	//
	// The check has two arms — version and timestamp — and the spec above moves
	// only the VERSION. Deleting the timestamp arm therefore left the whole
	// suite green, which is a brand-new two-arm gate shipping with one arm never
	// driven. It is also the arm that matters for the likeliest hand-edit: a
	// writer that preserves the version it read and stamps its own time.
	//
	// A single spec cannot cover both, because either arm alone satisfies an OR.
	// So they are separate, and each moves exactly one field.
	It("refuses a set whose entry TIMESTAMP disagrees with the header", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 6, 2)
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		before, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(),
			"the set was already unusable, so the refusal below proves nothing")

		// VERSION PRESERVED, timestamp moved — the mirror of the spec above.
		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			edited := before.PerType["Order"]
			edited.CollectedAtUnixNanos = before.CollectedAtUnixNanos + 1
			rtx.Transaction().Set(target.Pack(tuple.Tuple{"Order"}), packStatistic(edited))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadTimestampMismatch,
			"an entry whose TIMESTAMP differs from the header must be refused for "+
				"THAT reason — the version arm alone was carrying this check once "+
				"already, so half of it went unexercised")
	})

	// THE TWO KEY ARMS THAT NOTHING DROVE.
	//
	// ReadStatisticsAtWithRefusal declines eight ways. Extending the mutation
	// matrix over all of them — one arm neutered at a time, each required to
	// redden exactly its own spec — found these two reddening NOTHING: an entry
	// key that does not unpack to a single element, and a single element that is
	// neither the header integer nor a record-type string.
	//
	// Both are corruption or a newer writer's layout, and both used to fail OPEN
	// in an earlier revision of this reader, which is the direction that hands a
	// partial set to a caller with no completeness gate. An arm nothing drives
	// fires for the first time in front of an operator.
	It("refuses an entry key that does not unpack to one element", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 4, 2)
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		_, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadOK,
			"the set was already refused, so the arm below is not what this pins")

		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			// TWO elements: unpacks cleanly, but no entry has this shape.
			rtx.Transaction().Set(target.Pack(tuple.Tuple{"Order", int64(2)}),
				packStatistic(RecordTypeStatistic{Count: 1}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, refusal, _, err = ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadUndecodableKey,
			"a key this build cannot parse must refuse the SET, and for that reason — "+
				"skipping it returns the rest as complete, which is the partial answer "+
				"the all-or-nothing contract exists to forbid")
	})

	It("refuses an entry key that is neither the header nor a record type", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 4, 2)
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			// A FLOAT element: one element, unpacks fine, and neither the header
			// integer nor a record-type name.
			rtx.Transaction().Set(target.Pack(tuple.Tuple{1.5}),
				packStatistic(RecordTypeStatistic{Count: 1}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadNonStringKey,
			"a key element that is neither the header integer nor a record-type "+
				"string must refuse the SET, and for that reason")
	})

	// THE COLLECTOR OWNS THE SYNTHETIC REFUSAL, NOT ITS CALLERS.
	//
	// RecordTypes() deliberately OMITS joined and unnested declarations — this
	// port carries them opaquely — so for such metadata the declared-type set is
	// a PARTIAL view of the schema. A run over it writes a header vouching for a
	// set that can never be complete, and the record-layer reader has no
	// completeness gate to catch that; only the relational one does.
	//
	// Both in-repo callers already refuse before opening a store. That is not
	// the same as the invariant being enforced: a direct record-layer caller
	// reaches this function with no gate in front of it at all, which is exactly
	// the population this package is for. Same reasoning as the entry stamps —
	// the writer owns what the reader requires.
	It("refuses metadata declaring synthetic record types, before scanning", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 6, 2)

		// Round-trip the suite's own metadata and add a JOINED declaration, so
		// the only difference from a passing run is the synthetic type.
		p, err := metaData.ToProto()
		Expect(err).NotTo(HaveOccurred())
		p.JoinedRecordTypes = append(p.JoinedRecordTypes, &gen.JoinedRecordType{
			Name: proto.String("OrderWithCustomer"),
			JoinConstituents: []*gen.JoinedRecordType_JoinConstituent{
				{Name: proto.String("o"), RecordType: proto.String("Order")},
				{Name: proto.String("c"), RecordType: proto.String("Customer")},
			},
		})
		syntheticMD, err := RecordMetaDataFromProto(p)
		Expect(err).NotTo(HaveOccurred())
		Expect(syntheticMD.DeclaresSyntheticRecordTypes()).To(BeTrue(),
			"the fixture does not actually declare a synthetic type, so the refusal "+
				"below would prove nothing")

		_, err = CollectStatistics(ctx, sharedDB,
			func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
				return NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(syntheticMD).SetSubspace(sub).CreateOrOpen()
			}, stats, CollectOptions{BatchSize: 5})

		var syntheticErr *SyntheticRecordTypesNotModeledError
		Expect(errors.As(err, &syntheticErr)).To(BeTrue(),
			"collection over metadata declaring synthetic types must be REFUSED by the "+
				"collector itself: got %v", err)
		Expect(syntheticErr.TypeNames).To(ContainElement("OrderWithCustomer"),
			"the refusal must name what it refused for")

		// And nothing was persisted — a refusal that still wrote a header would
		// leave behind the partial set it exists to prevent.
		_, refusal, _, rErr := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(rErr).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadAbsent,
			"a refused collection still wrote something")
	})

	// ENTRIES WITHOUT A HEADER ARE TORN, NOT ABSENT.
	//
	// The read loop populates PerType regardless of whether the header turned
	// up, so a range whose header alone was deleted arrives with a non-empty map
	// and found=false. That used to return the same refusal as an EMPTY range,
	// which the relational gate maps to "not collected" — so `stats show` told an
	// operator nothing was stored while the store held orphaned per-type entries.
	//
	// Same absent-versus-stored conflation this reader spent several commits
	// removing, at the one arm that had not been split yet. The header is what
	// makes a set vouchable; entries without one are torn.
	It("refuses entries whose header is missing as torn, not as absent", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 6, 2)
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		before, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadOK,
			"the set was already unusable, so the refusal below proves nothing")
		Expect(before.PerType).NotTo(BeEmpty(),
			"no per-type entries to orphan, so deleting the header would leave an "+
				"EMPTY range and this spec would pin the absent case instead")

		// Delete ONLY the header. The per-type entries survive.
		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Clear(fdb.Key(target.Pack(statisticsHeaderKey)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, refusal, _, err = ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadHeaderMissing,
			"orphaned entries with no header were reported as ABSENT — an operator is "+
				"then told the store is empty while it holds entries a collect would clear")
	})

	// The other side, or the arm above is satisfiable by returning HeaderMissing
	// for everything: a genuinely EMPTY range must still be absent.
	It("refuses an empty range as absent", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()

		_, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		expectReadRefusal(refusal, StatisticsReadAbsent,
			"an empty range must be ABSENT, not torn — reporting nothing-stored as a "+
				"torn set sends an operator looking for damage that is not there")
	})

	// A MALFORMED ENTRY POISONS THE WHOLE SET.
	//
	// ReadStatistics documents all-or-nothing: "a caller gets a usable set or
	// nothing". Skipping one unreadable entry while the header stays valid broke
	// that — it returned ok=true with a map missing a type, which is precisely
	// the partial set the completeness gate exists to reject, handed to it
	// pre-broken. A caller below the relational layer has no gate at all.
	It("refuses the whole set when one entry cannot be decoded", func() {
		ctx := context.Background()
		sub := specSubspace()
		stats := statsRoot()
		seed(ctx, sub, 8, 3)
		_, err := CollectStatistics(ctx, sharedDB, builderFor(sub), stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		// Control: readable before corruption, or the refusal below proves nothing.
		_, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(target.Pack(tuple.Tuple{"Order"}), []byte("not a packed tuple"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		got, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		// THE DECODE ARM SPECIFICALLY. Asserting ok==false left this check dead:
		// a garbage value decodes to a ZERO-VALUED statistic, whose version and
		// nanos are 0, so the stamp check refused it first and this spec passed
		// for that reason instead. No fixture can fix that -- an undecodable entry
		// has no stamps BY CONSTRUCTION, so the decode arm can never be the only
		// arm able to refuse. Naming the reason is the only thing that can.
		expectReadRefusal(refusal, StatisticsReadUndecodableValue,
			"one undecodable entry must reject the SET for the DECODE reason. "+
				"Returning the rest with ok=true is a partial answer wearing a "+
				"complete one's shape; refusing for a different reason leaves the "+
				"decode arm unpinned, which is how it was found dead")
		Expect(got.PerType).To(BeEmpty())
	})
})

// replayingTransactor invokes each transaction closure twice and returns the
// second result — an FDB retry, minus the timing. Used to prove the collector's
// per-batch accumulators are reset per attempt rather than merged per attempt.
type replayingTransactor struct {
	inner   fdb.Transactor
	replays int
	spent   int
	// beforeReplay runs between a discarded attempt and the real one, so a test
	// can mutate the store mid-retry — the state a retry actually races.
	beforeReplay func()
}

func (r *replayingTransactor) used() int { return r.spent }

// Transact keeps the SAME-transaction replay, deliberately, and differently from
// ReadTransact above. The write-side defect is about accumulator state surviving
// a re-execution, which one transaction reproduces exactly; running two separate
// inner transactions would additionally COMMIT the discarded attempt, which is
// not what a retry does and would muddy what the test is measuring.
//
// ITS BLIND SPOT, stated because an instrument's limits are the part nobody
// discovers until it matters: both attempts share one read version, so anything
// the write path derives FROM the data — collectedAtVersion is the live example
// — reads identically on each pass and a staleness bug in it would be invisible
// here. That is the same gap ReadTransact's separate-transaction replay exists
// to close. There is no such bug today (collectedAtVersion is an unconditional
// overwrite), which is why this is a note about the instrument rather than a
// second test.
func (r *replayingTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return r.inner.Transact(func(tx fdb.WritableTransaction) (any, error) {
		if r.replays > 0 {
			r.replays--
			r.spent++
			// Discard the first run exactly as a retry discards an aborted
			// attempt: its reads happened, and nothing durable may survive them.
			if _, err := fn(tx); err != nil {
				return nil, err
			}
		}
		return fn(tx)
	})
}

// ReadTransact replays too. A bare pass-through here would leave the READ path
// unexercised while the test around it claims to cover every transaction — and
// the read path is where the same per-attempt-reset bug was found the second
// time, so it is the half most in need of the instrument.
//
// beforeReplay, when set, runs between the discarded attempt and the real one.
// That is what turns a replay into the actual reproducer: a retry racing a
// concurrent ClearStatistics is the case where unreset state yields the previous
// attempt's entries stamped with the new attempt's version.
func (r *replayingTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	if r.replays > 0 {
		r.replays--
		r.spent++
		// A SEPARATE inner transaction for the discarded attempt, not a second
		// call on the same one. That distinction is the whole test: FDB pins a
		// read version per transaction, so two invocations on one rtx observe
		// the identical snapshot and a store mutated in between is invisible.
		// A real retry gets a fresh transaction at a NEW read version, which is
		// what lets beforeReplay's clear be seen — and what makes unreset state
		// produce the stale-entries-with-fresh-stamp result this pins.
		if _, err := r.inner.ReadTransact(fn); err != nil {
			return nil, err
		}
		if r.beforeReplay != nil {
			r.beforeReplay()
		}
	}
	return r.inner.ReadTransact(fn)
}

// errInjectedGRV is the fault a grvFailingTransactor surfaces from a read-version
// request. A distinct sentinel so a test can assert the error it INJECTED came
// back, rather than that some error did — the difference between pinning the
// propagation and pinning that the operation can fail at all.
var errInjectedGRV = errors.New("injected: GetReadVersion failed")

// grvFailingTransactor fails every GetReadVersion and passes everything else
// through. The chaos package has FaultReadError for key reads but nothing for a
// read VERSION, and a version failure is its own case: it is the one read whose
// result gets persisted.
//
// SCOPE, stated because embedding the interface looks like it covers everything
// and does not:
//
//   - It fails only the EXPLICIT GetReadVersion call and leaves the underlying
//     reads working. A real cluster failure that killed the version would redden
//     the key reads too, so this is not a cluster simulation — it is an isolator
//     for the two call sites, which is what makes a failure here name one of
//     them instead of pointing at the whole transaction.
//   - Both live routes to a version ARE covered: rtx.ReadTransaction(true) goes
//     through Snapshot(), rtx.ReadTransaction(false) through the writable
//     transaction itself.
//   - NOT covered: fdb.ReadTransaction embeds ReadTransactor, so a Transact or
//     ReadTransact called THROUGH one of these wrappers hands back an unwrapped
//     transaction, and any view accessor added to the interface later escapes
//     the same way. Embedding stops the wrapper failing to COMPILE as the
//     interface grows; it does not stop a new route around it.
type grvFailingTransactor struct {
	inner  fdb.Transactor
	failed int
}

func (g *grvFailingTransactor) hits() int { return g.failed }

func (g *grvFailingTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return g.inner.Transact(func(tx fdb.WritableTransaction) (any, error) {
		return fn(&grvFailingWritable{WritableTransaction: tx, owner: g})
	})
}

func (g *grvFailingTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return g.inner.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return fn(&grvFailingRead{ReadTransaction: rtx, owner: g})
	})
}

// grvFailingWritable embeds the real transaction so the wrapper stays a statement
// about read versions and cannot drift as fdb.WritableTransaction grows.
type grvFailingWritable struct {
	fdb.WritableTransaction
	owner *grvFailingTransactor
}

func (w *grvFailingWritable) GetReadVersion() fdb.FutureInt64 {
	w.owner.failed++
	return failedInt64{}
}

// Snapshot is the route the collector actually takes: it reads the version
// through rtx.ReadTransaction(true), which is the snapshot view.
func (w *grvFailingWritable) Snapshot() fdb.ReadTransaction {
	return &grvFailingRead{ReadTransaction: w.WritableTransaction.Snapshot(), owner: w.owner}
}

type grvFailingRead struct {
	fdb.ReadTransaction
	owner *grvFailingTransactor
}

func (r *grvFailingRead) GetReadVersion() fdb.FutureInt64 {
	r.owner.failed++
	return failedInt64{}
}

func (r *grvFailingRead) Snapshot() fdb.ReadTransaction { return r }

// failedInt64 is an already-ready FutureInt64 carrying the injected error.
type failedInt64 struct{}

func (failedInt64) Get() (int64, error) { return 0, errInjectedGRV }
func (failedInt64) MustGet() int64      { panic(errInjectedGRV) }
func (failedInt64) BlockUntilReady()    {}
func (failedInt64) IsReady() bool       { return true }
func (failedInt64) Cancel()             {}

var _ = Describe("CollectStatisticsIntegerKeys", func() {
	// AN INTEGER KEY THAT IS NOT THE HEADER IS CORRUPTION.
	//
	// Record type names are strings, so an integer tuple key can only be the
	// header — or something this build does not understand: corruption, or a
	// newer writer's layout. Skipping it and returning the rest is the same
	// partial answer a malformed value gives, against the same all-or-nothing
	// contract, and it was the one remaining spelling of that bug after the
	// malformed-value and malformed-key cases were closed.
	It("refuses a set containing an unknown integer key", func() {
		ctx := context.Background()
		sub := specSubspace()
		// Sibling of the store subspace, never a child — the store keyspace is
		// Java's (constants.go).
		stats := NewStatisticsSubspace(subspace.FromBytes(
			tuple.Tuple{"__stats__", CurrentSpecReport().FullText()}.Pack()))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		build := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := build(rtx)
			if sErr != nil {
				return nil, sErr
			}
			_, sErr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
			return nil, sErr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = CollectStatistics(ctx, sharedDB, build, stats, CollectOptions{BatchSize: 5})
		Expect(err).NotTo(HaveOccurred())

		// Control: readable before the injection, or the refusal proves nothing.
		_, ok, err := ReadStatistics(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		// A second integer key — what a newer writer's header revision, or
		// corruption, would look like.
		target := stats.forStore(sub)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(target.Pack(tuple.Tuple{int64(1)}),
				packStatistic(RecordTypeStatistic{Count: 5, CollectedAtVersion: 1}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		got, refusal, _, err := ReadStatisticsAtWithRefusal(ctx, sharedDB, stats, sub)
		Expect(err).NotTo(HaveOccurred())
		// THE REASON, not ok==false. Asserting the bool left this arm DEAD: an
		// integer key is not a string either, so the adjacent
		// neither-header-nor-record-type arm absorbs it and refuses the set anyway.
		// The spec kept passing off its neighbour while the arm it names did
		// nothing — the fourth check in this PR found dead the same way, and the
		// reason it survived a mutation matrix is that a blunt mutation (breaking
		// out of the loop) reddens it for a cause the assertion cannot express.
		expectReadRefusal(refusal, StatisticsReadUnknownIntegerKey,
			"an integer key that is not the header must poison the SET, and for THAT "+
				"reason. It cannot be a record type — names are strings — so it is a "+
				"layout this build does not understand; refusing it as a non-string key "+
				"instead means this arm is doing nothing")
		Expect(got.PerType).To(BeEmpty())
	})
})

// countingTransactor counts how many transactions a call opens, passing every
// one straight through. Used as a vacuity guard: a test that claims a scan
// limit forced the collector to resume across transactions has to show that
// more than one scan transaction actually happened.
type countingTransactor struct {
	inner fdb.Transactor
	n     int
}

func (c *countingTransactor) count() int { return c.n }

func (c *countingTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	c.n++
	return c.inner.Transact(fn)
}

func (c *countingTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	c.n++
	return c.inner.ReadTransact(fn)
}

// EVERY DECLARED READ-REFUSAL MUST BE PRODUCED BY A SPEC.
//
// The mutation matrix over ReadStatisticsAtWithRefusal's arms was run by hand
// and each arm reddened its own spec. Nothing keeps that true. A ninth arm can
// land reusing an existing reason, or with no spec at all, and the suite stays
// green — which is the same "a claim that cannot fail" shape the matrix exists
// to remove, one level up.
//
// So the coverage is enforced rather than remembered. This is the record-layer
// mirror of TestDecideStatisticsCoversEveryRefusal in core/embedded, and it has
// the same acknowledged hole: the list below is hand-maintained, so a constant
// absent from BOTH it and every spec cannot be caught — Go cannot enumerate
// constants at runtime. Adding one to the const block means adding it here.
var allStatisticsReadRefusals = []StatisticsReadRefusal{
	StatisticsReadOK,
	StatisticsReadAbsent,
	StatisticsReadUndecodableKey,
	StatisticsReadUnknownIntegerKey,
	StatisticsReadNonStringKey,
	StatisticsReadUndecodableValue,
	StatisticsReadCountMismatch,
	StatisticsReadVersionMismatch,
	StatisticsReadTimestampMismatch,
	StatisticsReadHeaderMissing,
}

var (
	observedReadRefusalsMu sync.Mutex
	observedReadRefusals   = map[StatisticsReadRefusal]bool{}
)

// expectReadRefusal asserts the refusal AND records that it was produced, so the
// suite-level guard below can tell a reason nothing exercises from one that is
// merely rare. Recording what was OBSERVED rather than what was WANTED is
// deliberate: a spec asserting the wrong reason fails anyway, and recording the
// wanted value would let a never-produced reason count itself as covered.
func expectReadRefusal(got, want StatisticsReadRefusal, description string, args ...any) {
	observedReadRefusalsMu.Lock()
	observedReadRefusals[got] = true
	observedReadRefusalsMu.Unlock()
	msg := description
	if len(args) > 0 {
		msg = fmt.Sprintf(description, args...)
	}
	ExpectWithOffset(1, got).To(Equal(want), msg)
}

var _ = ReportAfterSuite("every statistics-read refusal is produced by a spec", func(report Report) {
	// A FILTERED run reaches only the specs the filter selected, so an
	// unobserved reason says nothing about the suite. Reporting a pass there
	// would be the vacuous-green this guard exists to prevent; reporting a
	// failure would make every focused run red. It declines instead, loudly.
	if len(report.SuiteConfig.FocusStrings) > 0 || report.SuiteConfig.LabelFilter != "" ||
		len(report.SuiteConfig.SkipStrings) > 0 {
		GinkgoWriter.Printf("statistics-read refusal coverage NOT checked: the run was " +
			"filtered, so an unobserved reason would only describe the filter.\n")
		return
	}
	observedReadRefusalsMu.Lock()
	defer observedReadRefusalsMu.Unlock()
	var missing []string
	for _, r := range allStatisticsReadRefusals {
		if !observedReadRefusals[r] {
			missing = append(missing, string(r))
		}
	}
	// Both directions. A reason produced but not listed means the list has gone
	// stale against the const block, which is how the hole above widens.
	for r := range observedReadRefusals {
		found := false
		for _, known := range allStatisticsReadRefusals {
			if r == known {
				found = true
				break
			}
		}
		if !found {
			Fail(fmt.Sprintf("a spec produced refusal %q, which allStatisticsReadRefusals "+
				"does not name — the list is stale against the const block", r))
		}
	}
	if len(missing) > 0 {
		Fail(fmt.Sprintf("%d statistics-read refusal(s) are produced by NO spec: %s.\n"+
			"An arm nothing drives fires for the first time in front of an operator, and "+
			"four checks in this feature were already found dead exactly that way.",
			len(missing), strings.Join(missing, ", ")))
	}
})
