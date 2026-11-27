// Portions derived from FoundationDB Record Layer (AggregateCursor.java,
// StreamGrouping.java, FlatMapPipelinedCursor.java,
// NumericAggregationValue.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/recordlayer"
)

// ---------------------------------------------------------------------------
// nljCursor — streaming nested-loop join
// ---------------------------------------------------------------------------

// nljCursor implements RecordCursor[QueryResult] for nested-loop joins.
// Loads the inner side once, then streams outer rows one-by-one.
type nljCursor struct {
	outerInner recordlayer.RecordCursor[QueryResult]
	innerRows  []QueryResult
	joinType   plans.JoinType
	outerAlias values.CorrelationIdentifier
	innerAlias values.CorrelationIdentifier
	preds      []predicates.QueryPredicate
	evalCtx    *EvaluationContext

	// hashIndex is a hash index on inner rows keyed by the equijoin
	// inner operand's evaluated value. When non-nil, inner row lookup is
	// O(1) per outer row instead of O(N). Built by newNLJCursor when it
	// detects a single-equality equijoin over two baked leg references.
	hashIndex    map[any][]int // join-key → indices into innerRows
	hashOuterVal values.Value  // outer-side baked operand, probes the index

	currentOuter          *QueryResult
	innerIdx              int
	innerCandidateIndices []int // nil means candidate position is the innerRows index
	innerCandidateCount   int
	outerMatched          bool
	outerExhausted        bool
	closed                bool
	// chargedBytes: what this cursor holds against the statement budget
	// (materialized inner rows + hash index), released once on Close —
	// the inner side is rebuilt and re-charged each page (live-bytes model).
	chargedBytes int64

	// Continuation state (the FlatMapPipelinedCursor pattern flatMapCursor
	// also ports): priorOuterCont is the outer position that re-produces
	// the CURRENT outer row on resume, lastOuterCont the advanced position
	// that produces the next one. The resume* fields hold a decoded page
	// continuation, applied when the first outer row arrives.
	priorOuterCont recordlayer.RecordCursorContinuation
	lastOuterCont  recordlayer.RecordCursorContinuation
	// lastNoNext replays a prior out-of-band stop on a contract-violating
	// re-call (Java FlatMapPipelinedCursor:123-125 caches every no-next
	// result) — without it a re-call re-pulls the outer and skews the
	// reason/continuation pair.
	lastNoNext         *recordlayer.RecordCursorResult[QueryResult]
	resumePending      bool
	resumeInnerIdx     int
	resumeOuterMatched bool
	resumeCheck        []byte
	resumeInnerPK      []byte

	// FULL OUTER JOIN drain state. matchedInner[i] is set when
	// innerRows[i] passes the join predicates against any outer row;
	// after the outer side is exhausted, the drain phase emits every
	// unmatched inner row NULL-padded on the left (symmetric to the
	// LEFT-OUTER unmatched-outer emission). Allocated only for
	// JoinFullOuter — other join types pay nothing. This state lives only
	// in memory and is NOT serialized into the continuation, so the
	// FULL-outer NLJ must complete within a single transaction;
	// executeNestedLoopJoin clears the outer's time/row limits for FULL to
	// guarantee that (see the comment there).
	matchedInner []bool
	drainIdx     int

	// st is the statement memory budget (RFC-130). buildErr captures a budget
	// breach during hash-index construction (newNLJCursor cannot return an
	// error) and is surfaced on the first OnNext, so the join fails fast rather
	// than silently dropping the index.
	st       *recordlayer.ExecuteState
	buildErr error

	// build is the join's ordinal-build state (an *ordinalJoinBuild), probed
	// ONCE at construction from the plan's result value (nil = un-gated merge:
	// the emitted row is mergeRows' leg-concat positional row). When enabled,
	// every emitted row carries the positional merged row evaluated from the RC
	// with per-leg bindings. Gates ALL ordinal-build work below via
	// build.enabled().
	build *ordinalJoinBuild
	// innerAdapted is the FIXED inner-rows slice adapted once at construction
	// (parallel to innerRows); outerAdapted is the current outer row adapted
	// once per outer-row advance. Together they make the per-candidate-pair
	// cost one small twoLegBinder — no re-adaptation, no map (per-pair adapter
	// work would be a structural O(rows²) perf regression).
	innerAdapted []values.OrdinalRow
	outerAdapted values.OrdinalRow
	// outerCorr/innerCorr are the leg correlation identifiers, resolved once.
	outerCorr, innerCorr values.CorrelationIdentifier
}

