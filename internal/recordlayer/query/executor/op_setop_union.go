// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// UnionCursor.java, MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func executeUnion(
	ctx context.Context,
	p *plans.RecordQueryUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}
	// Each branch is a lazy CursorFactory resumable from its OWN continuation,
	// folded through recordlayer.ConcatCursors — Java's ConcatCursor with the
	// wire-compatible branch-tagged ConcatContinuation proto (an N-way union
	// is a right-nested chain of binary concats). Before RFC-180 (A1) the
	// incoming continuation was silently DISCARDED and every child started at
	// nil, so a mid-union page break replayed the union from row 0 on resume:
	// an outer aggregate that correctly restores its own partial state then
	// re-consumed the whole union per page (SUM over a grouped UNION ALL
	// returned 3×13=39 for 13 — yamsql union_aggregate_java).
	//
	// Every leg flows the union's one exact row — the translator aligned the
	// legs onto it and the plan's constructor asserted it — so a leg's rows
	// are concatenated as they are, never re-typed to another leg's names
	// (RFC-242). A buffered fallback used to exist here for legs whose column
	// names could not be read off the plan; with the names stated by every
	// plan's type there is nothing to discover, and that path (which could not
	// resume) is gone with it.
	branchFactory := func(i int) recordlayer.CursorFactory[QueryResult] {
		inner := inners[i]
		return func(cont []byte) recordlayer.RecordCursor[QueryResult] {
			c, err := ExecutePlan(ctx, inner, store, evalCtx, cont, props.ClearSkipAndAdjustLimit())
			if err != nil {
				return &errResultCursor{err: fmt.Errorf("union branch %d: %w", i, err)}
			}
			return c
		}
	}
	factories := make([]recordlayer.CursorFactory[QueryResult], len(inners))
	for i := range inners {
		factories[i] = branchFactory(i)
	}
	return applySkipLimit(concatFactories(factories, continuation), props.Skip, props.ReturnedRowLimit), nil
}

func executeUnorderedUnion(
	ctx context.Context,
	p *plans.RecordQueryUnorderedUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inners := p.GetInners()
	if len(inners) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}
	if len(inners) == 1 {
		c, err := ExecutePlan(ctx, inners[0], store, evalCtx, continuation, props.ClearSkipAndAdjustLimit())
		if err != nil {
			return nil, err
		}
		return applySkipLimit(c, props.Skip, props.ReturnedRowLimit), nil
	}
	// Java UnorderedUnionCursor: per-child slots in UnionCursorContinuation,
	// order unspecified (a deterministic serial order is a legal
	// realization), a limit-stopped child does not stop the union — the
	// remaining children keep emitting — and the union's terminal carries
	// the strongest child reason with every child's resume slot. This
	// replaces the RFC-180 A2 loud decline (which itself replaced feeding
	// the parent token raw to every child).
	resume, derr := decodeUnionContinuation(continuation, len(inners))
	if derr != nil {
		return nil, derr
	}
	// Every leg flows the union's one exact row: the translator aligned the
	// legs onto it and the plan's constructor asserted it, so a leg's rows
	// carry the names every other leg's rows carry and nothing here re-types
	// them (RFC-242). Java's UnorderedUnionCursor likewise concatenates the
	// children's results unchanged.
	childProps := props.ClearSkipAndAdjustLimit()
	u := &unorderedUnionCursor{
		children:  make([]recordlayer.RecordCursor[QueryResult], len(inners)),
		states:    make([]recordlayer.RecordCursorContinuation, len(inners)),
		reasons:   make([]recordlayer.NoNextReason, len(inners)),
		stopped:   make([]bool, len(inners)),
		exhausted: make([]bool, len(inners)),
	}
	for i, inner := range inners {
		if resume[i].exhausted {
			u.exhausted[i] = true
			u.states[i] = &recordlayer.EndContinuation{}
			continue
		}
		c, err := ExecutePlan(ctx, inner, store, evalCtx, resume[i].continuation, childProps)
		if err != nil {
			_ = u.Close()
			return nil, err
		}
		u.children[i] = c
		if len(resume[i].continuation) > 0 {
			u.states[i] = recordlayer.NewBytesContinuation(resume[i].continuation)
		}
	}
	return applySkipLimit(u, props.Skip, props.ReturnedRowLimit), nil
}

