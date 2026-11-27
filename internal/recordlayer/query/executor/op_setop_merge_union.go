// Portions derived from FoundationDB Record Layer (UnionCursor.java,
// MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"container/heap"
	"context"
	"fmt"

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/recordlayer"
)

func executeMergeSortUnion(
	ctx context.Context,
	p *plans.RecordQueryMergeSortUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return newEmptyCursor[QueryResult](), nil
	}

	// Java RecordQueryUnionPlanBase.executePlan: children are CURSOR FACTORIES
	// (childContinuation → executePlan(child, childContinuation, childProps)),
	// and the union continuation decodes into per-child start states. The
	// pre-A5 eager construction discarded the incoming continuation — a
	// resumed merge replayed every leg from row 0.
	factories := make([]recordlayer.CursorFactory[QueryResult], len(inners))
	for i, inner := range inners {
		inner := inner
		factories[i] = func(cont []byte) recordlayer.RecordCursor[QueryResult] {
			c, err := ExecutePlan(ctx, inner, store, evalCtx, cont, props.ClearSkipAndAdjustLimit())
			if err != nil {
				return &errResultCursor{err: err}
			}
			return c
		}
	}
	if len(factories) == 1 {
		// A 1-leg merge union cannot occur from the planner (the distinct-union
		// rule requires >= 2 legs, matching Java UnionCursor.create's >= 2
		// check); defensively hand the continuation straight to the sole leg —
		// Java's InUnion size==1 arm.
		return applySkipLimit(factories[0](continuation), props.Skip, props.ReturnedRowLimit), nil
	}
	merged, err := newMergeSortCursorFromFactories(
		factories, p.GetComparisonKeys(), p.IsReverse(), p.RemovesDuplicates(), continuation)
	if err != nil {
		return nil, err
	}
	return applySkipLimit(merged, props.Skip, props.ReturnedRowLimit), nil
}

// mergeSortChildState tracks one child of the merge-sort union — a faithful
// port of Java MergeCursorState / KeyedMergeCursorState
// (provider/foundationdb/cursors/MergeCursorState.java,
// KeyedMergeCursorState.java). cont is the child's cached resume point: it is
// initialized to the child's START state (or its decoded resume position) and
// advances ONLY when
//   - the child yields NO value (handleNextCursorResult, MergeCursorState
//     .java:58-63 — a stop's continuation IS the safe resume point), or
//   - the child's held value is CONSUMED into an emitted merge row (consume,
//     :76-81).
//
// A PEEKED-but-unconsumed row never advances cont — the emitted-row snapshot
// then re-reads that row on resume instead of silently skipping it.
type mergeSortChildState struct {
	cursor  recordlayer.RecordCursor[QueryResult]
	cont    recordlayer.RecordCursorContinuation
	result  recordlayer.RecordCursorResult[QueryResult]
	pulled  bool  // an OnNext result is cached and not yet consumed
	keyVals []any // comparison-key values of the held row (KeyedMergeCursorState.comparisonKey)

	// origIndex is this state's position in mergeSortCursor.states, assigned
	// once by mergeSortCursor's lazy init. It is the heap's tie-break: Java's
	// UnionCursor.chooseStates (:101-131) visits cursorStates in that fixed
	// list order and keeps the FIRST leg seen at a tied key, so
	// mergeSortHeap.Less must reproduce that same total order (key, then
	// origIndex) rather than leaving heap ties to container/heap's internal
	// sift order, which has no relation to leg position.
	origIndex int
}

// pull fetches the child's next result if none is cached (Java
// MergeCursorState.getOnNextFuture + handleNextCursorResult: the onNext future
// is created once and cleared only by consume, so a stopped child's cached
// result is re-served without re-pulling the cursor).
func (s *mergeSortChildState) pull(ctx context.Context, m *mergeSortCursor) error {
	if s.pulled {
		return nil
	}
	result, err := s.cursor.OnNext(ctx)
	if err != nil {
		return err
	}
	s.result = result
	s.pulled = true
	if result.HasNext() {
		kv, kerr := m.evalCompKeys(result.GetValue())
		if kerr != nil {
			return kerr
		}
		s.keyVals = kv
	} else {
		s.keyVals = nil
		// No value: the child advanced to a stop — its continuation is the
		// safe resume point (MergeCursorState.java:61).
		s.cont = result.GetContinuation()
	}
	return nil
}