func newNLJCursor(
	outer recordlayer.RecordCursor[QueryResult],
	innerRows []QueryResult,
	joinType plans.JoinType,
	outerAlias, innerAlias values.CorrelationIdentifier,
	preds []predicates.QueryPredicate,
	resultValue values.Value,
	outputLayout values.OrdinalLayout,
	outputSourceOrigins map[values.CorrelationIdentifier]outputSourceOrigin,
	evalCtx *EvaluationContext,
	st *recordlayer.ExecuteState,
) (*nljCursor, error) {
	for source, origin := range outputSourceOrigins {
		if origin.topLegAlias != outerAlias && origin.topLegAlias != innerAlias {
			return nil, layoutBindingError(values.CorrelationForeignValue,
				fmt.Sprintf("retained output source %s names a foreign NLJ origin", source))
		}
	}
	build, err := newOrdinalJoinBuildWithOutputLayout(
		resultValue, preds, outputLayout, outputSourceOrigins,
		[]values.CorrelationIdentifier{outerAlias, innerAlias},
	)
	if err != nil {
		return nil, err
	}
	if build != nil {
		build.Clock = evalCtx
	}
	if build.enabled() {
		switch joinType {
		case plans.JoinLeftOuter:
			if err := build.configureNullSupplying(innerAlias); err != nil {
				return nil, err
			}
		case plans.JoinFullOuter:
			if err := build.configureNullSupplying(outerAlias, innerAlias); err != nil {
				return nil, err
			}
		}
	}
	c := &nljCursor{
		outerInner: outer,
		innerRows:  innerRows,
		joinType:   joinType,
		outerAlias: outerAlias,
		innerAlias: innerAlias,
		preds:      preds,
		evalCtx:    evalCtx,
		st:         st,
		build:      build,
	}
	// The plan hands its leg identities over TYPED, so the cursor holds the
	// identifiers the planner chose rather than ones it manufactured from their
	// text. This used to mint both here, which meant every leg identity on the
	// merge path was decided at the plan boundary by the consumer: an exact
	// comparison downstream cannot rule out a forgery its own mint constructed.
	c.outerCorr = outerAlias
	c.innerCorr = innerAlias
	if build.enabled() {
		// Adapt the FIXED inner side once — never per pair, which would turn
		// every candidate-pair comparison into a re-adaptation and make the
		// join O(rows²).
		innerType := build.legType(c.innerCorr)
		c.innerAdapted = make([]values.OrdinalRow, len(innerRows))
		for i := range innerRows {
			row, aerr := adaptLegPositional(innerRows[i], innerType, c.innerCorr)
			if aerr != nil {
				return nil, aerr
			}
			c.innerAdapted[i] = row
		}
	}
	if joinType == plans.JoinFullOuter {
		c.matchedInner = make([]bool, len(innerRows))
	}
	c.tryBuildHashIndex(innerAlias)
	return c, nil
}

// pairBinder builds the per-candidate-pair leg binder from PRE-adapted leg
// rows. A nil OrdinalRow is the deliberately-NULL leg (LEFT/FULL padding —
// the binder returns (nil, true): a leg bound to nil evaluates as NULL, never
// an error). Only called when build is enabled.
func (c *nljCursor) pairBinder(outer, inner values.OrdinalRow) *twoLegBinder {
	return &twoLegBinder{
		outerID: c.outerCorr, innerID: c.innerCorr,
		outer: outer, inner: inner,
		outerType: c.build.legValueType(c.outerCorr), innerType: c.build.legValueType(c.innerCorr),
		base: correlationBase(c.evalCtx),
	}
}

