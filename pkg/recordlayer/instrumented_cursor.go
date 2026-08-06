package recordlayer

import (
	"context"
	"time"
)

// instrumentedCursor times every OnNext call and records the elapsed span
// against one event. It is the port of Java's StoreTimer.instrument(Event,
// RecordCursor) (StoreTimer.java:894-931), whose Javadoc is explicit that
// "timing information is recorded for each invocation of the
// RecordCursor#onNext() asynchronous method" (StoreTimer.java:885-886).
//
// Per-OnNext is the whole point, and it is what an elapsed span around cursor
// CONSTRUCTION cannot give you. A record cursor is lazy: building it issues no
// read, so timing the construction measures allocation and nothing else, and
// yields a count of 1 for a scan that returned a million rows. Java's numbers
// mean "N records passed through, costing T in total"; that is the shape an
// operator can divide into a per-record cost, and it is the shape this
// wrapper reproduces.
//
// The count therefore lands at records-produced plus one — the terminal
// no-next-record probe is an OnNext like any other, and Java counts it too
// (its wrapper instruments every onNext, including the one that reports
// exhaustion).
type instrumentedCursor[T any] struct {
	inner RecordCursor[T]
	timer *StoreTimer
	event Event
}

// instrumentCursor wraps inner so each OnNext is timed against event.
//
// A nil timer returns inner unwrapped rather than a wrapper that checks for
// nil on every element: cursors are the hot path, an uninstrumented store is
// the default, and the nil check belongs at the wrap site where it runs once.
func instrumentCursor[T any](timer *StoreTimer, event Event, inner RecordCursor[T]) RecordCursor[T] {
	if timer == nil {
		return inner
	}
	return &instrumentedCursor[T]{inner: inner, timer: timer, event: event}
}

// InstrumentIndexScanCursor times each entry an index-scan cursor produces into
// EventScanIndex. It is the exported form of the wrapper the store's own scan
// methods apply, for the one caller that cannot use them: the query executor
// builds its index cursor from the maintainer directly (it needs the per-range
// factory that FDBRecordStore.ScanIndex does not expose), so without this the
// engine's primary index-scan path produces no scan event at all.
//
// Decorating the maintainer instead would be the tidier-looking fix and is wrong:
// IndexMaintainer implementations are type-asserted for capability interfaces
// (RankQuerier among them), and a struct decorator silently fails those
// assertions.
//
// A nil timer returns the cursor unchanged, so an uninstrumented store pays
// nothing per entry.
func InstrumentIndexScanCursor(timer *StoreTimer, inner RecordCursor[*IndexEntry]) RecordCursor[*IndexEntry] {
	return instrumentCursor(timer, EventScanIndex, inner)
}

func (c *instrumentedCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	start := time.Now()
	result, err := c.inner.OnNext(ctx)
	// Recorded on the error path too — Java's instrumentAsync uses
	// whenComplete, which fires for both completion and exception
	// (StoreTimer.java:933-940). Time spent on a read that failed was still
	// time spent, and dropping it would make a failing scan look free.
	c.timer.RecordSince(c.event, start)
	return result, err
}

func (c *instrumentedCursor[T]) Close() error { return c.inner.Close() }

func (c *instrumentedCursor[T]) IsClosed() bool { return c.inner.IsClosed() }