// consume records that the held row was emitted as part of a merge result,
// advancing the cached continuation past it (MergeCursorState.java:76-81).
func (s *mergeSortChildState) consume() {
	s.cont = s.result.GetContinuation()
	s.pulled = false
	s.keyVals = nil
}

// mergeSortCursor merges N compatibly-ordered children — Java's UnionCursor
// (dedup=true: per-step advance-all-equal on the comparison key,
// UnionCursor.java:100-131) riding the MergeCursor state machine. dedup=false
// is the ordered UNION-ALL merge (Go's RecordQueryMergeSortUnionPlan with
// removesDuplicates=false): only the first minimum child is consumed per step,
// so cross-child ties emit every tied row.
//
// Leg selection uses a binary min-heap (mergeSortHeap), not Java's per-row
// linear rescan of every leg (UnionCursor.chooseStates, :101-131). Java can
// afford O(N) per row because its N is bounded by query shape (two-cursor
// UNION, a handful of OR branches); Go's InUnion plan turns a SQL IN list
// into one leg per value, so N can run into the hundreds or thousands and
// the per-row rescan dominates. The heap keeps selection O(log N) after an
// unavoidable O(N) initial build, while reproducing Java's result and
// tie-break exactly: see mergeSortHeap's doc comment and OnNext.
type mergeSortCursor struct {
	states   []*mergeSortChildState
	compKeys []values.Value
	// compKeyPrograms holds the comparison-key program specialized per leg row
	// shape, so a merge builds one per LEG rather than one per row.
	compKeyPrograms comparisonKeyPrograms
	reverse         bool
	dedup           bool
	closed          bool
	// lastNoNext caches a terminal result (Java MergeCursor.onNext:291-293:
	// once stopped, every later onNext returns the same result).
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]

	heap        mergeSortHeap
	initialized bool // states[*].origIndex assigned + heap built from the first pull
	// pendingAdmit is the QUEUE of legs that must be pulled and (if they
	// still have a value) pushed into the heap before the NEXT row can be
	// chosen — every leg on the first call, or the legs just consumed on
	// every call after. See OnNext's admission comment for why this must
	// happen at the START of the following call rather than eagerly at the
	// end of the round that consumed them. pullAndAdmit consumes this queue
	// leg-by-leg from the front, popping a leg only once its pull succeeds,
	// so a pull error leaves the failing leg (and everything behind it)
	// still queued here for the next call to retry — no leg is ever dropped
	// on the floor by a partial admit.
	pendingAdmit []*mergeSortChildState
	// pendingStop/pendingStopReason latch a limit stop discovered while
	// admitting a batch of freshly-pulled legs (init, or the legs consumed
	// last round) — Java's computeNextResultStates :81-84: any leg stopping
	// for a non-exhaustion reason stops the WHOLE merge before the next row
	// is chosen. What is deferred is the ADMIT itself (the pull of the legs
	// consumed at the end of round R), pushed out to the top of round R+1's
	// OnNext call instead of happening eagerly at the end of round R — see
	// pendingAdmit's comment for why. The pendingStop check that follows the
	// admit fires in that SAME round-R+1 call, immediately after — it is not
	// itself deferred to a later call — so the row round R already chose and
	// returned is unaffected; only round R+1 short-circuits into stopWith
	// instead of choosing another row.
	pendingStop       bool
	pendingStopReason recordlayer.NoNextReason
	// heapErr latches a comparison error surfaced from within
	// mergeSortHeap.Less, which cannot itself return an error (the
	// heap.Interface contract). Every call site reads it via takeHeapErr
	// immediately after the heap mutation that could have triggered it,
	// which also CLEARS it — deliberately, so the error is a one-shot signal
	// tied to that specific mutation rather than a permanent poison flag: an
	// uncleared heapErr would trip every later, otherwise-successful
	// takeHeapErr check on stale state, turning one bad comparison into a
	// cursor that errors forever even on rounds whose own comparisons are
	// fine. A retried comparison of the SAME two keys is still deterministic
	// and will set it again on its own, exactly as the replaced linear scan
	// re-errored deterministically on a repeat call.
	heapErr error
}