// adaptOuter adapts a just-advanced outer row ONCE (shared by every candidate
// pair and the unmatched-outer emission for this outer row). No-op for
// un-gated (build-disabled) cursors.
func (c *nljCursor) adaptOuter(outerRow QueryResult) error {
	if !c.build.enabled() {
		return nil
	}
	row, err := adaptLegPositional(outerRow, c.build.legType(c.outerCorr), c.outerCorr)
	if err != nil {
		return err
	}
	c.outerAdapted = row
	return nil
}

// innerLegRow returns inner row i as the leg-local OrdinalRow the equijoin
// operand evaluates against: the pre-adapted leg row for an ordinal-build
// cursor, else the row's own positional (the un-gated merge concatenates leg
// rows verbatim, so a leg window over the merged row reads the same slots).
func (c *nljCursor) innerLegRow(i int) values.OrdinalRow {
	if c.innerAdapted != nil {
		return c.innerAdapted[i]
	}
	return c.innerRows[i].Positional
}

// innerCandidateIndex maps the current candidate position to innerRows.
// A nil index slice is an identity view used by ordinary linear scans and
// degraded hash probes, avoiding an O(len(innerRows)) identity allocation.
func (c *nljCursor) innerCandidateIndex(position int) int {
	if c.innerCandidateIndices == nil {
		return position
	}
	return c.innerCandidateIndices[position]
}

func (c *nljCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf("cursor is closed")
	}
	// RFC-130: a memory-budget breach during hash-index construction is
	// surfaced here on the first pull (newNLJCursor cannot return an error).
	if c.buildErr != nil {
		err := c.buildErr
		c.buildErr = nil
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if c.currentOuter == nil {
			if c.outerExhausted {
				return c.drainFullOuterNext(ctx)
			}
			stopRes, hasStop, err := c.advanceOuter(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			if hasStop {
				return stopRes, nil
			}
			if c.outerExhausted {
				continue
			}
		}

		for c.innerIdx < c.innerCandidateCount {
			if err := ctx.Err(); err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			idx := c.innerCandidateIndex(c.innerIdx)
			innerRow := c.innerRows[idx]
			c.innerIdx++
			combined := mergeRows(*c.currentOuter, innerRow, c.outerCorr, c.innerCorr)
			// For an ordinal-build cursor the predicate row context carries
			// the per-leg bindings from the PRE-adapted legs (nil binder =
			// today's path bit-identically); the same binder builds the
			// positional row on a pass.
			var pair *twoLegBinder
			if c.build.enabled() {
				pair = c.pairBinder(c.outerAdapted, c.innerAdapted[idx])
			}
			passes, perr := passesJoinPredicatesLegs(combined, c.preds, c.evalCtx, pair)
			if perr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, perr
			}
			if !passes {
				continue
			}
			if c.matchedInner != nil {
				c.matchedInner[idx] = true
			}
			c.outerMatched = true
			switch c.joinType {
			case plans.JoinInner, plans.JoinLeftOuter, plans.JoinCross, plans.JoinFullOuter:
				// A fused-top merge (a positional-merge RC) reshapes the
				// merged legs into the RC's OUTPUT columns; the build RC
				// produces that ordinal output directly (evaluateBound). A
				// plain merge keeps mergeRows' leg-concat positional row.
				if pair != nil {
					pos, berr := c.build.evaluateBound(pair)
					if berr != nil {
						return recordlayer.RecordCursorResult[QueryResult]{}, berr
					}
					combined.Positional = pos
				}
				return recordlayer.NewResultWithValue(combined, c.midInnerContinuation()), nil
			}
			if c.currentOuter == nil {
				break
			}
		}

		if c.currentOuter == nil {
			continue
		}

		outerRow := *c.currentOuter
		matched := c.outerMatched
		c.currentOuter = nil

		if !matched && (c.joinType == plans.JoinLeftOuter || c.joinType == plans.JoinFullOuter) {
			return c.emitUnmatchedOuter(outerRow)
		}
	}
}

