package recordlayer

import (
	"time"

	"fdb.dev/pkg/dst"
)

// ScanLimiterState holds the mutable, execution-scoped counters that the
// scanned-records / scanned-bytes / time limits are checked against. It is
// the structural analog of Java's ExecuteState (ExecuteState.java:44-47, the
// RecordScanLimiter + ByteScanLimiter pair) plus the transaction-anchored
// TimeScanLimiter that CursorLimitManager mints from
// FDBRecordContext.getTransactionCreateTime() (CursorLimitManager.java:88-98)
// — NOT the Go type literally named ExecuteState (execute_state.go), which is
// a Go-only, statement-scoped extension (RFC-130's memory byte budget, no
// Java analog) held on ExecuteProperties.State. The two live on separate
// pointer fields with separate lifetimes for a reason: State/ExecuteState is
// minted ONCE per statement and survives every page/transaction retry
// (RFC-130 deliberately budgets memory across the whole result set); ScanState/
// ScanLimiterState is minted fresh every time DefaultExecuteProperties is
// called — which paginatingRows.executeProps() does exactly once per page,
// INSIDE the retried transaction closure, so every retry attempt and every
// page gets its own honest scan/byte/time budget, matching Java's
// CursorLimitManager being rebuilt from a fresh FDBRecordContext each attempt.
// Do not read this comment's resemblance to "ExecuteState" as a naming
// correspondence with Go's existing ExecuteState type; it names the Java
// class this ports, not the sibling Go type.
//
// The pointer is carried on ExecuteProperties and copied BY REFERENCE through
// every value-copy (WithX helpers, ClearSkipAndLimit, per-leg innerProps) —
// the same reference-sharing pattern ExecuteProperties.State uses for the
// RFC-130 memory budget, just page-scoped instead of statement-scoped. That
// reference sharing is the fix for a real hang: an IN-join/IN-union with many
// legs previously gave every leg's leaf cursor its OWN local
// recordsScanned/bytesScanned/startTime, so a scan/byte/time limit configured
// to keep one FDB transaction under the 5s hard wall never tripped when each
// individual leg's own contribution stayed small — the limit was checked,
// just against the wrong (per-leg) denominator. A single ExecuteProperties
// value threaded through ExecutePlan's recursive leg calls
// (executeInJoin/executeInUnion: props.ClearSkipAndLimit(), never a fresh
// DefaultExecuteProperties()) now shares ONE counter set across every leg, so
// the aggregate scan actually stops (or errors, with FailOnScanLimitReached)
// once the total crosses the configured budget — matching Java's
// per-transaction CursorLimitManager sharing.
//
// A nil *ScanLimiterState (any ExecuteProperties built as a raw struct
// literal rather than through DefaultExecuteProperties) is handled by each
// leaf cursor falling back to a private, cursor-local instance — identical
// to the pre-fix per-cursor counters, so every existing single-cursor caller
// is unaffected.
//
// Concurrency: plain (non-atomic) fields, deliberately. Every leaf cursor
// that touches a ScanLimiterState is reached exclusively through the
// executor's single-threaded call chain (pkg/recordlayer/query/executor
// launches zero goroutines — the pinned invariant test package_invariant_test.go
// enforces this, and execute_state.go's ExecuteState relies on the same
// guarantee for its own counters) and none of the leaf cursors in this
// package (key_value_cursor.go, index_scan.go, record_key_cursor.go,
// count_index_maintainer.go, bitmap_value_index_maintainer.go,
// multidimensional_index_maintainer.go, text_cursor.go) spawn a goroutine
// either — including text_cursor.go's KVCallback, invoked synchronously from
// within textCursor.OnNext's own call to the underlying iterator, never from
// a separate goroutine. An earlier version of this file used
// sync/atomic for bytesScanned only, defensively, while leaving
// recordsScanned a plain field — inconsistent under both readings of the
// invariant (if it holds, the atomic ops are pure overhead — a per-record RMW
// where a `+=` used to suffice; if it doesn't, recordsScanned was ALREADY a
// silent race the moment anything pipelines). If the executor ever goes
// concurrent, the pinned test fires and every field here moves to atomic
// together, not just the one a defensive habit happened to cover.
type ScanLimiterState struct {
	recordsScanned int
	bytesScanned   int64
	startTime      time.Time

	// env supplies the clock BOTH the anchor and every elapsed measurement are taken on. It is
	// stored rather than passed per call so the two cannot come from different clocks: an
	// anchor minted on the wall clock and compared against a simulated Now is not merely
	// nondeterministic, it is the difference between two unrelated epochs, so the time limit
	// trips on the first record or never. A nil env is production (wall clock), which is what
	// every caller that does not run under a simulation gets.
	env *dst.Env
}