// takeHeapErr returns and clears m.heapErr — see its field doc for why
// clearing on read (rather than latching permanently) is deliberate.
func (m *mergeSortCursor) takeHeapErr() error {
	err := m.heapErr
	m.heapErr = nil
	return err
}

// unionChildResume adapts shared union framing to executor START, MID and END states.
type unionChildResume struct {
	continuation []byte
	exhausted    bool
}

// decodeUnionContinuation adapts recordlayer.DecodeUnionContinuation and preserves executor error types.
// Unlike the shared decoder, nil or empty input starts every child fresh.
func decodeUnionContinuation(data []byte, n int) ([]unionChildResume, error) {
	out := make([]unionChildResume, n)
	if len(data) == 0 {
		return out, nil // all children fresh (START)
	}
	states, err := recordlayer.DecodeUnionContinuation(data, n)
	if err != nil {
		switch e := err.(type) {
		case *recordlayer.RecordCoreArgumentError:
			return nil, fmt.Errorf("%s", e.Message)
		case *recordlayer.RecordCoreError:
			return nil, &recordlayer.ContinuationParseError{Message: "invalid continuation", RawBytes: data, Cause: e.Cause}
		default:
			return nil, err
		}
	}
	for i, state := range states {
		if state.IsEnd() {
			out[i].exhausted = true
			continue
		}
		out[i].continuation, err = state.ToBytes()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// newMergeSortCursorFromFactories constructs the merge over child CURSOR
// FACTORIES, decoding the incoming continuation into per-child start states —
// Java UnionCursor.createCursorStates + KeyedMergeCursorState.from: an END
// child is NEVER re-opened (an empty cursor with an END continuation stands in
// for it); a MID child resumes from its own bytes; a START child begins fresh.
func newMergeSortCursorFromFactories(
	factories []recordlayer.CursorFactory[QueryResult],
	compKeys []values.Value,
	reverse, dedup bool,
	continuation []byte,
) (*mergeSortCursor, error) {
	resumes, err := decodeUnionContinuation(continuation, len(factories))
	if err != nil {
		return nil, err
	}
	states := make([]*mergeSortChildState, len(factories))
	for i, factory := range factories {
		if resumes[i].exhausted {
			// Java MergeCursorState.from: continuation.isEnd() →
			// (RecordCursor.empty(), END) — the exhausted child stays exhausted.
			states[i] = &mergeSortChildState{cursor: newEmptyCursor[QueryResult](), cont: &recordlayer.EndContinuation{}}
			continue
		}
		var cont recordlayer.RecordCursorContinuation = &recordlayer.StartContinuation{}
		if len(resumes[i].continuation) > 0 {
			cont = recordlayer.NewBytesContinuation(resumes[i].continuation)
		}
		states[i] = &mergeSortChildState{cursor: factory(resumes[i].continuation), cont: cont}
	}
	return &mergeSortCursor{
		states:   states,
		compKeys: compKeys,
		reverse:  reverse,
		dedup:    dedup,
	}, nil
}

func (m *mergeSortCursor) IsClosed() bool { return m.closed }

// OnNext is Java MergeCursor.onNext (:288-305) with UnionCursor's
// computeNextResultStates (:74-98) and chooseStates (:101-131) reworked to
// select the minimum leg via mergeSortHeap instead of Java's per-row linear
// rescan of every leg — see mergeSortCursor's and mergeSortHeap's doc
// comments for why and for the exact-equivalence argument. Behaviorally
// this is still: admit every leg's held row once (whenAll), stop the whole
// merge if ANY leg stopped for a limit (in-band ReturnLimitReached included
// — treating it as exhaustion would silently drop that leg's remaining
// rows), otherwise emit the minimum-key row, consume the chosen legs, and
// snapshot every leg's cached continuation into the emitted row's lazy
// UnionContinuation.
func (m *mergeSortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if m.lastNoNext != nil {
		return *m.lastNoNext, nil
	}
	if m.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if err := ctx.Err(); err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}

	if !m.initialized {
		m.initialized = true
		m.heap.m = m
		for i, s := range m.states {
			s.origIndex = i
		}
		// The very first round has no choice but to look at every leg
		// (Java's initial whenAll is the same O(N) — there is no smaller
		// set of legs to consider before anything has been chosen yet).
		m.pendingAdmit = m.states
	}
	// pendingAdmit is the legs consumed last round (or, on the first call,
	// every leg). Pulling and admitting them HERE — before this round's
	// choice — rather than eagerly at the end of the round that consumed
	// them is what keeps the emitted row's snapshot correct: consume()
	// already set each chosen leg's continuation to "right after this row"
	// (MergeCursorState.java:76-81), and that must be what gets snapshotted
	// for THIS row. Pulling those legs' NEXT value before returning THIS
	// row would let a leg that turns out exhausted overwrite its
	// just-consumed continuation with an END one, and if every leg ends up
	// admitted-as-END in the same round a row is returned, the snapshot
	// would wrongly claim the whole merge is done while also carrying a
	// value — exactly the invariant NewResultWithValue rejects. Java avoids
	// this by construction: consume() clears the cached onNext future
	// (MergeCursorState.java:76-81) but does not resolve a new one; the
	// next future is only resolved by the FOLLOWING round's whenAll.
	if len(m.pendingAdmit) > 0 {
		if err := m.pullAndAdmit(ctx); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
	}
	if m.pendingStop {
		// A leg admitted at the end of the previous round (or during init)
		// stopped for a non-exhaustion reason. UnionCursor.computeNextResultStates
		// (:81-84) stops the WHOLE merge before choosing another row.
		return m.stopWith(m.pendingStopReason), nil
	}
	if m.heap.Len() == 0 {
		// Every leg exhausted: the snapshot is all-END (IsEnd() true),
		// satisfying the SourceExhausted invariant.
		return m.stopWith(recordlayer.SourceExhausted), nil
	}

	chosen, err := m.chooseMinKeyStates()
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}

	// MergeCursor.onNext (:299-301): take the first chosen leg's value
	// (origIndex-least among ties — see mergeSortHeap's doc comment),
	// consume EVERY chosen leg (the advance-all-equal dedup — one emitted
	// row per distinct comparison key, no cross-page state), snapshot, and
	// defer pulling each consumed leg's next row to the top of the
	// following OnNext call (see the pendingAdmit comment above).
	result := chosen[0].result.GetValue()
	for _, s := range chosen {
		s.consume()
	}
	m.pendingAdmit = chosen
	return recordlayer.NewResultWithValue(result, m.snapshotContinuation()), nil
}