func (c *nljCursor) drainFullOuterNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	// FULL OUTER drain: after the outer side is exhausted,
	// emit every inner row that matched no outer row,
	// NULL-padded on the left (outer columns absent →
	// resolve to NULL downstream). Symmetric to the
	// LEFT-OUTER unmatched-outer emission below.
	if c.joinType == plans.JoinFullOuter {
		for c.drainIdx < len(c.innerRows) {
			if err := ctx.Err(); err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			i := c.drainIdx
			c.drainIdx++
			if c.matchedInner[i] {
				continue
			}
			// FULL drain: an unmatched INNER row, the OUTER leg is the
			// NULL leg. The ordinal null-pad row comes from the build RC
			// (evaluateBound with a nil outer leg — QOV(outer)→nil, the
			// outer slots fall out NULL) when active; otherwise the
			// inner's own positional qualified under its alias so a
			// downstream qualified ref to the inner side resolves, and
			// the absent outer side reads NULL.
			qr := QueryResult{Record: c.innerRows[i].Record, PrimaryKey: c.innerRows[i].PrimaryKey}
			qr.Positional = qualifyOuterPositional(c.innerRows[i].Positional, c.innerCorr)
			if c.build.enabled() {
				pos, berr := c.build.evaluateBound(c.pairBinder(nil, c.innerAdapted[i]))
				if berr != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, berr
				}
				qr.Positional = pos
			}
			// FULL OUTER never resumes (the drain bitmap has no
			// serialized form; limits are cleared so the whole
			// join runs in one page) — the marker continuation
			// rejects loudly on decode.
			return recordlayer.NewResultWithValue(qr, &nljContinuation{fullOuter: true}), nil
		}
	}
	return recordlayer.NewResultNoNext[QueryResult](
		recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
	), nil
}

