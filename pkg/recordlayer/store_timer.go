package recordlayer

import (
	"sync"
	"sync/atomic"
	"time"
)

// Event represents a measurable instrumentation event.
// Matches Java's FDBStoreTimer.Events / FDBStoreTimer.Counts.
type Event struct {
	Name  string // machine-readable, e.g. "save_record"
	Title string // human-readable, e.g. "Save Record"
}

// Standard timed events — matching Java's FDBStoreTimer.Events.
var (
	EventSaveRecord     = Event{"save_record", "Save Record"}
	EventLoadRecord     = Event{"load_record", "Load Record"}
	EventDeleteRecord   = Event{"delete_record", "Delete Record"}
	EventCommit         = Event{"commit", "Commit"}
	EventGetReadVersion = Event{"get_read_version", "Get Read Version"}
	EventScanRecords    = Event{"scan_records", "Scan Records"}
	EventScanIndex      = Event{"scan_index", "Scan Index"}
	EventOpenStore      = Event{"open_store", "Open Store"}
	EventRebuildIndex   = Event{"rebuild_index", "Rebuild Index"}
)

// Standard count events — matching Java's FDBStoreTimer.Counts.
var (
	CountSaveRecordKey        = Event{"save_record_key", "Save Record Key"}
	CountSaveRecordKeyBytes   = Event{"save_record_key_bytes", "Save Record Key Bytes"}
	CountSaveRecordValueBytes = Event{"save_record_value_bytes", "Save Record Value Bytes"}
	CountDeleteRecordKey      = Event{"delete_record_key", "Delete Record Key"}
	CountDeleteRecordKeyBytes = Event{"delete_record_key_bytes", "Delete Record Key Bytes"}
	CountReads                = Event{"reads", "Reads"}
	CountWrites               = Event{"writes", "Writes"}
	CountBytesRead            = Event{"bytes_read", "Bytes Read"}
	CountBytesWritten         = Event{"bytes_written", "Bytes Written"}
)

// Counter tracks count and cumulative value (nanoseconds for timed events,
// bytes for size events). All operations are goroutine-safe.
type Counter struct {
	count           atomic.Int64
	cumulativeValue atomic.Int64
}

// Record records a single observation with the given value.
// For timed events, value is nanoseconds. For size events, value is bytes.
func (c *Counter) Record(value int64) {
	c.count.Add(1)
	c.cumulativeValue.Add(value)
}

// Increment adds amount to both count and cumulative value.
func (c *Counter) Increment(amount int64) {
	c.count.Add(amount)
	c.cumulativeValue.Add(amount)
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
type CounterSnapshot struct {
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
	c := &Counter{}
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
			Count:           c.Count(),
			CumulativeValue: c.CumulativeValue(),
		}
		return true
	})
	return result
}
