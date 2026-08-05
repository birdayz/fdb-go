package executor

import (
	"fmt"

	"fdb.dev/pkg/recordlayer"
)

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
// THE INVARIANT THAT MAKES IT SAFE: **executing a page must be idempotent with
// respect to the scratch.** Lifetime is not the whole story — the same
// continuation bytes are not merely HELD for the statement, they are EXECUTED
// more than once. paginatingRows.fetchPage runs its whole body inside the FDB
// retry loop (runInCapturedTx -> DB.Run -> TransactCtx, fdb/database.go:323),
// which re-invokes the closure from the UNCHANGED r.continuation on any
// retryable error; the closure resets r.buf, so the failed attempt's rows are
// discarded. A scratch that let the failed attempt mutate shared state would
// therefore lose rows silently — the retry would treat the discarded attempt's
// values as already emitted. (Reproduced: a mid-drain abort on a 12-value
// fixture emitted 10, no error. The by-value encoding was immune by
// construction, because bytes are immutable.) Two rules restore it:
//
//  1. A published set is IMMUTABLE. A resumed cursor never touches the set it
//     adopted; it accumulates its own page's new keys in a private delta and
//     publishes base+delta as a NEW entry. A dying attempt then mutates nothing
//     its retry can see. The base is never COPIED per page — that would restore
//     the quadratic — it is extended in place exactly once, when rule 2 proves
//     no one can reach its earlier form again.
//
//  2. Eviction keys on ADOPT, never on PARK. Reaching token T proves the page
//     that minted T committed and was consumed, so T's predecessor is dead.
//     MINTING T proves nothing: the attempt that minted it can still fail, and
//     its retry resumes from the predecessor it was minted from. (Reproduced:
//     park-time eviction turned a routine retryable error after a completed
//     page into a hard "seen-set not held" failure.) Evicting on adopt keeps
//     the bound at a couple of live entries per operator all the same.
//
// MEMORY. A committed layer is charged ONCE, permanently, against the
// statement's ExecuteState when its keys are folded in — it really is held for
// the statement's lifetime, so releasing it at page teardown would tell the
// budget that a live set is free. Only the page's own delta is charged and
// released per page. Between pages the by-value encoding was equally
// uncharged, so this is not a regression in either direction; it is simply the
// accurate account.
//
// Concurrency: the executor is single-threaded per statement (zero goroutine
// launches in this package, pinned by package_invariant_test.go), so the maps
// need no lock, exactly as ExecuteState's counters do not.
type ExecutionScratch struct {
	nextToken int64
	distinct  map[int64]*distinctResumeState
	// pageStart is the token high-water mark when the current page began, and
	// adopted is what the page has resumed from. SweepAfterPage keeps entries
	// newer than pageStart (this page is handing them forward) and entries the
	// page adopted (its own commit can still fail retryably, and the retry
	// resumes from the continuation naming them); everything older is
	// unreachable.
	pageStart int64
	adopted   map[int64]struct{}
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

// LiveDistinctSets returns how many parked states the scratch still holds.
// Test/diagnostic accessor; production code never reads it. It is what pins the
// eviction bound — the scratch must not accumulate one entry per page.
func (s *ExecutionScratch) LiveDistinctSets() int {
	if s == nil {
		return 0
	}
	return len(s.distinct)
}

// BeginPage tells the scratch a new page's execution is starting: it snapshots
// the token high-water mark and clears the adoption record that SweepAfterPage
// consumes. Safe on a nil scratch.
func (s *ExecutionScratch) BeginPage() {
	if s == nil {
		return
	}
	s.pageStart = s.nextToken
	s.adopted = nil
}

// SweepAfterPage drops every state parked BEFORE this page began that this page
// did not adopt.
//
// The rule is sound without inspecting a single byte. A page executes from one
// continuation; every entry that continuation reaches — including entries named
// by a correlated inner's continuation, adopted lazily part-way through the
// drain — is adopted during the page. So once the drain is over, an older entry
// that was never adopted is unreachable: no continuation the statement still
// holds can name it. Entries parked BY this page are kept, because they are
// what the page is handing forward.
//
// It has to be this rule and not "keep what the surviving continuation names".
// Marking during that continuation's serialization looks equivalent and is not:
// enclosing continuation objects cache their own serialized bytes, so the final
// ToBytes need never call down into the distinct's, the mark is missed, and the
// live entry is swept. That mistake was measured — `SELECT DISTINCT v FROM t
// LIMIT 3` under a scanned-rows limit returned 2 rows and then failed with
// "seen-set 1 ... does not hold".
//
// Retry-safe: a retry re-executes the same continuation and adopts the same
// entries, and this sweep only ever drops entries no adoption reached.
//
// Safe on a nil scratch.
func (s *ExecutionScratch) SweepAfterPage() {
	if s == nil || s.distinct == nil {
		return
	}
	for token := range s.distinct {
		if token > s.pageStart {
			continue // parked by this page: it is what we hand forward
		}
		if _, keep := s.adopted[token]; keep {
			continue
		}
		delete(s.distinct, token)
	}
	s.adopted = nil
}

// seenLayer is a committed set of dedup keys, shared by every state that
// extends it. It is append-only and is extended ONLY by adoptDistinct's fold,
// which runs at the moment adoption proves the layer's earlier form is
// unreachable. Growing it in place is what keeps the whole drain linear: each
// key is folded once, never re-copied per page.
type seenLayer struct {
	m map[string]struct{}
}

func newSeenLayer() *seenLayer {
	return &seenLayer{m: make(map[string]struct{})}
}

// Contains reports whether the committed set holds key. Nil-safe: a cursor with
// no adopted layer (the scratch-less fallback) has none.
func (l *seenLayer) Contains(key string) bool {
	if l == nil {
		return false
	}
	_, ok := l.m[key]
	return ok
}

// distinctResumeState is ONE CURSOR's published dedup state: the committed set
// it started from, plus the live insertion-ordered slice of the keys it has
// sighted since.
//
// It belongs to the cursor, not to a single continuation. A cursor's
// continuations are all prefixes of the same order slice, so they share this
// one entry and are told apart by the prefix length riding in the continuation
// (stateDeltaN). That is what keeps the scratch at one entry per cursor even
// when an enclosing operator serializes a child continuation on EVERY emitted
// row (limitEnvelopeCursor does exactly that), while leaving every one of those
// continuations independently resumable.
//
// order is append-only and read through order[:n], so the prefix a continuation
// named stays an immutable snapshot however far the cursor later advances.
// foldedN is how much of it has already been merged into base, so a repeated
// adoption (an FDB retry replaying the same bytes) folds nothing twice.
type distinctResumeState struct {
	base    *seenLayer
	order   []string
	foldedN int
	// prev is the token this state's cursor adopted, dropped when a SUCCESSOR
	// is adopted (never when one is minted — see rule 2 above).
	prev int64
}

// parkDistinct publishes a cursor's state and returns the token naming it. It
// evicts NOTHING: the attempt that mints a token may still fail, and its retry
// resumes from the predecessor.
func (s *ExecutionScratch) parkDistinct(st *distinctResumeState) int64 {
	if s.distinct == nil {
		s.distinct = make(map[int64]*distinctResumeState, 4)
	}
	s.nextToken++
	token := s.nextToken
	s.distinct[token] = st
	return token
}

// adoptDistinct returns the committed set named by token, extended with the
// first n keys of that entry's delta. Adoption is the proof of consumption, so
// it is also where the predecessor — and any sibling published by a failed
// attempt from the same predecessor — is evicted.
//
// The returned layer is READ-ONLY to the caller: a resumed cursor accumulates
// into its own delta, so a failed attempt leaves nothing behind for its retry
// to trip over.
//
// A miss is a hard error: the alternative is to resume with an empty set and
// silently re-admit every duplicate already emitted.
func (s *ExecutionScratch) adoptDistinct(
	token int64,
	n int,
	state *recordlayer.ExecuteState,
) (*seenLayer, error) {
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
	if n > len(st.order) {
		return nil, fmt.Errorf(
			"distinct-hash continuation claims %d keys of seen-set %d, which holds %d",
			n, token, len(st.order),
		)
	}
	// The predecessor is now unreachable, and so is anything a FAILED attempt
	// published from that same predecessor — this adoption proves a different
	// state was the one that committed. Dropping the siblings is what lets the
	// fold below extend the shared base safely.
	if st.prev != 0 {
		delete(s.distinct, st.prev)
		for other, cand := range s.distinct {
			if other != token && cand.prev == st.prev {
				delete(s.distinct, other)
			}
		}
	}
	if n < st.foldedN {
		// A SHORTER prefix than the base already carries. The base cannot be
		// un-folded, and deduping against the longer one would drop rows the
		// continuation had not yet emitted, so this is loud rather than wrong.
		// Unreachable through the paging loop, which keeps exactly one
		// continuation per page and resumes only that one.
		return nil, fmt.Errorf(
			"distinct-hash continuation claims %d keys of seen-set %d, which has already been folded to %d",
			n, token, st.foldedN,
		)
	}
	if n > st.foldedN {
		// Fold this continuation's keys into the committed layer, ONCE. Their
		// bytes become a permanent statement charge (see MEMORY above).
		var folded int64
		for _, key := range st.order[st.foldedN:n] {
			if _, dup := st.base.m[key]; dup {
				continue
			}
			st.base.m[key] = struct{}{}
			folded += int64(len(key))
		}
		if err := state.ChargeMemory(folded); err != nil {
			return nil, err
		}
		st.foldedN = n
	}
	if s.adopted == nil {
		s.adopted = make(map[int64]struct{}, 2)
	}
	s.adopted[token] = struct{}{}
	return st.base, nil
}
