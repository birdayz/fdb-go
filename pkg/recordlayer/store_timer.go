package recordlayer

import (
	"sync"
	"sync/atomic"
	"time"
)

// Kind classifies an event the way Java's StoreTimer taxonomy does with
// interfaces (StoreTimer.java:203-402). Java splits the classification across
// the type system — a bare `Event` is timed, `Count extends Event` adds
// `isSize()` — and reads it back with `instanceof` in getKeysAndValues
// (StoreTimer.java:747-780). Go has no sealed interface hierarchy, so the same
// three-way distinction is carried as data on the one Event struct and read
// back with a switch. The distinction is not cosmetic: it decides which field
// of the Counter carries meaning, and therefore how the event may be exported.
type Kind uint8

const (
	// KindUnspecified is the zero value, and no declared event may carry it.
	// Making the zero value invalid is deliberate: every Event literal in this
	// package is unkeyed, so adding Kind to the struct made the compiler
	// demand a classification at each declaration rather than silently
	// defaulting a new event into whichever kind happened to be zero.
	KindUnspecified Kind = iota

	// KindTimed is Java's bare StoreTimer.Event: recorded with Record /
	// RecordSince, cumulative value is nanoseconds. Both Count() (occurrences)
	// and CumulativeValue() (total nanos) are meaningful.
	KindTimed

	// KindCount is Java's StoreTimer.Count with isSize()==false: recorded with
	// Increment / IncrementBy, ONLY Count() is meaningful. CumulativeValue()
	// stays 0, exactly as in Java (Counter.increment touches count alone,
	// StoreTimer.java:484-487).
	KindCount

	// KindSize is Java's StoreTimer.Count with isSize()==true — e.g.
	// Counts.SAVE_RECORD_KEY_BYTES (FDBStoreTimer.java:524). Recorded and read
	// exactly like KindCount: the BYTE TOTAL lands in Count(), not in
	// CumulativeValue(). Kept a distinct kind because it changes what the
	// number means to a consumer (bytes, not occurrences) even though the
	// storage is identical.
	KindSize
)

// String renders the kind for diagnostics.
func (k Kind) String() string {
	switch k {
	case KindTimed:
		return "timed"
	case KindCount:
		return "count"
	case KindSize:
		return "size"
	case KindUnspecified:
		return "unspecified"
	default:
		return "unknown"
	}
}

// Event represents a measurable instrumentation event.
// Matches Java's FDBStoreTimer.Events / FDBStoreTimer.Counts.
type Event struct {
	Name  string // machine-readable, e.g. "save_record"
	Title string // human-readable, e.g. "Save Record"
	Kind  Kind   // how the counter's fields are to be read
}

// Standard timed events — matching Java's FDBStoreTimer.Events.
//
// EventOpenStore is the one entry with no Java Events counterpart: Java splits
// store opening into Counts.OPEN_CONTEXT (FDBStoreTimer.java:510) plus the
// timed Events.CHECK_VERSION (:198) and Events.LOAD_RECORD_STORE_STATE (:103).
// Go times the whole open as one event.
var (
	EventSaveRecord     = Event{"save_record", "Save Record", KindTimed}
	EventLoadRecord     = Event{"load_record", "Load Record", KindTimed}
	EventDeleteRecord   = Event{"delete_record", "Delete Record", KindTimed}
	EventCommit         = Event{"commit", "Commit", KindTimed}
	EventGetReadVersion = Event{"get_read_version", "Get Read Version", KindTimed}
	EventScanRecords    = Event{"scan_records", "Scan Records", KindTimed}
	EventScanIndex      = Event{"scan_index", "Scan Index", KindTimed}
	EventOpenStore      = Event{"open_store", "Open Store", KindTimed}
	EventRebuildIndex   = Event{"rebuild_index", "Rebuild Index", KindTimed}
)

// Standard count events — matching Java's FDBStoreTimer.Counts. The KindSize
// entries are the ones whose Java constant is declared with isSize()==true.
var (
	CountSaveRecordKey        = Event{"save_record_key", "Save Record Key", KindCount}
	CountSaveRecordKeyBytes   = Event{"save_record_key_bytes", "Save Record Key Bytes", KindSize}
	CountSaveRecordValueBytes = Event{"save_record_value_bytes", "Save Record Value Bytes", KindSize}
	CountDeleteRecordKey      = Event{"delete_record_key", "Delete Record Key", KindCount}
	CountDeleteRecordKeyBytes = Event{"delete_record_key_bytes", "Delete Record Key Bytes", KindSize}
	CountReads                = Event{"reads", "Reads", KindCount}
	CountWrites               = Event{"writes", "Writes", KindCount}
	CountBytesRead            = Event{"bytes_read", "Bytes Read", KindSize}
	CountBytesWritten         = Event{"bytes_written", "Bytes Written", KindSize}

	// Index-level count events — matching Java's FDBStoreTimer.Counts for index ops.
	// Used by InstrumentedBunchedMap (TEXT index) and other index maintainers.
	CountSaveIndexKey          = Event{"save_index_key", "Save Index Key", KindCount}
	CountSaveIndexKeyBytes     = Event{"save_index_key_bytes", "Save Index Key Bytes", KindSize}
	CountSaveIndexValueBytes   = Event{"save_index_value_bytes", "Save Index Value Bytes", KindSize}
	CountLoadIndexKey          = Event{"load_index_key", "Load Index Key", KindCount}
	CountLoadIndexKeyBytes     = Event{"load_index_key_bytes", "Load Index Key Bytes", KindSize}
	CountLoadIndexValueBytes   = Event{"load_index_value_bytes", "Load Index Value Bytes", KindSize}
	CountDeleteIndexKey        = Event{"delete_index_key", "Delete Index Key", KindCount}
	CountDeleteIndexKeyBytes   = Event{"delete_index_key_bytes", "Delete Index Key Bytes", KindSize}
	CountDeleteIndexValueBytes = Event{"delete_index_value_bytes", "Delete Index Value Bytes", KindSize}
)

