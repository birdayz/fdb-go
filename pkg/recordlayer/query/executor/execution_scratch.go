package executor

import "fmt"

// ExecutionScratch is the statement-scoped home for operator resume state that
// is too large to serialize into every page's continuation.
//
// WHY IT EXISTS. A continuation is normally self-contained: the operator packs
// whatever it needs to resume into bytes, and the bytes alone rebuild it. That
// is right when the state is O(1) — the streaming distinct's single previous
// key, a scan's position. It is quadratic when the state is O(rows already
// emitted), which is exactly the unordered hash distinct: its seen-set is the
// operator's whole memory, so page P's continuation would carry every key
// emitted through page P and a P-page drain would serialize and re-parse
// O(P^2) keys in total. Measured on a 401-page drain of 400 distinct values,
// the continuation grew 9 -> 1749 bytes and the drain re-parsed ~350 KB of keys
// to emit 400 rows.
//
// The state cannot be shrunk: exact dedup over UNORDERED input fundamentally
// needs the set (an ordered input is the case that needs only the last key, and
// the planner already routes it to the streaming executor). Java declines to
// pay either cost — RecordQueryUnorderedPrimaryKeyDistinctPlan.java:100-104
// mints `new HashSet<>()` per execution and passes the inner's continuation
// through untouched, so a duplicate spanning a resume is silently re-admitted.
// Go does not accept wrong rows, so the set has to survive the page boundary;
// the only remaining choice is whether it survives THROUGH bytes or BESIDE
// them. Beside them is the one that is not quadratic.
//
// WHY IT IS SOUND. The bytes are round-tripped by exactly one production
// caller, the SQL statement's paging loop (cascades_generator.go), which
// re-executes the same plan against a fresh transaction to respect FDB's 5s
// bound. Its continuations never escape the statement: the driver rejects
// statement continuations outright (Go SQL tokens are engine-private and no
// resume entry point exists). So a statement-scoped side channel has exactly
// the lifetime the bytes do, and a resume that finds no scratch entry is an
// impossible state rather than a supported one — it is reported as an error,
// never deduped against an empty set.
//
// Concurrency: the executor is single-threaded per statement (zero goroutine
// launches in this package, pinned by package_invariant_test.go), so the maps
// need no lock, exactly as ExecuteState's counters do not.
type ExecutionScratch struct {
	nextToken int64
	distinct  map[int64]*distinctResumeState
}

// NewExecutionScratch mints a scratch for one statement. A nil *ExecutionScratch
// is valid and means "no scratch": operators then fall back to self-contained
// continuations, which is correct and merely quadratic.
func NewExecutionScratch() *ExecutionScratch {
	return &ExecutionScratch{}
}

// MintedDistinctSets returns how many seen-sets have been parked in this
// scratch. Test/diagnostic accessor (like ExecuteState.MemUsed); production
// code never reads it. It exists because the statement layer's wiring of the
// scratch is otherwise INVISIBLE: dropping WithExecutionScratch would leave
// every row-level assertion green while silently restoring the quadratic
// continuation, so the wiring needs an observable of its own.
func (s *ExecutionScratch) MintedDistinctSets() int64 {
	if s == nil {
		return 0
	}
	return s.nextToken
}

// distinctResumeState is one unordered-distinct operator's live seen-set, parked
// between the page that produced it and the page that resumes it.
//
// order/n are the insertion-ordered keys and the prefix length the continuation
// was snapshotted at. seen may hold MORE than order[:n] if the cursor advanced
// after the continuation was serialized, so adoption checks the two agree and
// rebuilds the set from the immutable prefix when they do not (correct, and off
// the paging loop's path — it serializes a drained cursor's terminal position).
type distinctResumeState struct {
	seen    *boundedSet[string]
	order   []string
	n       int
	charged int64
	// prev is the token this state was adopted from, dropped when this state is
	// parked under a new token. Without it the scratch would accumulate one
	// entry per page for the life of the statement; with it at most the
	// previous and current page's entries are live per operator, so a
	// continuation stays resumable until its successor is minted.
	prev int64
}

// parkDistinct stores a seen-set snapshot and returns the token naming it,
// dropping the entry the state was adopted from.
func (s *ExecutionScratch) parkDistinct(st *distinctResumeState) int64 {
	if s.distinct == nil {
		s.distinct = make(map[int64]*distinctResumeState, 2)
	}
	if st.prev != 0 {
		delete(s.distinct, st.prev)
	}
	s.nextToken++
	token := s.nextToken
	s.distinct[token] = st
	return token
}

// adoptDistinct returns the seen-set parked under token. A miss is a hard
// error: the alternative is to resume with an empty set and silently re-admit
// every duplicate already emitted.
func (s *ExecutionScratch) adoptDistinct(token int64) (*distinctResumeState, error) {
	if s == nil || s.distinct == nil {
		return nil, fmt.Errorf(
			"distinct-hash continuation names seen-set %d but this execution carries no scratch",
			token,
		)
	}
	st, ok := s.distinct[token]
	if !ok {
		return nil, fmt.Errorf(
			"distinct-hash continuation names seen-set %d, which this execution's scratch does not hold",
			token,
		)
	}
	return st, nil
}
