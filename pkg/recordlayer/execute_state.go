package recordlayer

import "fmt"

// ExecuteState holds the mutable, statement-scoped counters for a single
// statement execution. It mirrors Java's
// com.apple.foundationdb.record.ExecuteState (ExecuteState.java:44-47): the
// counters live in a small mutable object held BY REFERENCE inside the
// otherwise value-copied ExecuteProperties, so every value-copy of the
// properties (the WithX helpers, ClearSkipAndLimit, per-operator innerProps)
// shares one counter and none of them reset it. Statement-wide survival is
// therefore STRUCTURAL — exactly as in Java, where clearSkipAndLimit
// (:240-245) preserves the ExecuteState for free because it only zeroes
// skip/rowLimit.
//
// RFC-130: the only counter tracked here today is the statement-wide memory
// byte budget that bounds in-memory buffering operators (the row-count
// MaterializationLimit cannot stop 100k LARGE rows from OOMing). The
// scan/byte/time counters Go currently tracks per-cursor are the correct
// future home here too, to fully match Java — out of scope for RFC-130, noted
// for the divergence ledger.
//
// Concurrency: the executor is single-threaded per statement (zero goroutine
// launches in pkg/recordlayer/query/executor — pinned by
// package_invariant_test.go), so ChargeMemory needs no mutex/atomic. If a
// future parallel-union breaks that invariant the pinned test fires and the
// counter moves to atomic.
type ExecuteState struct {
	memUsed  int64
	memLimit int64
}

// NewExecuteState mints a fresh statement-scoped state with the given memory
// byte limit. A limit <= 0 means unlimited (no budget). The state is ALWAYS
// minted once per statement — never nil — so a missed accumulation site
// charges an unlimited counter (a no-op charge) rather than silently
// no-oping via a nil receiver, which would make a missed wiring an invisible
// bypass instead of a visible (if unbounded) charge.
func NewExecuteState(memLimit int64) *ExecuteState {
	return &ExecuteState{memLimit: memLimit}
}

// MemUsed returns the bytes charged against this state so far. Test/diagnostic
// accessor; production code never reads it.
func (s *ExecuteState) MemUsed() int64 {
	if s == nil {
		return 0
	}
	return s.memUsed
}

// MemLimit returns the configured memory byte budget (<= 0 == unlimited).
func (s *ExecuteState) MemLimit() int64 {
	if s == nil {
		return 0
	}
	return s.memLimit
}

// HasMemLimit reports whether a positive memory budget is active. Callers MUST
// gate any expensive byte-estimate computation (e.g. the per-row proto Size()
// walk in estimateQueryResultBytes) on this, so the default/unlimited path pays
// nothing — RFC-130's zero-overhead-when-off invariant. nil receiver or
// memLimit<=0 → false.
func (s *ExecuteState) HasMemLimit() bool {
	return s != nil && s.memLimit > 0
}

// ChargeMemory adds n bytes to the statement's running memory total and
// returns a *MemoryLimitExceededError if the total then exceeds the budget.
//
// A nil receiver or a non-positive memLimit means "no budget" and is a no-op
// (memUsed is still accumulated for an active budget, but a <=0 limit short-
// circuits before touching the counter so the unlimited path stays free). The
// charge is applied BEFORE the check, so the error carries the would-be total
// — the buffer that breaches the budget is never kept by the caller, which
// errors out instead of appending.
func (s *ExecuteState) ChargeMemory(n int64) error {
	if s == nil || s.memLimit <= 0 {
		return nil
	}
	s.memUsed += n
	if s.memUsed > s.memLimit {
		return &MemoryLimitExceededError{Used: s.memUsed, Limit: s.memLimit}
	}
	return nil
}

// MemoryLimitExceededError is returned when a statement's accounted in-memory
// buffering exceeds the statement-wide memory byte budget (RFC-130). It is a
// per-statement resource-limit error in the same family as the scan/byte/time
// limits — the relational layer maps it to SQLSTATE 54F01
// (ErrCodeExecutionLimitReached), matching Java's reuse of that SQLSTATE for
// resource limits (there is no memory-specific SQLSTATE in Java either).
type MemoryLimitExceededError struct {
	// Used is the running memory total (in bytes) at the point the budget
	// was breached — i.e. the would-be total INCLUDING the charge that
	// tripped it.
	Used int64
	// Limit is the configured statement-wide memory byte budget.
	Limit int64
}

func (e *MemoryLimitExceededError) Error() string {
	return fmt.Sprintf("statement memory budget exceeded: %d bytes buffered exceeds limit %d bytes", e.Used, e.Limit)
}