// Counter tracks an occurrence count plus a cumulative value.
// Matches Java's StoreTimer.Counter (StoreTimer.java:408-517), including which
// of the two fields carries the payload for each Kind:
//
//   - KindTimed: Count() is occurrences, CumulativeValue() is total nanoseconds.
//   - KindCount / KindSize: Count() carries everything (occurrences, or a byte
//     total for KindSize) and CumulativeValue() stays 0.
//
// The second bullet is the non-obvious one, and it is Java's:
// Counter.increment(int) adds to `count` alone (StoreTimer.java:484-487), which
// is why Java can use `getTimeNanos() == 0` as a proof that an event is a
// counter rather than a timer (RecordLayerMetricCollector.java:90).
//
// All operations are goroutine-safe.
type Counter struct {
	// event is the classified Event this counter was created for, so a
	// Snapshot can report a Kind rather than just a name. Immutable after
	// getOrCreateCounter stores it.
	event Event

	count           atomic.Int64
	cumulativeValue atomic.Int64
}

// Event returns the event this counter records.
func (c *Counter) Event() Event {
	return c.event
}

// Record records a single observation with the given value: one occurrence,
// plus value added to the cumulative total. Java's Counter.record
// (StoreTimer.java:473-477). For timed events, value is nanoseconds.
func (c *Counter) Record(value int64) {
	c.count.Add(1)
	c.cumulativeValue.Add(value)
}

// Increment adds amount to the occurrence count, leaving the cumulative value
// untouched — Java's Counter.increment (StoreTimer.java:484-487). For a
// KindSize event the byte total therefore accumulates in Count(), which is
// where Java keeps it too (getKeysAndValues emits a size Count as a plain
// `_count` key, StoreTimer.java:754).
func (c *Counter) Increment(amount int64) {
	c.count.Add(amount)
}

// Count returns the number of observations recorded.
func (c *Counter) Count() int64 {
	return c.count.Load()
}

// CumulativeValue returns the sum of all recorded values.
func (c *Counter) CumulativeValue() int64 {
	return c.cumulativeValue.Load()
}

// Reset zeroes the counter.
func (c *Counter) Reset() {
	c.count.Store(0)
	c.cumulativeValue.Store(0)
}

// CounterSnapshot is an immutable point-in-time snapshot of a Counter.
// Matches Java's StoreTimerSnapshot.CounterSnapshot (StoreTimerSnapshot.java:179-226).
//
// Event is carried alongside the numbers because the numbers are not
// self-describing: without the Kind, a consumer cannot tell whether Count is
// occurrences or bytes, nor whether CumulativeValue is a duration or
// meaningless. Java recovers that from the map key's static type; Go carries it.
type CounterSnapshot struct {
	Event           Event
	Count           int64
	CumulativeValue int64
}

// StoreTimer collects instrumentation counters for Record Layer operations.
// All operations are goroutine-safe. A nil *StoreTimer is safe to use
// (all methods are no-ops on nil receiver).
type StoreTimer struct {
	counters sync.Map // map[string]*Counter, keyed by Event.Name
}

// NewStoreTimer creates a new StoreTimer.
func NewStoreTimer() *StoreTimer {
	return &StoreTimer{}
}

// getOrCreateCounter returns the counter for the event, creating it if needed.
func (t *StoreTimer) getOrCreateCounter(event Event) *Counter {
	if v, ok := t.counters.Load(event.Name); ok {
		return v.(*Counter)
	}
	c := &Counter{event: event}
	actual, _ := t.counters.LoadOrStore(event.Name, c)
	return actual.(*Counter)
}

// Record records an elapsed time in nanoseconds for the given event.
func (t *StoreTimer) Record(event Event, timeNanos int64) {
	if t == nil {
		return
	}
	t.getOrCreateCounter(event).Record(timeNanos)
}