// unorderedUnionCursor is the serial realization of Java's
// UnorderedUnionCursor: children emit in deterministic index order (Java's
// order is explicitly unspecified), a child's out-of-band stop parks that
// child and the union keeps emitting from the rest (a shared scan/time
// limiter parks them all in short order), and the terminal result carries the
// STRONGEST stopped-child reason with a per-child UnionContinuation slot —
// exhausted children marked, stopped children at their positions. Every
// emitted row's continuation snapshots all children's current states.
type unorderedUnionCursor struct {
	children   []recordlayer.RecordCursor[QueryResult] // nil = exhausted at resume
	states     []recordlayer.RecordCursorContinuation  // last-known per child; nil = START
	reasons    []recordlayer.NoNextReason              // valid where stopped
	stopped    []bool
	exhausted  []bool
	idx        int
	closed     bool
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (u *unorderedUnionCursor) snapshotContinuation() recordlayer.RecordCursorContinuation {
	children := make([]recordlayer.RecordCursorContinuation, len(u.states))
	for i, c := range u.states {
		if c == nil {
			// A not-yet-pulled child is at START (Java's START_PROTO); a
			// raw nil interface would panic the encoder's IsEnd probe.
			children[i] = &recordlayer.StartContinuation{}
			continue
		}
		children[i] = c
	}
	return &mergeSortContinuation{children: children}
}

func (u *unorderedUnionCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if u.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if u.lastNoNext != nil {
		return *u.lastNoNext, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		// Find the next live child from the current position.
		live := -1
		for off := 0; off < len(u.children); off++ {
			i := (u.idx + off) % len(u.children)
			if u.children[i] != nil && !u.exhausted[i] && !u.stopped[i] {
				live = i
				break
			}
		}
		if live < 0 {
			// Terminal: all children exhausted → genuine end; any stopped
			// child → strongest reason + per-child resume slots.
			anyStopped := false
			reason := recordlayer.SourceExhausted
			found := false
			for i := range u.children {
				if !u.stopped[i] {
					continue
				}
				anyStopped = true
				if !found || u.reasons[i].IsOutOfBand() || reason.IsSourceExhausted() {
					reason = u.reasons[i]
					found = true
				}
			}
			if !anyStopped {
				return recordlayer.NewResultNoNext[QueryResult](
					recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
				), nil
			}
			res := recordlayer.NewResultNoNext[QueryResult](reason, u.snapshotContinuation())
			u.lastNoNext = &res
			return res, nil
		}
		u.idx = live
		result, err := u.children[live].OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if result.HasNext() {
			u.states[live] = result.GetContinuation()
			return recordlayer.NewResultWithValue(result.GetValue(), u.snapshotContinuation()), nil
		}
		reason := result.GetNoNextReason()
		if reason == recordlayer.SourceExhausted {
			u.exhausted[live] = true
			u.states[live] = &recordlayer.EndContinuation{}
		} else {
			u.stopped[live] = true
			u.reasons[live] = reason
			u.states[live] = result.GetContinuation()
		}
		u.idx = (live + 1) % len(u.children)
	}
}

func (u *unorderedUnionCursor) Close() error {
	if u.closed {
		return nil
	}
	u.closed = true
	var firstErr error
	for _, c := range u.children {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (u *unorderedUnionCursor) IsClosed() bool { return u.closed }

// concatFactories folds N cursor factories into a right-nested chain of binary
// recordlayer.ConcatCursors (Java's ConcatCursor with the branch-tagged
// ConcatContinuation proto), so the concat resumes the branch the continuation
// names. The single concat fold — both executeUnion (UNION ALL) and
// executeInUnion's order-free arm delegate here.
func concatFactories(factories []recordlayer.CursorFactory[QueryResult], continuation []byte) recordlayer.RecordCursor[QueryResult] {
	if len(factories) == 1 {
		return factories[0](continuation)
	}
	tail := factories[len(factories)-1]
	for i := len(factories) - 2; i >= 1; i-- {
		fi, next := factories[i], tail
		tail = func(cont []byte) recordlayer.RecordCursor[QueryResult] {
			return recordlayer.ConcatCursors(fi, next, cont)
		}
	}
	return recordlayer.ConcatCursors(factories[0], tail, continuation)
}
