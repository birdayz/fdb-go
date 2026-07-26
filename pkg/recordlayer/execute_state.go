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
// RFC-130 minted the memory byte budget that bounds in-memory buffering
// operators (the row-count MaterializationLimit cannot stop 100k LARGE rows
// from OOMing). recursionLevels below is a second, unrelated statement-scoped
// counter set that reuses the same by-reference survival pattern for a
// different purpose (see its own doc comment). The scan/byte/time counters Go
// currently tracks per-cursor are the correct future home here too, to fully
// match Java — out of scope for RFC-130, noted for the divergence ledger.
//
// Concurrency: the executor is single-threaded per statement (zero goroutine
// launches in pkg/recordlayer/query/executor — pinned by
// package_invariant_test.go), so ChargeMemory/IncrementRecursionLevel need no
// mutex/atomic. If a future parallel-union breaks that invariant the pinned
// test fires and the counters move to atomic / a mutex.
type ExecuteState struct {
	memUsed  int64
	memLimit int64

	// recursionLevels backs the streaming recursive-CTE cycle guard
	// (recordlayer/query/executor/recursive_union_cursor.go) — see
	// IncrementRecursionLevel for why it exists and exactly what it does and
	// does not cover. Lazily allocated; nil until first use.
	recursionLevels map[any]int
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

// ReleaseMemory returns n previously ChargeMemory'd bytes to the statement
// budget. The budget bounds LIVE buffered bytes: a buffer that outlives its
// cursor by round-tripping through a continuation is re-charged at resume
// injection, so the producing cursor must release its charges at teardown —
// otherwise a multi-page statement re-accounts the same buffer once per page
// and a compliant query trips the limit merely by crossing page boundaries.
// Callers release exactly what they charged; nil receiver / no budget is a
// no-op mirroring ChargeMemory.
func (s *ExecuteState) ReleaseMemory(n int64) {
	if s == nil || s.memLimit <= 0 {
		return
	}
	s.memUsed -= n
}

// IncrementRecursionLevel bumps and returns the level count tracked under id.
//
// This is the STATEMENT-scoped half of the streaming level-union recursive
// CTE's cycle guard: RecordQueryRecursiveLevelUnionPlan's UNION ALL arm is a
// pure streaming cursor with no fixpoint/dedup (Java has none either — this
// cap is a Go-only safety extension), so a cyclic self-referential CTE only
// terminates via a hard recursion-depth cap. That cap must count LEVELS FOR
// THE WHOLE STATEMENT, but the streaming cursor itself is rebuilt fresh on
// every page (a new *recursiveUnionCursor per fetchPage, per the SQL layer's
// "no cursor persists across transactions" architecture) — a counter living
// on the cursor resets to zero every page and a cyclic CTE that pages
// mid-recursion would never trip it. ExecuteState is the vehicle already used
// for exactly this shape of problem (RFC-130's memory budget): a pointer the
// SQL layer mints ONCE per statement (cascades_generator.go's
// paginatingRows.execState) and threads into every page's
// ExecuteProperties.State, so it survives the per-page cursor rebuild
// structurally.
//
// id anchors the count to one recursive construct — pass the recursive-CTE
// plan node's own pointer, which is stable across every page of one
// statement execution (the SQL layer rebuilds the cursor hierarchy each page
// but reuses the same plan tree). Two independent recursive CTEs in the same
// statement therefore get INDEPENDENT budgets (distinct plan pointers),
// matching the eager recursion arms' own per-call-local bound rather than
// sharing one statement-wide total.
//
// SCOPE, stated exactly: the guard covers one *ExecuteState's lifetime, i.e.
// one Execute() call / one statement execution — cascades_generator.go mints
// a fresh ExecuteState per Execute and never reuses one across statements or
// connections. A continuation resumed by a DIFFERENT statement execution
// cannot inherit the count (there is no such thing today: Go's SQL layer
// rejects caller-supplied continuations outright, see cascadesPlan.Execute,
// so this is not a live gap, just an honest boundary of the mechanism).
//
// A nil receiver returns 0 on every call — it cannot persist anything, by
// construction. This is a Go-only safety cap, not an RFC-130 resource budget,
// so callers must not treat a nil ExecuteState as "no cap": the caller
// (recursiveUnionCursor.checkDepth) always combines this with its own
// per-cursor-instance counter and uses whichever is larger, so a nil State
// still bounds runaway growth within one page/cursor lifetime — identical to
// the pre-fix behavior — even though it can no longer see across pages.
func (s *ExecuteState) IncrementRecursionLevel(id any) int {
	if s == nil {
		return 0
	}
	if s.recursionLevels == nil {
		s.recursionLevels = make(map[any]int)
	}
	s.recursionLevels[id]++
	return s.recursionLevels[id]
}

// ResetRecursionLevel zeroes the level count tracked under id, marking the
// start of a FRESH invocation of a recursive-union plan node.
//
// id is the plan node's own pointer — stable for the whole statement — so
// without this reset, IncrementRecursionLevel's map entry accumulates across
// every invocation of that node, not just within one. That over-counts: a
// recursive CTE nested in a correlated subquery is re-invoked once per outer
// row (recursive_union_cursor.go's newRecursiveUnionCursor is called fresh,
// continuation==nil, from flatMapCursor.OnNext for every outer row — see its
// doc comment), and a CTE nested in a scalar subquery is re-invoked once per
// page it is re-evaluated on. Neither case is a RESUME of an in-flight
// invocation; each is independent and must start its own count at zero, or a
// shallow recursion invoked many times trips the depth cap that no single
// invocation ever approached.
//
// The caller resets exactly when continuation==nil at cursor construction —
// that is the invocation boundary: a resume of an in-flight invocation
// (paging mid-recursion) always carries a non-nil continuation and must NOT
// reset, or the original bug (a cyclic recursion that pages mid-recursion
// streaming forever because its counter resets every page) returns.
//
// A nil receiver or a never-populated map is a no-op — nothing to reset.
func (s *ExecuteState) ResetRecursionLevel(id any) {
	if s == nil || s.recursionLevels == nil {
		return
	}
	delete(s.recursionLevels, id)
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
