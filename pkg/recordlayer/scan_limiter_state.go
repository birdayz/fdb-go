// Portions derived from FoundationDB Record Layer (ExecuteState.java,
// CursorLimitManager.java, FDBRecordContext.java, RecordScanLimiter.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"time"

	"fdb.dev/pkg/dst"
)

// ScanLimiterState shares Java ExecuteState's scan/byte budgets and a transaction-anchored time limit.
// Property copies must retain it: separate IN-join/IN-union leg budgets can overrun the transaction's limit.
//
// Auto-commit pages/retries get fresh state; explicit transactions share it at the read-version instant.
// Without ExecuteProperties.ScanState each leaf has private state.
//
// Concurrency: plain fields rely on the single-threaded executor (executor/package_invariant_test.go).
// Concurrent cursors require synchronizing all fields, not just bytes.
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

// resolveScanLimiterState preserves the shared budget, or supplies a private
// wall-clock budget when none was provided. Simulations must supply ScanState.
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

// AnchorAt moves the time anchor to t without resetting records/bytes counters;
// explicit transactions use the read-version instant. Nil receivers and zero t are ignored.
func (s *ScanLimiterState) AnchorAt(t time.Time) {
	if s == nil || t.IsZero() {
		return
	}
	s.startTime = t
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

// StartTime returns the current time-budget anchor, or zero for a nil receiver.
// Use Elapsed for limit checks so the anchor and measurement use the same clock.
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