// chooseMinKeyStates (UnionCursor.java:101-131): collect every leg holding the
// minimum comparison key (maximum for reverse — the comparator flip).
// heap[0] is that minimum by construction (mergeSortHeap.Less), so the
// first pop is free; ties are found by repeatedly comparing the new
// root against the established minimum and popping while equal. Every
// comparison happens BEFORE the corresponding pop, and any already-popped
// tied leg is pushed back on a comparison error, so an error leaves the
// heap exactly as it was on entry — matching the pre-heap linear scan,
// which never mutated m.states before returning an error. This holds for
// BOTH error sources below: the direct peek compare against minKeyVals,
// and heap.Pop's own internal sift-down (which, with 3+ legs remaining,
// compares two OTHER legs to pick the smaller child and can latch an
// error into m.heapErr with no relation to the leg heapPop just
// returned) — an error return must leave the heap holding every leg it
// held on entry, so a leg m.heapPop() already extracted from the heap
// gets pushed back right alongside any earlier ties in chosen before the
// error propagates; otherwise that leg is gone from both the heap and
// chosen the moment this call returns, orphaned exactly like the legs
// pullAndAdmit's doc comment describes.
func (m *mergeSortCursor) chooseMinKeyStates() ([]*mergeSortChildState, error) {
	minKeyVals := m.heap.items[0].keyVals
	chosen := make([]*mergeSortChildState, 0, 1)
	for m.heap.Len() > 0 {
		top := m.heap.items[0]
		if len(chosen) > 0 {
			cmp, cmpErr := m.compareKeyVals(top.keyVals, minKeyVals)
			if cmpErr != nil {
				for _, s := range chosen {
					m.heapPush(s)
				}
				// cmpErr is the error being surfaced; discard anything the
				// push-back sift latched into m.heapErr so it cannot poison
				// an unrelated later call (see takeHeapErr).
				m.heapErr = nil
				return nil, cmpErr
			}
			if cmp != 0 {
				break
			}
			if !m.dedup {
				// Ordered UNION-ALL merge: consume ONLY the first minimum
				// leg; the other tied legs re-offer their rows on later
				// steps, so every tied row is emitted. (Java's UnionCursor
				// always dedups — the ALL variant is the Go
				// merge-sort-union with removesDuplicates=false.)
				break
			}
		}
		chosen = append(chosen, m.heapPop())
		if err := m.takeHeapErr(); err != nil {
			// Same invariant as the cmpErr branch above, and for the same
			// reason: chosen (including the leg m.heapPop() just returned)
			// holds legs no longer present in m.heap, so they must go back
			// in before this error reaches the caller.
			for _, s := range chosen {
				m.heapPush(s)
			}
			m.heapErr = nil
			return nil, err
		}
	}
	return chosen, nil
}

