package recordlayer

import (
	"context"
	"errors"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
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

		It("records a size distribution the way Java's recordSize does", func() {
			// Java's StoreTimer.SizeEvent is NOT its size Count, and the two
			// differ in exactly the way the assertions below separate: recordSize
			// (StoreTimer.java:569-571) is Counter.record, so the MAGNITUDE
			// accumulates in the cumulative value and the OBSERVATION in the
			// count. Reusing IncrementBy here would report the magnitudes summed
			// into the count — "the window was size 2, then 2, then 1" rendered
			// as the number 5, with the three observations lost.
			t := NewStoreTimer()
			t.RecordSize(SizeSWWindowCount, 2)
			t.RecordSize(SizeSWWindowCount, 2)
			t.RecordSize(SizeSWWindowCount, 1)
			Expect(t.GetCount(SizeSWWindowCount)).To(Equal(int64(3)))
			Expect(t.GetCounter(SizeSWWindowCount).CumulativeValue()).To(Equal(int64(5)))

			// Java emits `_size` for a SizeEvent and `_micros` for nothing else
			// (StoreTimer.java:755-759). The negative half matters: a `_micros`
			// key here would render a window size of 5 as 5 nanoseconds.
			kv := t.KeysAndValues()
			Expect(kv).To(HaveKeyWithValue("sw_window_count_count", int64(3)))
			Expect(kv).To(HaveKeyWithValue("sw_window_count_size", int64(5)))
			Expect(kv).NotTo(HaveKey("sw_window_count_micros"))

			var nilTimer *StoreTimer
			Expect(func() { nilTimer.RecordSize(SizeSWWindowCount, 1) }).NotTo(Panic())
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
			builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
				_, err = store.SaveRecord(&demopb.Order{OrderId: &id, Price: &price})
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
				_, err = store.SaveRecord(&demopb.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
				timer.Reset()
				_, err = store.LoadRecord(tuple.Tuple{int64(1)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(timer.GetCount(EventLoadRecord)).To(Equal(int64(1)))
			Expect(timer.GetTimeNanos(EventLoadRecord)).To(BeNumerically(">", 0))
		})

		for _, api := range []string{"sync", "async", "typed"} {
			for _, outcome := range []string{"found", "missing", "error"} {
				It("records EventLoadRecord once for "+api+"/"+outcome, func() {
					_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						timer := NewStoreTimer()
						rtx.SetTimer(timer)
						store, err := NewStoreBuilder().
							SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
						if err != nil {
							return nil, err
						}
						pk := tuple.Tuple{int64(1)}
						switch outcome {
						case "found":
							_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1)})
							if err != nil {
								return nil, err
							}
						case "error":
							// An invalid protobuf makes the load fail after its FDB read completes.
							rtx.Transaction().Set(store.recordsSubspace.Pack(tuple.Tuple{int64(1), int64(0)}), []byte{0xff})
						}
						timer.Reset()
						var found bool
						switch api {
						case "sync":
							record, loadErr := store.LoadRecord(pk)
							found, err = record != nil, loadErr
						case "async":
							pending := store.LoadRecordAsync(pk, nil)
							record, loadErr := pending.Get()
							found, err = record != nil, loadErr
							elapsed := timer.GetTimeNanos(EventLoadRecord)
							_, repeatedErr := pending.Get()
							Expect(repeatedErr == loadErr).To(BeTrue())
							Expect(timer.GetTimeNanos(EventLoadRecord)).To(Equal(elapsed))
						case "typed":
							typed, typedErr := NewTypedRecordStore[*demopb.Order](store, md.GetRecordType("Order"))
							Expect(typedErr).NotTo(HaveOccurred())
							record, loadErr := typed.LoadRecord(pk)
							found, err = record != nil, loadErr
						}
						if outcome == "error" {
							var deserializationErr *RecordDeserializationError
							Expect(errors.As(err, &deserializationErr)).To(BeTrue(), "load error: %v", err)
						} else {
							Expect(err).NotTo(HaveOccurred())
						}
						Expect(found).To(Equal(outcome == "found"))
						Expect(timer.GetCount(EventLoadRecord)).To(Equal(int64(1)))
						Expect(timer.GetTimeNanos(EventLoadRecord)).To(BeNumerically(">", 0))
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
				})
			}
		}

		for _, outcome := range []string{"found", "missing", "error"} {
			It("records EventLoadRecord for index fetch/"+outcome, func() {
				builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				builder.AddIndex("Order", NewIndex("order_price", Field("price")))
				indexedMD, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					timer := NewStoreTimer()
					rtx.SetTimer(timer)
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(indexedMD).SetSubspace(specSubspace()).CreateOrOpen()
					if err != nil {
						return nil, err
					}
					_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
					if err != nil {
						return nil, err
					}
					key := store.recordsSubspace.Pack(tuple.Tuple{int64(1), int64(0)})
					if outcome == "missing" {
						rtx.Transaction().Clear(key) // Leave an orphan index entry.
					} else if outcome == "error" {
						rtx.Transaction().Set(key, []byte{0xff})
					}
					timer.Reset()
					_, err = AsList(ctx, store.ScanIndexRecords("order_price", TupleRangeAll, nil, ForwardScan()))
					if outcome == "missing" {
						var orphanErr *RecordCoreStorageError
						Expect(errors.As(err, &orphanErr)).To(BeTrue(), "index fetch error: %v", err)
					} else if outcome == "error" {
						var deserializationErr *RecordDeserializationError
						Expect(errors.As(err, &deserializationErr)).To(BeTrue(), "index fetch error: %v", err)
					} else {
						Expect(err).NotTo(HaveOccurred())
					}
					Expect(timer.GetCount(EventLoadRecord)).To(Equal(int64(1)))
					Expect(timer.GetTimeNanos(EventLoadRecord)).To(BeNumerically(">", 0))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}

		for _, api := range []string{"save", "typed-save", "pipelined-save", "dry-save", "delete", "typed-delete", "dry-delete"} {
			for _, outcome := range []string{"found", "missing", "error"} {
				It("records Java timer events for "+api+"/"+outcome, func() {
					_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						timer := NewStoreTimer()
						rtx.SetTimer(timer)
						store, err := NewStoreBuilder().
							SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
						if err != nil {
							return nil, err
						}
						record := &demopb.Order{OrderId: proto.Int64(1)}
						if outcome == "found" {
							_, err = store.SaveRecord(record)
							if err != nil {
								return nil, err
							}
						} else if outcome == "error" {
							rtx.Transaction().Set(store.recordsSubspace.Pack(tuple.Tuple{int64(1), int64(0)}), []byte{0xff})
						}
						typed, err := NewTypedRecordStore[*demopb.Order](store, md.GetRecordType("Order"))
						Expect(err).NotTo(HaveOccurred())
						timer.Reset()
						var event Event
						switch api {
						case "save":
							event = EventSaveRecord
							_, err = store.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExists)
						case "typed-save":
							event = EventSaveRecord
							_, err = typed.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExists)
						case "pipelined-save":
							event = EventSaveRecord
							_, err = store.SaveRecordsPipelined([]proto.Message{record}, RecordExistenceCheckErrorIfNotExists)
						case "dry-save":
							event = EventSaveRecord
							_, err = store.DryRunSaveRecord(record, RecordExistenceCheckErrorIfNotExists)
						case "delete":
							event = EventDeleteRecord
							_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
						case "typed-delete":
							event = EventDeleteRecord
							_, err = typed.DeleteRecord(tuple.Tuple{int64(1)})
						case "dry-delete":
							_, err = store.DryRunDeleteRecord(tuple.Tuple{int64(1)})
						}
						if outcome == "error" {
							var deserializationErr *RecordDeserializationError
							Expect(errors.As(err, &deserializationErr)).To(BeTrue(), "operation error: %v", err)
						} else if outcome == "missing" && event == EventSaveRecord {
							var missingErr *RecordDoesNotExistError
							Expect(errors.As(err, &missingErr)).To(BeTrue(), "operation error: %v", err)
						} else {
							Expect(err).NotTo(HaveOccurred())
						}
						wantCounts := map[string]int64{"load": 1, "save": 0, "delete": 0}
						if api != "dry-delete" {
							if event == EventSaveRecord {
								wantCounts["save"] = 1
							} else {
								wantCounts["delete"] = 1
							}
						}
						Expect(map[string]int64{
							"load":   timer.GetCount(EventLoadRecord),
							"save":   timer.GetCount(EventSaveRecord),
							"delete": timer.GetCount(EventDeleteRecord),
						}).To(Equal(wantCounts))
						Expect(timer.GetTimeNanos(EventLoadRecord)).To(BeNumerically(">", 0))
						if api != "dry-delete" {
							Expect(timer.GetTimeNanos(event)).To(BeNumerically(">", 0))
						}
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
				})
			}
		}

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
				_, err = store.SaveRecord(&demopb.Order{OrderId: &id, Price: &price})
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
				_, err = store.SaveRecord(&demopb.Order{OrderId: &id, Price: &price})
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
					_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(i), Price: &price})
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
			// Three saves and the delete each load the existing record too.
			Expect(timer.GetCount(EventLoadRecord)).To(Equal(int64(6)))
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