// resolveScanLimiterState returns props.ScanState when the caller threaded
// one through (the normal DefaultExecuteProperties path), or a fresh private
// instance otherwise. The private-instance fallback is what every leaf
// cursor gets when ExecuteProperties was built as a raw struct literal
// (bypassing DefaultExecuteProperties, which always mints a ScanState) —
// identical to the pre-fix per-cursor-local counters, so those call sites
// are unaffected by this change.
//
// The fallback anchors on the WALL CLOCK (NewScanLimiterState passes a nil env), which is a hole
// in the DST claim that a simulated run takes every clock read from the env: nothing in the type
// system stops a raw-literal ExecuteProperties from reaching a leaf cursor inside a simulation.
// Closing it structurally is not a small change and would not actually close it — a struct
// literal always compiles with a nil pointer field, so "required" could only mean panicking in a
// constructor that cannot return an error, and there are ~70 literal sites.
//
// What makes it harmless is a fact about call sites: the clock only DECIDES anything here through
// the time budget (which is what ends a page and picks the continuation), and the one production
// site that arms a time limit builds its properties with DefaultExecutePropertiesIn. That fact is
// gated, not merely asserted — pkg/docscheck TestScanLimiterStateArmingIsSeamed fails the build
// if any production function arms a time limit without the seamed constructor.
func resolveScanLimiterState(props ExecuteProperties) *ScanLimiterState {
	if props.ScanState != nil {
		return props.ScanState
	}
	return NewScanLimiterState()
}

// NewScanLimiterState mints a fresh counter set anchored to the current
// instant — Java's FDBRecordContext.transactionCreateTime, set once when the
// transaction/execution attempt begins (FDBRecordContext.java:187) and
// shared by every TimeScanLimiter minted for that transaction
// (CursorLimitManager.java:93).
func NewScanLimiterState() *ScanLimiterState {
	return NewScanLimiterStateIn(nil)
}

// NewScanLimiterStateIn is NewScanLimiterState anchored on env's clock.
//
// The time budget is not instrumentation: it decides whether a leaf cursor stops with
// TimeLimitReached, and therefore WHERE THE PAGE ENDS and what continuation the caller gets
// back. The SQL layer arms it on every single statement (paginatingRows.executeProps clamps to
// a per-transaction page budget so the FDB 5s wall is never crossed), so a wall-clock anchor
// means a simulated run pages differently depending on how fast the machine happened to be —
// nondeterminism in the exact layer RFC-199 exists to make reproducible.
func NewScanLimiterStateIn(env *dst.Env) *ScanLimiterState {
	return &ScanLimiterState{startTime: env.Now(), env: env}
}

// RecordsScanned returns the number of records charged against this state so
// far. A nil receiver (no shared state configured) reports zero.
func (s *ScanLimiterState) RecordsScanned() int {
	if s == nil {
		return 0
	}
	return s.recordsScanned
}

// AddRecordScanned charges one more record against the shared counter.
func (s *ScanLimiterState) AddRecordScanned() {
	if s == nil {
		return
	}
	s.recordsScanned++
}

// BytesScanned returns the number of bytes charged against this state so
// far. A nil receiver reports zero.
func (s *ScanLimiterState) BytesScanned() int64 {
	if s == nil {
		return 0
	}
	return s.bytesScanned
}

// AddBytesScanned charges n more bytes against the shared counter.
func (s *ScanLimiterState) AddBytesScanned(n int64) {
	if s == nil {
		return
	}
	s.bytesScanned += n
}

// StartTime returns the instant this state was minted — the anchor every
// leaf cursor sharing it measures TimeLimit against. A nil receiver returns
// the zero Time; callers must not call this on a nil state for a live time
// check (leaf cursors fall back to a private, non-nil state first).
//
// Use Elapsed for a live limit check. Reading the anchor and subtracting it from some other
// clock is the mistake this type now makes structurally impossible.
func (s *ScanLimiterState) StartTime() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.startTime
}

// Elapsed reports how long this state has been running, on the SAME clock its anchor was minted
// on. Every leaf cursor's TimeLimit check goes through here.
//
// A nil receiver reports zero, which reads as "no time has passed" and so never trips a limit —
// the safe direction for a state that was never configured.
func (s *ScanLimiterState) Elapsed() time.Duration {
	if s == nil {
		return 0
	}
	return s.env.Since(s.startTime)
}