// pullAndAdmit pulls and admits the legs queued in m.pendingAdmit, one leg at
// a time, popping a leg off the FRONT of the queue only AFTER its pull
// succeeds. This makes a mid-queue pull error non-destructive: the failing
// leg (and every leg still behind it) stays in m.pendingAdmit, so the very
// next OnNext call re-enters here and retries that SAME leg first —
// mergeSortChildState.pull is idempotent (a no-op once s.pulled is set), so
// a leg that actually succeeded before the error is never re-pulled or
// double-pushed into the heap.
//
// This replaced an all-or-nothing version that captured the whole batch into
// a local and cleared m.pendingAdmit up front, before pulling anything: a
// pull error partway through the batch then permanently orphaned every leg
// from the failure point onward — not in the heap (never pushed), not in
// pendingAdmit (already cleared), and never pulled again on any later call.
// A caller that kept driving OnNext after the error either silently lost
// rows (an orphaned leg's remaining values simply never resurfaced) or, if
// every remaining leg ended up orphaned, hit a panic out of stopWith:
// SourceExhausted requires an all-END continuation snapshot, but an orphaned
// leg's continuation was never advanced past its last successful pull, so
// the snapshot was not actually END.
//
// A leg that stopped (this call) is folded into the pending-stop bookkeeping
// instead of being scanned again next round: a leg's stop state can only
// change when it is (re-)pulled, so only the legs actually pulled THIS round
// can introduce a new stop reason — Java's getStrongestNoNextReason
// (MergeCursor.java:172-194) collapses to considering just this batch
// because the merge always stops the very round a non-exhaustion reason
// first appears (see mergeSortCursor.pendingStop's doc comment), so no
// earlier-stopped leg can still be "new" information by a later round.
func (m *mergeSortCursor) pullAndAdmit(ctx context.Context) error {
	for len(m.pendingAdmit) > 0 {
		s := m.pendingAdmit[0]
		if err := s.pull(ctx, m); err != nil {
			return err
		}
		m.pendingAdmit = m.pendingAdmit[1:]
		if s.result.HasNext() {
			m.heapPush(s)
			if err := m.takeHeapErr(); err != nil {
				return err
			}
			continue
		}
		reason := s.result.GetNoNextReason()
		if !reason.IsLimitReached() {
			continue // plain exhaustion: this leg just drops out of the merge
		}
		// getStrongestNoNextReason's combine rule, restricted to this batch
		// (see the doc comment above): the first non-exhaustion reason wins
		// unless a later one in the same batch is out-of-band, in which case
		// out-of-band always wins.
		if !m.pendingStop || reason.IsOutOfBand() {
			m.pendingStopReason = reason
			m.pendingStop = true
		}
	}
	return nil
}