// RecordSince records the duration elapsed since startTime for the given event.
func (t *StoreTimer) RecordSince(event Event, startTime time.Time) {
	if t == nil {
		return
	}
	t.getOrCreateCounter(event).Record(time.Since(startTime).Nanoseconds())
}

// Increment increments the event's count and cumulative value by 1.
func (t *StoreTimer) Increment(event Event) {
	if t == nil {
		return
	}
	t.getOrCreateCounter(event).Increment(1)
}

// IncrementBy increments the event's count and cumulative value by amount.
func (t *StoreTimer) IncrementBy(event Event, amount int64) {
	if t == nil {
		return
	}
	t.getOrCreateCounter(event).Increment(amount)
}

// GetCounter returns the counter for the given event, or nil if never recorded.
func (t *StoreTimer) GetCounter(event Event) *Counter {
	if t == nil {
		return nil
	}
	v, ok := t.counters.Load(event.Name)
	if !ok {
		return nil
	}
	return v.(*Counter)
}

// GetCount returns the occurrence count for the given event (0 if never recorded).
func (t *StoreTimer) GetCount(event Event) int64 {
	if t == nil {
		return 0
	}
	c := t.GetCounter(event)
	if c == nil {
		return 0
	}
	return c.Count()
}

// GetTimeNanos returns the cumulative nanoseconds for the given event (0 if never recorded).
func (t *StoreTimer) GetTimeNanos(event Event) int64 {
	if t == nil {
		return 0
	}
	c := t.GetCounter(event)
	if c == nil {
		return 0
	}
	return c.CumulativeValue()
}

// Reset clears all counters from the timer.
func (t *StoreTimer) Reset() {
	if t == nil {
		return
	}
	t.counters.Range(func(key, _ any) bool {
		t.counters.Delete(key)
		return true
	})
}

// Snapshot returns an immutable snapshot of all counters at the current instant.
// The returned map is keyed by Event.Name.
func (t *StoreTimer) Snapshot() map[string]*CounterSnapshot {
	if t == nil {
		return nil
	}
	result := make(map[string]*CounterSnapshot)
	t.counters.Range(func(key, value any) bool {
		c, ok := value.(*Counter)
		if !ok {
			return true
		}
		k, ok := key.(string)
		if !ok {
			return true
		}
		result[k] = &CounterSnapshot{
			Event:           c.Event(),
			Count:           c.Count(),
			CumulativeValue: c.CumulativeValue(),
		}
		return true
	})
	return result
}

// Add folds every counter of other into this timer, matching Java's
// StoreTimer.add (StoreTimer.java:731-740).
//
// This is Java's mechanism for aggregating instrumentation collected by a
// short-lived timer into a longer-lived one. It is NOT the only one, and not
// the one the SQL layer uses: Java's other model shares a single timer instance
// across many contexts (FDBDatabaseRunner.setTimer feeds every context the
// retry loop opens, FDBDatabaseRunner.java:105-119), which needs no fold at all
// because the contexts write into the same counters. Go's FDBDatabase-level
// timer follows that second model. Add exists for callers doing the first —
// measuring one unit of work in a scratch timer and rolling it up afterwards.
//
// Counters are read one at a time, so a timer being concurrently written to
// does not tear an individual counter but may contribute a mix of pre- and
// post-write values across counters. Java has exactly the same property.
func (t *StoreTimer) Add(other *StoreTimer) {
	if t == nil || other == nil || t == other {
		return
	}
	other.counters.Range(func(_, value any) bool {
		src, ok := value.(*Counter)
		if !ok {
			return true
		}
		dst := t.getOrCreateCounter(src.Event())
		dst.count.Add(src.Count())
		dst.cumulativeValue.Add(src.CumulativeValue())
		return true
	})
}

// KeysAndValues renders the timer in Java's log-key form — the map
// StoreTimer.getKeysAndValues produces (StoreTimer.java:747-780) and feeds to
// KeyValueLogMessage.
//
// The suffix rules are Java's, verbatim: every event emits `<name>_count`, and
// a timed event additionally emits `<name>_micros` carrying nanoseconds divided
// by 1000 with integer truncation. A Count — including a size Count — emits
// only `_count`, because its cumulative value is 0 by construction.
//
// This is the LOGGING surface and is deliberately distinct from the Prometheus
// exposition in pkg/recordlayer/rlmetrics, which speaks that ecosystem's
// conventions (base units, `_total`, summaries) instead of Java's log keys.
// Both read the same counters; neither is a re-encoding of the other.
func (t *StoreTimer) KeysAndValues() map[string]int64 {
	if t == nil {
		return nil
	}
	result := make(map[string]int64)
	t.counters.Range(func(_, value any) bool {
		c, ok := value.(*Counter)
		if !ok {
			return true
		}
		ev := c.Event()
		result[ev.Name+"_count"] = c.Count()
		if ev.Kind == KindTimed {
			result[ev.Name+"_micros"] = c.CumulativeValue() / 1000
		}
		return true
	})
	return result
}
