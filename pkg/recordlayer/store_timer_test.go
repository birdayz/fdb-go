package recordlayer

import (
	"context"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

var _ = Describe("StoreTimer", func() {
	Describe("Counter", func() {
		It("starts at zero", func() {
			c := &Counter{}
			Expect(c.Count()).To(Equal(int64(0)))
			Expect(c.CumulativeValue()).To(Equal(int64(0)))
		})

		It("records a single observation", func() {
			c := &Counter{}
			c.Record(500)
			Expect(c.Count()).To(Equal(int64(1)))
			Expect(c.CumulativeValue()).To(Equal(int64(500)))
		})

		It("accumulates multiple observations", func() {
			c := &Counter{}
			c.Record(100)
			c.Record(200)
			c.Record(300)
			Expect(c.Count()).To(Equal(int64(3)))
			Expect(c.CumulativeValue()).To(Equal(int64(600)))
		})

		// Java's Counter.increment adds to `count` alone and never touches
		// cumulativeValue (StoreTimer.java:484-487) — which is what lets Java use
		// getTimeNanos()==0 as proof that an event is a counter and not a timer
		// (RecordLayerMetricCollector.java:90). Go used to add the amount to BOTH,
		// so every count event reported a "duration" equal to its own count.
		It("increments only the count, leaving the cumulative value at zero", func() {
			c := &Counter{}
			c.Increment(5)
			Expect(c.Count()).To(Equal(int64(5)))
			Expect(c.CumulativeValue()).To(Equal(int64(0)))
		})

		It("resets to zero", func() {
			c := &Counter{}
			c.Record(999)
			c.Increment(3)
			c.Reset()
			Expect(c.Count()).To(Equal(int64(0)))
			Expect(c.CumulativeValue()).To(Equal(int64(0)))
		})
	})

	Describe("Timer operations", func() {
		It("records a timed event", func() {
			t := NewStoreTimer()
			t.Record(EventSaveRecord, 12345)
			Expect(t.GetCount(EventSaveRecord)).To(Equal(int64(1)))
			Expect(t.GetTimeNanos(EventSaveRecord)).To(Equal(int64(12345)))
		})

		It("records since a start time", func() {
			t := NewStoreTimer()
			start := time.Now().Add(-10 * time.Millisecond)
			t.RecordSince(EventLoadRecord, start)
			Expect(t.GetCount(EventLoadRecord)).To(Equal(int64(1)))
			Expect(t.GetTimeNanos(EventLoadRecord)).To(BeNumerically(">=", int64(10*time.Millisecond)))
		})

		It("increments a counter", func() {
			t := NewStoreTimer()
			t.Increment(CountReads)
			t.Increment(CountReads)
			t.Increment(CountReads)
			Expect(t.GetCount(CountReads)).To(Equal(int64(3)))
		})

		It("increments by amount", func() {
			t := NewStoreTimer()
			t.IncrementBy(CountBytesWritten, 1024)
			t.IncrementBy(CountBytesWritten, 2048)
			// A size Count carries its byte total in the COUNT field — Java's
			// getKeysAndValues emits a size Count as a plain `_count` key and never
			// a `_micros` one (StoreTimer.java:753-758). GetTimeNanos on a count
			// event must therefore read 0; it previously echoed the byte total,
			// which would have exported 3072 bytes as a 3.072-microsecond duration.
			Expect(t.GetCount(CountBytesWritten)).To(Equal(int64(3072)))
			Expect(t.GetTimeNanos(CountBytesWritten)).To(Equal(int64(0)))
		})

		It("returns nil counter for unrecorded event", func() {
			t := NewStoreTimer()
			Expect(t.GetCounter(EventDeleteRecord)).To(BeNil())
		})

		It("returns zero count for unrecorded event", func() {
			t := NewStoreTimer()
			Expect(t.GetCount(EventDeleteRecord)).To(Equal(int64(0)))
			Expect(t.GetTimeNanos(EventDeleteRecord)).To(Equal(int64(0)))
		})

		It("returns a counter for a recorded event", func() {
			t := NewStoreTimer()
			t.Record(EventCommit, 999)
			c := t.GetCounter(EventCommit)
			Expect(c).NotTo(BeNil())
			Expect(c.Count()).To(Equal(int64(1)))
			Expect(c.CumulativeValue()).To(Equal(int64(999)))
		})

		It("carries the classified event in the snapshot", func() {
			t := NewStoreTimer()
			t.Record(EventSaveRecord, 100)
			t.Increment(CountSaveRecordKey)
			t.IncrementBy(CountSaveRecordKeyBytes, 32)

			snap := t.Snapshot()
			// Without the Kind a consumer cannot tell 32 bytes from a
			// 32-nanosecond duration — the numbers alone are not self-describing.
			Expect(snap["save_record"].Event.Kind).To(Equal(KindTimed))
			Expect(snap["save_record_key"].Event.Kind).To(Equal(KindCount))
			Expect(snap["save_record_key_bytes"].Event.Kind).To(Equal(KindSize))
			Expect(snap["save_record"].Event.Title).To(Equal("Save Record"))
		})

		It("folds another timer with Add, matching Java's StoreTimer.add", func() {
			dst := NewStoreTimer()
			dst.Record(EventSaveRecord, 100)
			dst.Increment(CountSaveRecordKey)

			src := NewStoreTimer()
			src.Record(EventSaveRecord, 250)
			src.Increment(CountSaveRecordKey)
			src.IncrementBy(CountSaveRecordKeyBytes, 64)

			dst.Add(src)

			// Both fields of every counter add — count AND cumulative value.
			Expect(dst.GetCount(EventSaveRecord)).To(Equal(int64(2)))
			Expect(dst.GetTimeNanos(EventSaveRecord)).To(Equal(int64(350)))
			Expect(dst.GetCount(CountSaveRecordKey)).To(Equal(int64(2)))
			// An event present only in the source is created in the destination,
			// classified — a fold that dropped the Kind would export bytes as a
			// duration.
			Expect(dst.GetCount(CountSaveRecordKeyBytes)).To(Equal(int64(64)))
			Expect(dst.Snapshot()["save_record_key_bytes"].Event.Kind).To(Equal(KindSize))
			// The source is untouched: Java's add() only reads `other`.
			Expect(src.GetCount(EventSaveRecord)).To(Equal(int64(1)))
		})

		It("ignores nil and self in Add", func() {
			t := NewStoreTimer()
			t.Record(EventSaveRecord, 100)
			Expect(func() { t.Add(nil) }).NotTo(Panic())
			// Self-add would double every counter, and Range-while-mutating the
			// same sync.Map is not a shape worth having in the first place.
			t.Add(t)
			Expect(t.GetCount(EventSaveRecord)).To(Equal(int64(1)))
			var nilTimer *StoreTimer
			Expect(func() { nilTimer.Add(t) }).NotTo(Panic())
		})

		It("resolves a runner's timer from the config first, then the database", func() {
			// No FDB needed: contextTimer is a pure read of the two fields, and the
			// precedence between them is the whole behaviour. A runner that ignored
			// the database's timer would leave every retry uncounted by a
			// process-wide exporter; one that ignored its own config's timer would
			// silently merge an isolated measurement back into the process total.
			dbTimer := NewStoreTimer()
			db := &FDBDatabase{}
			db.SetTimer(dbTimer)
			runner := NewFDBDatabaseRunner(db)

			// No config: inherit the database's.
			Expect(runner.contextTimer()).To(BeIdenticalTo(dbTimer))

			// Config present but its timer unset: still inherit. Nil means
			// "inherit", not "uninstrumented" — Java's runner and its context
			// config cannot disagree because they are the same field.
			runner.SetContextConfig(&RecordContextConfig{})
			Expect(runner.contextTimer()).To(BeIdenticalTo(dbTimer))

			// Config timer set: the more specific statement of intent wins.
			ownTimer := NewStoreTimer()
			runner.SetContextConfig(&RecordContextConfig{Timer: ownTimer})
			Expect(runner.contextTimer()).To(BeIdenticalTo(ownTimer))

			// An uninstrumented database resolves to nil, which every StoreTimer
			// method tolerates.
			Expect(NewFDBDatabaseRunner(&FDBDatabase{}).contextTimer()).To(BeNil())
		})

		It("returns a nil timer from a nil context rather than panicking", func() {
			// The whole instrumentation chain is nil-tolerant by design, and the
			// accessor at its head must not be the one link that panics — the same
			// contract FDBRecordContext.Env carries. Call sites read
			// store.context.Timer().RecordSince(...), so a panic here would have to
			// be pre-empted by a nil check at every one of them.
			var rc *FDBRecordContext
			Expect(func() { rc.Timer().Increment(CountReads) }).NotTo(Panic())
			Expect(rc.Timer()).To(BeNil())
		})

		It("renders Java's log keys from KeysAndValues", func() {
			t := NewStoreTimer()
			t.Record(EventCommit, 1_500_000) // 1.5ms
			t.Increment(CountSaveRecordKey)
			t.IncrementBy(CountBytesWritten, 4096)

			kv := t.KeysAndValues()
			// Java: every event emits _count; only a timed event also emits
			// _micros, as nanos/1000 with integer truncation
			// (StoreTimer.java:747-780).
			Expect(kv).To(HaveKeyWithValue("commit_count", int64(1)))
			Expect(kv).To(HaveKeyWithValue("commit_micros", int64(1500)))
			Expect(kv).To(HaveKeyWithValue("save_record_key_count", int64(1)))
			Expect(kv).NotTo(HaveKey("save_record_key_micros"))
			// A size Count carries its byte total in the _count key, exactly as
			// Java does — there is no _bytes suffix in Java's output.
			Expect(kv).To(HaveKeyWithValue("bytes_written_count", int64(4096)))
			Expect(kv).NotTo(HaveKey("bytes_written_micros"))

			var nilTimer *StoreTimer
			Expect(nilTimer.KeysAndValues()).To(BeNil())
		})

		It("produces a snapshot", func() {
			t := NewStoreTimer()
			t.Record(EventSaveRecord, 100)
			t.Record(EventSaveRecord, 200)
			t.Increment(CountWrites)

			snap := t.Snapshot()
			Expect(snap).To(HaveLen(2))
			Expect(snap["save_record"].Count).To(Equal(int64(2)))
			Expect(snap["save_record"].CumulativeValue).To(Equal(int64(300)))
			Expect(snap["writes"].Count).To(Equal(int64(1)))
		})

		It("resets all counters", func() {
			t := NewStoreTimer()
			t.Record(EventSaveRecord, 100)
			t.Increment(CountReads)
			t.Reset()
			Expect(t.GetCount(EventSaveRecord)).To(Equal(int64(0)))
			Expect(t.GetCount(CountReads)).To(Equal(int64(0)))
			snap := t.Snapshot()
			Expect(snap).To(BeEmpty())
		})
	})

	Describe("Nil safety", func() {
		It("does not panic on nil timer Record", func() {
			var t *StoreTimer
			Expect(func() { t.Record(EventSaveRecord, 100) }).NotTo(Panic())
		})

		It("does not panic on nil timer RecordSince", func() {
			var t *StoreTimer
			Expect(func() { t.RecordSince(EventSaveRecord, time.Now()) }).NotTo(Panic())
		})

		It("does not panic on nil timer Increment", func() {
			var t *StoreTimer
			Expect(func() { t.Increment(CountReads) }).NotTo(Panic())
		})

		It("does not panic on nil timer IncrementBy", func() {
			var t *StoreTimer
			Expect(func() { t.IncrementBy(CountReads, 5) }).NotTo(Panic())
		})

		It("returns nil counter on nil timer", func() {
			var t *StoreTimer
			Expect(t.GetCounter(EventSaveRecord)).To(BeNil())
		})

		It("returns zero count on nil timer", func() {
			var t *StoreTimer
			Expect(t.GetCount(EventSaveRecord)).To(Equal(int64(0)))
			Expect(t.GetTimeNanos(EventSaveRecord)).To(Equal(int64(0)))
		})

		It("does not panic on nil timer Reset", func() {
			var t *StoreTimer
			Expect(func() { t.Reset() }).NotTo(Panic())
		})

		It("returns nil snapshot on nil timer", func() {
			var t *StoreTimer
			Expect(t.Snapshot()).To(BeNil())
		})
	})

	Describe("Concurrent safety", func() {
		It("handles concurrent increments on the same counter", func() {
			t := NewStoreTimer()
			const goroutines = 100
			const iterations = 1000

			var wg sync.WaitGroup
			wg.Add(goroutines)
			for i := 0; i < goroutines; i++ {
				go func() {
					defer wg.Done()
					for j := 0; j < iterations; j++ {
						t.Increment(CountWrites)
					}
				}()
			}
			wg.Wait()

			Expect(t.GetCount(CountWrites)).To(Equal(int64(goroutines * iterations)))
		})

		It("handles concurrent records on different events", func() {
			t := NewStoreTimer()
			const iterations = 500

			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					t.Record(EventSaveRecord, 10)
				}
			}()
			go func() {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					t.Record(EventLoadRecord, 20)
				}
			}()
			go func() {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					t.Record(EventDeleteRecord, 30)
				}
			}()
			wg.Wait()

			Expect(t.GetCount(EventSaveRecord)).To(Equal(int64(iterations)))
			Expect(t.GetCount(EventLoadRecord)).To(Equal(int64(iterations)))
			Expect(t.GetCount(EventDeleteRecord)).To(Equal(int64(iterations)))
			Expect(t.GetTimeNanos(EventSaveRecord)).To(Equal(int64(iterations * 10)))
			Expect(t.GetTimeNanos(EventLoadRecord)).To(Equal(int64(iterations * 20)))
			Expect(t.GetTimeNanos(EventDeleteRecord)).To(Equal(int64(iterations * 30)))
		})
	})

	Describe("FDBRecordContext integration", func() {
		It("timer is nil by default", func() {
			ctx := context.Background()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				Expect(rtx.Timer()).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("set and get timer round-trips", func() {
			ctx := context.Background()
			timer := NewStoreTimer()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetTimer(timer)
				Expect(rtx.Timer()).To(BeIdenticalTo(timer))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("End-to-end instrumentation", func() {
		var (
			ctx context.Context
			md  *RecordMetaData
		)

		BeforeEach(func() {
			ctx = context.Background()
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			var err error
			md, err = builder.Build()
			Expect(err).NotTo(HaveOccurred())
		})

		It("records EventOpenStore on CreateOrOpen", func() {
			timer := NewStoreTimer()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetTimer(timer)
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(timer.GetCount(EventOpenStore)).To(Equal(int64(1)))
			Expect(timer.GetTimeNanos(EventOpenStore)).To(BeNumerically(">", 0))
		})

		It("records EventSaveRecord on save", func() {
			timer := NewStoreTimer()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetTimer(timer)
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(timer.GetCount(EventSaveRecord)).To(Equal(int64(1)))
			Expect(timer.GetTimeNanos(EventSaveRecord)).To(BeNumerically(">", 0))
			// Should also have key/value byte counts
			Expect(timer.GetCount(CountSaveRecordKey)).To(Equal(int64(1)))
			Expect(timer.GetCount(CountSaveRecordKeyBytes)).To(BeNumerically(">", 0))
			Expect(timer.GetCount(CountSaveRecordValueBytes)).To(BeNumerically(">", 0))
		})

		It("records EventLoadRecord on load", func() {
			timer := NewStoreTimer()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetTimer(timer)
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
				_, err = store.LoadRecord(tuple.Tuple{int64(1)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(timer.GetCount(EventLoadRecord)).To(Equal(int64(1)))
			Expect(timer.GetTimeNanos(EventLoadRecord)).To(BeNumerically(">", 0))
		})

		It("records EventDeleteRecord on delete", func() {
			timer := NewStoreTimer()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetTimer(timer)
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				Expect(deleted).To(BeTrue())
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(timer.GetCount(EventDeleteRecord)).To(Equal(int64(1)))
			Expect(timer.GetTimeNanos(EventDeleteRecord)).To(BeNumerically(">", 0))
			Expect(timer.GetCount(CountDeleteRecordKey)).To(Equal(int64(1)))
			Expect(timer.GetCount(CountDeleteRecordKeyBytes)).To(BeNumerically(">", 0))
		})

		It("does not record events when timer is nil", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// No timer set
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			// If we got here without panicking, nil timer is safe
		})

		It("accumulates across multiple operations", func() {
			timer := NewStoreTimer()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetTimer(timer)
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save 3 records
				for i := int64(1); i <= 3; i++ {
					price := int32(100)
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: &price})
					if err != nil {
						return nil, err
					}
				}

				// Load 2 records
				_, _ = store.LoadRecord(tuple.Tuple{int64(1)})
				_, _ = store.LoadRecord(tuple.Tuple{int64(2)})

				// Delete 1 record
				_, err = store.DeleteRecord(tuple.Tuple{int64(3)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(timer.GetCount(EventSaveRecord)).To(Equal(int64(3)))
			Expect(timer.GetCount(EventLoadRecord)).To(Equal(int64(2)))
			Expect(timer.GetCount(EventDeleteRecord)).To(Equal(int64(1)))
			Expect(timer.GetCount(CountSaveRecordKey)).To(Equal(int64(3)))
			Expect(timer.GetCount(CountDeleteRecordKey)).To(Equal(int64(1)))

			// Snapshot reflects all events
			snap := timer.Snapshot()
			Expect(snap).To(HaveKey("save_record"))
			Expect(snap).To(HaveKey("load_record"))
			Expect(snap).To(HaveKey("delete_record"))
			Expect(snap["save_record"].Count).To(Equal(int64(3)))
		})
	})
})