// heapPush pushes s onto the heap. Any comparison error the resulting sift
// hits is latched into m.heapErr (mergeSortHeap.Less cannot return one
// itself) — callers read it via takeHeapErr immediately after.
func (m *mergeSortCursor) heapPush(s *mergeSortChildState) {
	heap.Push(&m.heap, s)
}

// heapPop pops the minimum leg. Any comparison error the resulting sift hits
// is latched into m.heapErr (mergeSortHeap.Less cannot return one itself) —
// callers read it via takeHeapErr immediately after.
func (m *mergeSortCursor) heapPop() *mergeSortChildState {
	return heap.Pop(&m.heap).(*mergeSortChildState)
}

// stopWith caches and returns the merge's terminal no-next result.
func (m *mergeSortCursor) stopWith(reason recordlayer.NoNextReason) recordlayer.RecordCursorResult[QueryResult] {
	res := recordlayer.NewResultNoNext[QueryResult](reason, m.snapshotContinuation())
	m.lastNoNext = &res
	return res
}

// evalCompKeys evaluates the comparison keys against a held row's ordinal row
// (KeyedMergeCursorState.handleNextCursorResult's comparisonKeyFunction.apply).
// Comparison keys are plan-baked field extractions; an evaluation error is a
// planner invariant violation surfaced as a loud cursor error.
func (m *mergeSortCursor) evalCompKeys(qr QueryResult) ([]any, error) {
	kv := make([]any, len(m.compKeys))
	evalKeys, inputEdges, err := m.compKeyPrograms.forRow(m.compKeys, qr.Positional)
	if err != nil {
		return nil, fmt.Errorf("merge comparison key program: %w", err)
	}
	arg, err := compKeyEvalArg(qr, inputEdges...)
	if err != nil {
		return nil, err
	}
	for i, key := range evalKeys {
		v, err := key.Evaluate(arg)
		if err != nil {
			return nil, fmt.Errorf("merge comparison key %d: %w", i, err)
		}
		kv[i] = v
	}
	return kv, nil
}

// compareKeyVals compares two evaluated comparison-key vectors in merge order
// (Java KeyComparisons.KEY_COMPARATOR with the reverse flip,
// UnionCursor.java:114). compareValues is the same per-element ordering
// authority the sort/merge paths share (Java-faithful float total order, FDB
// tuple order for bytes/UUID/bool).
func (m *mergeSortCursor) compareKeyVals(a, b []any) (int, error) {
	for i := range m.compKeys {
		cmp, err := compareValues(a[i], b[i])
		if err != nil {
			return 0, err
		}
		if cmp == 0 {
			continue
		}
		if m.reverse {
			return -cmp, nil
		}
		return cmp, nil
	}
	return 0, nil
}

