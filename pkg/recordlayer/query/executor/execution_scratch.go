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
// RETIREMENT FOLLOWS NAMEABILITY. An entry must live exactly as long as some
// continuation the statement still holds can name its token — no longer, and
// not one moment less. Three earlier designs keyed retirement on a CURSOR
// LIFECYCLE EVENT instead, and each was wrong for its own reason. They are
// recorded because the wrong answer is the intuitive one:
//
//   - on PARK: a retry resumes the predecessor the failed attempt was minted
//     from, so minting cannot retire it.
//   - on a page-local MARK of the surviving continuation: enclosing
//     continuation objects cache their own bytes, so the final ToBytes need
//     never call down and the mark is missed. It silently dropped rows.
//   - on EXHAUSTION: an enclosing operator can retain a child continuation
//     OBJECT past the child's end and serialize it later —
//     aggregateCursor.emitFinal emits its final row carrying a retained earlier
//     inner position (streaming_cursors.go:393-401). A token minted before the
//     inner ended is still named after it.
//
// Object-graph reachability from the surviving continuation is what this
// WANTED to be, and the code forbids it: sixteen sites wrap a child by
// serializing it eagerly into a plain BytesContinuation (limitEnvelopeCursor,
// executor.go:2009, is the one that matters here), so the child object is
// dropped and no walk — however careful about byte caching — can see through
// it. Bytes are the only medium that survives, so ADOPTION is the only ground
// truth available: a page adopts exactly the entries its own continuation
// named. SweepAfterPage is built on that, at the cost of one page of lag, plus
// the exact case that needs no lag (an exhausted page names nothing at all).
//
// Residency is then bounded by that sweep plus two structural rules:
//
//  1. An entry belongs to a CURSOR, not to a continuation (the prefix length
//     rides the continuation instead), so an operator that serializes a child
//     continuation on EVERY emitted row adds one entry, not one per row.
//  2. Adoption evicts the predecessor and any sibling a failed attempt
//     published from it, so a paging chain keeps two.
//
// MEMORY. A parked entry holds its byte weight against the statement budget for
// exactly as long as it lives: the producing cursor hands its page charge over
// at park time instead of releasing it at close, and retirement releases it.
// That is what makes an accumulating shape LOUD — the earlier design released
// at cursor close, so leaked entries were invisible to the budget and grew in
// silence.
//
// Concurrency: the executor is single-threaded per statement (zero goroutine
// launches in this package, pinned by package_invariant_test.go), so the maps
// need no lock, exactly as ExecuteState's counters do not.
type ExecutionScratch struct {
	nextToken int64
	distinct  map[int64]*distinctResumeState
	// pageStart is the token high-water mark when the current page began, and
	// adopted is what the page resumed from. See SweepAfterPage.
	pageStart int64
	adopted   map[int64]struct{}
	// state is the statement's memory budget, so a parked entry's bytes stay
	// charged for exactly as long as the entry lives. Set on first park.
	state *recordlayer.ExecuteState
}

// BeginPage tells the scratch a page's execution is starting: it snapshots the
// token high-water mark and clears the adoption record. Safe on a nil scratch.
func (s *ExecutionScratch) BeginPage() {
	if s == nil {
		return
	}
	s.pageStart = s.nextToken
	s.adopted = nil
}

// SweepAfterPage retires the entries the completed page has made unreachable.
//
// RETIREMENT FOLLOWS NAMEABILITY, and the only thing that can name a token is a
// byte string the statement still holds. The statement holds exactly one — the
// page's surviving continuation — so:
//
//   - exhausted: nothing is resumable at all, so NOTHING is nameable. Every
//     entry retires. This is what bounds a single-page statement, including one
//     whose correlated inner ran thousands of times.
//   - otherwise: an entry parked BEFORE this page began and not adopted DURING
//     it is unreachable, because a page adopts exactly the entries its own
//     continuation named. Entries parked by this page are kept: which of them
//     the surviving continuation names is not knowable yet, and the next page's
//     adoptions are what settle it.
//
// Deliberately NOT object-graph reachability from the surviving continuation,
// which is the shape this wanted to be: enclosing operators serialize their
// child eagerly and hand back a plain BytesContinuation (limitEnvelopeCursor,
// executor.go:2009, plus fifteen other sites), so the child object is dropped
// and no walk can see through it. Adoption is the only ground truth that
// survives that, at the cost of one page of lag.
//
// Retiring on a CURSOR lifecycle event instead — park, close, exhaustion — is
// the mistake this replaced three times over. Exhaustion in particular does not
// imply un-nameability: aggregateCursor.emitFinal emits its final row carrying
// a RETAINED earlier inner continuation (streaming_cursors.go:393-401), so a
// token minted before the inner exhausted is still named after it.
//
// Safe on a nil scratch.
func (s *ExecutionScratch) SweepAfterPage(exhausted bool) {
	if s == nil || s.distinct == nil {
		return
	}
	for token, st := range s.distinct {
		if !exhausted {
			if token > s.pageStart {
				continue
			}
			if _, keep := s.adopted[token]; keep {
				continue
			}
		}
		s.releaseEntry(st)
		delete(s.distinct, token)
	}
	s.adopted = nil
}

// releaseEntry returns a retired entry's bytes to the statement budget.
func (s *ExecutionScratch) releaseEntry(st *distinctResumeState) {
	if st.charged > 0 {
		s.state.ReleaseMemory(st.charged)
		st.charged = 0
	}
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
	// charged is the entry's byte weight, held against the statement budget
	// for exactly as long as the entry lives. The producing cursor hands its
	// page charge over at park time rather than releasing it at close, so an
	// entry that outlives its cursor is VISIBLE to the budget: an accumulating
	// shape fails loudly on the budget instead of growing silently.
	charged int64
}

// parkDistinct publishes a cursor's state and returns the token naming it. It
// evicts NOTHING: the attempt that mints a token may still fail, and its retry
// resumes from the predecessor.
func (s *ExecutionScratch) parkDistinct(st *distinctResumeState, state *recordlayer.ExecuteState) int64 {
	s.state = state
	if s.distinct == nil {
		s.distinct = make(map[int64]*distinctResumeState, 4)
	}
	s.nextToken++
	token := s.nextToken
	s.distinct[token] = st
	return token
}

// discardDistinct drops an entry whose owning cursor has proved it unreachable.
// See distinctHashCursor.Close for the proof obligation the caller carries.
func (s *ExecutionScratch) discardDistinct(token int64) {
	if s == nil || s.distinct == nil {
		return
	}
	if st, ok := s.distinct[token]; ok {
		s.releaseEntry(st)
	}
	delete(s.distinct, token)
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
		// Fold this continuation's keys into the committed layer, ONCE. No
		// charge here: these bytes were charged when the producing cursor
		// parked them, and the entry has held that charge ever since. Charging
		// again would double-count every key on every page.
		for _, key := range st.order[st.foldedN:n] {
			if _, dup := st.base.m[key]; dup {
				continue
			}
			st.base.m[key] = struct{}{}
		}
		st.foldedN = n
	}
	// Record the adoption: this is the ground truth SweepAfterPage retires on —
	// a page adopts exactly the entries its own continuation named.
	if s.adopted == nil {
		s.adopted = make(map[int64]struct{}, 2)
	}
	s.adopted[token] = struct{}{}
	return st.base, nil
}