func (c *nljCursor) advanceOuter(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], bool, error) {
	result, err := c.outerInner.OnNext(ctx)
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, false, err
	}
	if !result.HasNext() {
		reason := result.GetNoNextReason()
		if reason == recordlayer.SourceExhausted {
			// Loop back to the top so the FULL OUTER drain
			// (above) runs before reporting exhaustion.
			c.outerExhausted = true
			return recordlayer.RecordCursorResult[QueryResult]{}, false, nil
		}
		// Out-of-band outer stop between rows: wrap the outer's
		// position in the NLJ envelope so every NLJ continuation
		// decodes uniformly (a raw pass-through would be ambiguous
		// against the mid-inner format on resume).
		w := &nljContinuation{outerCont: result.GetContinuation()}
		if c.joinType == plans.JoinFullOuter {
			w = &nljContinuation{fullOuter: true}
		} else if c.resumePending {
			// The outer stopped BEFORE re-yielding the resumed row:
			// the armed mid-inner state must ride the new
			// continuation or the next resume replays that outer
			// row from inner index 0 (duplicate pairs).
			w.hasInner = true
			w.innerIdx = c.resumeInnerIdx
			w.outerMatched = c.resumeOuterMatched
			w.checkValue = c.resumeCheck
			w.innerPK = c.resumeInnerPK
		}
		res := recordlayer.NewResultNoNext[QueryResult](reason, w)
		c.lastNoNext = &res
		return res, true, nil
	}
	outerRow := result.GetValue()
	c.priorOuterCont = c.lastOuterCont
	c.lastOuterCont = result.GetContinuation()
	c.currentOuter = &outerRow
	c.innerIdx = 0
	c.innerCandidateIndices = nil
	c.innerCandidateCount = 0
	c.outerMatched = false
	// Adapt the new outer leg ONCE for all its candidate
	// pairs (no-op for un-gated cursors).
	if err := c.adaptOuter(outerRow); err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, false, err
	}

	// Hash probe: resolve inner row candidates for this outer row by
	// evaluating the OUTER operand against the outer leg row — the
	// same resolution the predicate re-check performs per pair.
	if c.hashIndex == nil {
		c.innerCandidateCount = len(c.innerRows)
	} else if outerRow.Positional != nil {
		outerLeg := c.outerAdapted
		if outerLeg == nil {
			outerLeg = outerRow.Positional
		}
		if key, ok := evalLegHashKey(c.hashOuterVal, c.outerCorr, outerLeg); ok {
			c.innerCandidateIndices = c.hashIndex[key]
			c.innerCandidateCount = len(c.innerCandidateIndices)
		} else {
			// A probe that cannot resolve — or whose key has no
			// hashable promotion-stable canonical form (NaN, a
			// cross-typed []byte) — degrades this outer row to an
			// identity view over every inner row: the per-pair
			// predicate evaluation is the semantics of record and
			// will surface any real failure.
			c.innerCandidateCount = len(c.innerRows)
		}
	}
	// A decoded page continuation resumes THIS outer row mid-inner:
	// verify the row is the one the continuation was taken over
	// (Java's check value), then restore the inner position and the
	// matched flag. The candidate view was recomputed above
	// deterministically (same innerRows order, same probe), so the
	// restored index addresses the same candidate list. The saved
	// inner PK then verifies the ordinal or repositions it
	// (repositionInner) — key-positional like Java's inner-cursor
	// continuation; only a deleted saved row degrades to the
	// documented ordinal residue.
	if c.resumePending {
		if len(c.resumeCheck) > 0 && outerRow.PrimaryKey != nil &&
			!bytes.Equal(outerRow.PrimaryKey.Pack(), c.resumeCheck) {
			// Not the saved outer row (data changed between
			// transactions — e.g. a row inserted before it): run
			// THIS row's inner from its fresh-reset start and keep
			// the state ARMED for the saved row when it re-appears.
			// Java nulls initialInnerContinuation ONLY in the match
			// branch (FlatMapPipelinedCursor:216-219); discarding on
			// the first mismatch would re-emit the saved row's
			// already-delivered pairs when it arrives later
			// (duplicate rows). Never an error either way.
		} else {
			c.resumePending = false
			c.innerIdx = c.repositionInner(c.resumeInnerIdx, c.resumeInnerPK)
			c.outerMatched = c.resumeOuterMatched
		}
	}
	return recordlayer.RecordCursorResult[QueryResult]{}, false, nil
}

func (c *nljCursor) emitUnmatchedOuter(outerRow QueryResult) (recordlayer.RecordCursorResult[QueryResult], error) {
	// Unmatched outer (LEFT/FULL): the INNER leg is the NULL leg. The
	// ordinal null-pad row comes from the build RC (evaluateBound with
	// a nil inner leg — QOV(inner)→nil, the inner slots fall out NULL,
	// never an error) when active; otherwise the outer's own
	// positional qualified under its alias, the absent inner side NULL.
	qr := QueryResult{Record: outerRow.Record, PrimaryKey: outerRow.PrimaryKey}
	qr.Positional = qualifyOuterPositional(outerRow.Positional, c.outerCorr)
	if c.build.enabled() {
		pos, berr := c.build.evaluateBound(c.pairBinder(c.outerAdapted, nil))
		if berr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, berr
		}
		qr.Positional = pos
	}
	if c.joinType == plans.JoinFullOuter {
		// See midInnerContinuation: no FULL continuation resumes.
		return recordlayer.NewResultWithValue(qr, &nljContinuation{fullOuter: true}), nil
	}
	// Last emission for this outer row: resume advances to the
	// next outer (no inner state, no check value — Java writes
	// only the advanced outer position in this branch).
	return recordlayer.NewResultWithValue(qr, &nljContinuation{outerCont: c.lastOuterCont}), nil
}

func (c *nljCursor) Close() error {
	if !c.closed && c.chargedBytes > 0 {
		c.st.ReleaseMemory(c.chargedBytes)
		c.chargedBytes = 0
	}
	c.closed = true
	return c.outerInner.Close()
}

func (c *nljCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*nljCursor)(nil)