// mergeSortHeap is a container/heap binary min-heap over the legs currently
// holding a comparison key, giving mergeSortCursor.OnNext O(log N) leg
// selection instead of Java UnionCursor.chooseStates's O(N) rescan of every
// leg on every emitted row (UnionCursor.java:101-131).
//
// Ordering is (comparison key via mergeSortCursor.compareKeyVals, which
// folds in the reverse flip; then origIndex) — a strict total order, so the
// classic heap invariant pins the root to a UNIQUE element: the leg with the
// smallest key and, among ties, the smallest original leg index. That is
// exactly chosen[0] from the replaced linear scan: chooseStates makes a
// single forward pass over cursorStates in list order and only replaces the
// running "chosen" set on a STRICTLY smaller key (`compare < 0`), so among
// legs tied at the minimum key the first one encountered — i.e., smallest
// index — is always chosen[0], and Java's getNextResult returns exactly
// chosenStates.get(0) (UnionCursorBase.java:70-72). Folding origIndex into
// Less reproduces that tie-break exactly rather than leaving it to
// container/heap's internal sift order, which has no relation to leg
// position and would silently pick a different (still-correct-as-a-*set*,
// but wrong-as-a-*value*) leg on ties.
//
// Less cannot return an error, but compareKeyVals can (a cross-type
// comparison-key pair across legs — a planner invariant violation): on
// error, Less latches it into m.heapErr and falls back to the origIndex
// order alone, which keeps the heap internally consistent (no panic, no
// infinite loop) until the caller reads it via takeHeapErr, which every
// production call site does immediately after the heap operation that
// could have triggered it.
//
// Note this also means the heap can SURFACE a cross-type comparison-key
// violation on a leg pair the replaced linear scan never happened to
// compare directly (chooseStates only ever compares each leg against the
// running minimum, not every pair). That is not a new bug — the keys were
// already invariant-violating — just an earlier, heap-shape-dependent
// discovery point for one that already existed.
type mergeSortHeap struct {
	items []*mergeSortChildState
	m     *mergeSortCursor
}

func (h *mergeSortHeap) Len() int { return len(h.items) }

func (h *mergeSortHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	cmp, err := h.m.compareKeyVals(a.keyVals, b.keyVals)
	if err != nil {
		if h.m.heapErr == nil {
			h.m.heapErr = err
		}
		return a.origIndex < b.origIndex
	}
	if cmp != 0 {
		return cmp < 0
	}
	return a.origIndex < b.origIndex
}

func (h *mergeSortHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *mergeSortHeap) Push(x any) { h.items = append(h.items, x.(*mergeSortChildState)) }

func (h *mergeSortHeap) Pop() any {
	old := h.items
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // drop the reference so a shrunk-but-not-reallocated backing array doesn't pin it
	h.items = old[:n-1]
	return item
}

// snapshotContinuation captures every child's CURRENT cached continuation
// (MergeCursor.getChildContinuations, called via getContinuationObject AFTER
// the chosen states were consumed): consumed children sit past their emitted
// rows, peeked-but-unconsumed children sit BEFORE their held rows, stopped
// children sit at their own stop positions.
func (m *mergeSortCursor) snapshotContinuation() *mergeSortContinuation {
	children := make([]recordlayer.RecordCursorContinuation, len(m.states))
	for i, s := range m.states {
		c := s.cont
		if c == nil {
			c = &recordlayer.StartContinuation{}
		}
		children[i] = c
	}
	return &mergeSortContinuation{children: children}
}

// mergeSortContinuation snapshots child positions for recordlayer.EncodeUnionContinuation.
// Encoding errors propagate lazily; all-END children yield END and nil bytes.
type mergeSortContinuation struct {
	children []recordlayer.RecordCursorContinuation
}

func (u *mergeSortContinuation) IsEnd() bool {
	for _, c := range u.children {
		if !c.IsEnd() {
			return false
		}
	}
	return true
}

func (u *mergeSortContinuation) ToBytes() ([]byte, error) {
	return recordlayer.EncodeUnionContinuation(u.children)
}

func (m *mergeSortCursor) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	var firstErr error
	for _, s := range m.states {
		if err := s.cursor.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
