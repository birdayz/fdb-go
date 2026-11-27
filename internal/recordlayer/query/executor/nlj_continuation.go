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
	"fmt"

	cursorpb "fdb.dev/internal/pb/cursor"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// midInnerContinuation snapshots the page continuation for a pair emission:
// the PRIOR outer position (re-produces the current outer row on resume), the
// current outer's PK as the check value, and the already-advanced inner
// position with the matched flag. Fields are copied at emit time — later
// cursor advances do not mutate an issued continuation.
func (c *nljCursor) midInnerContinuation() recordlayer.RecordCursorContinuation {
	// FULL OUTER accumulates the cross-outer matchedInner bitmap, which has
	// no serialized form — a resumed page would rebuild it zeroed and the
	// drain phase would re-pad already-matched inner rows (wrong rows). A
	// mid-stream FULL continuation (e.g. under LIMIT) therefore declines on
	// resume, like the drain-phase marker.
	if c.joinType == plans.JoinFullOuter {
		return &nljContinuation{fullOuter: true}
	}
	w := &nljContinuation{
		outerCont:    c.priorOuterCont,
		hasInner:     true,
		innerIdx:     c.innerIdx,
		outerMatched: c.outerMatched,
	}
	if c.currentOuter != nil && c.currentOuter.PrimaryKey != nil {
		w.checkValue = c.currentOuter.PrimaryKey.Pack()
	}
	// The just-emitted inner row (innerIdx already advanced past it): its PK
	// keys the resume position against inner-side drift.
	if idx, ok := c.lastEmittedInnerIndex(); ok && c.innerRows[idx].PrimaryKey != nil {
		w.innerPK = c.innerRows[idx].PrimaryKey.Pack()
	}
	return w
}

// nljContinuation lazily encodes the nested-loop-join page continuation at
// ToBytes() time using the FlatMapContinuation message — the NLJ is a flat
// map over (outer row → matching inner rows), so it reuses the same
// prior-outer / check-value / inner-position mechanics flatMapCursor ports
// from Java's FlatMapPipelinedCursor. The inner position is a tuple-packed
// (innerIdx, outerMatched, fullDrain) triple: the inner side re-materializes
// deterministically on resume, so an index is the complete inner state. A
// between-outer stop carries only the advanced outer position. Replaces the
// retired fake one-byte marker, whose bytes reached the outer child raw on
// resume and could be swallowed by a key-value cursor's raw-suffix fallback —
// a silently wrong scan position.
type nljContinuation struct {
	outerCont    recordlayer.RecordCursorContinuation // prior (mid-inner) or advanced (between-outer); nil = scan start
	checkValue   []byte                               // current outer PK (mid-inner only)
	hasInner     bool
	innerIdx     int
	outerMatched bool
	fullOuter    bool // FULL OUTER marker — every FULL emission; rejects loudly on decode
	// innerPK: the last-emitted inner row's packed primary key. The ordinal
	// inner position is a SNAPSHOT index; the PK makes the resume
	// key-positional like Java's inner-cursor continuation — a re-collected
	// inner that shifted (row inserted/removed before the position) is
	// repositioned by PK search, with the ordinal as the last-resort
	// fallback. Empty for computed inner rows without a PK.
	innerPK []byte
}

func (w *nljContinuation) IsEnd() bool { return false }

// The inner payload's third tuple slot is a KIND enum: 0 = normal mid-inner
// state, 1 = FULL OUTER (declines on resume), 2 = the outer child provided NO
// resume position (declines on resume — without this marker an advanced-outer
// envelope over a continuation-less outer, e.g. a FirstOrDefault single-result
// cursor, would marshal to ZERO bytes, which every consumer misreads: the
// relational driver as exhaustion, a fresh executor call as a full restart).
const (
	nljKindMidInner    = 0
	nljKindFullOuter   = 1
	nljKindUnresumable = 2
)

func (w *nljContinuation) ToBytes() ([]byte, error) {
	fmc := &cursorpb.FlatMapContinuation{CheckValue: w.checkValue}
	if w.outerCont != nil && !w.outerCont.IsEnd() {
		b, err := w.outerCont.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("nested loop join continuation outer: %w", err)
		}
		fmc.OuterContinuation = b
	}
	switch {
	case w.fullOuter:
		fmc.InnerContinuation = tuple.Tuple{int64(0), int64(0), int64(nljKindFullOuter)}.Pack()
	case w.hasInner:
		matched := int64(0)
		if w.outerMatched {
			matched = 1
		}
		fmc.InnerContinuation = tuple.Tuple{int64(w.innerIdx), matched, int64(nljKindMidInner), w.innerPK}.Pack()
	case len(fmc.OuterContinuation) == 0 && len(fmc.CheckValue) == 0:
		// Advanced-outer envelope with nothing to restore: never emit an
		// empty token — mark the position unresumable so a resume declines
		// loudly instead of silently restarting or ending the statement.
		fmc.InnerContinuation = tuple.Tuple{int64(0), int64(0), int64(nljKindUnresumable)}.Pack()
	}
	return proto.Marshal(fmc)
}

// lastEmittedInnerIndex returns the innerRows index of the row the cursor just
// emitted by mapping the already-advanced candidate position through the
// current candidate view.
func (c *nljCursor) lastEmittedInnerIndex() (int, bool) {
	position := c.innerIdx - 1
	if position < 0 || position >= c.innerCandidateCount {
		return 0, false
	}
	return c.innerCandidateIndex(position), true
}

// repositionInner turns the saved inner position into an index over the
// re-collected inner. Fast path: the ordinal still points just past the saved
// PK — indices are stable. Otherwise the inner shifted between transactions;
// search for the PK over the recomputed candidate view and resume AFTER it,
// exactly what Java's key-positional inner-cursor continuation yields. A
// vanished PK (row deleted) keeps the ordinal — the documented best-effort
// residue, no worse than the pure-ordinal model.
func (c *nljCursor) repositionInner(savedIdx int, savedPK []byte) int {
	if len(savedPK) == 0 || savedIdx <= 0 {
		return savedIdx
	}
	rowPK := func(pos int) []byte {
		if pos < 0 || pos >= c.innerCandidateCount {
			return nil
		}
		idx := c.innerCandidateIndex(pos)
		if c.innerRows[idx].PrimaryKey == nil {
			return nil
		}
		return c.innerRows[idx].PrimaryKey.Pack()
	}
	if pk := rowPK(savedIdx - 1); pk != nil && bytes.Equal(pk, savedPK) {
		return savedIdx
	}
	for pos := 0; pos < c.innerCandidateCount; pos++ {
		if pk := rowPK(pos); pk != nil && bytes.Equal(pk, savedPK) {
			return pos + 1
		}
	}
	return savedIdx
}

// nljResumeState is the decoded mid-inner half of an NLJ page continuation.
type nljResumeState struct {
	innerIdx     int
	outerMatched bool
	check        []byte
	innerPK      []byte
}

// decodeNLJContinuation parses NLJ page-continuation bytes (the
// nljContinuation encoding). Empty input means a fresh scan. Anything that is
// not this binary's format — including the retired one-byte fake marker —
// declines loudly: forwarding unrecognized bytes to the outer child risks a
// key-value cursor's raw-suffix fallback silently accepting them as a scan
// position (wrong rows). The FULL OUTER drain marker also declines (that
// phase's bitmap has no serialized form and never legitimately paginates).
func decodeNLJContinuation(continuation []byte) (outerContinuation []byte, resume *nljResumeState, err error) {
	if len(continuation) == 0 {
		return nil, nil, nil
	}
	fmc := &cursorpb.FlatMapContinuation{}
	if uerr := recordlayer.UnmarshalAsJava(continuation, fmc); uerr != nil {
		return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join (unrecognized continuation bytes)"}
	}
	if len(fmc.GetOuterContinuation()) == 0 && len(fmc.GetInnerContinuation()) == 0 && len(fmc.GetCheckValue()) == 0 {
		return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join (empty continuation payload)"}
	}
	if len(fmc.GetInnerContinuation()) == 0 {
		if len(fmc.GetOuterContinuation()) == 0 {
			// A check-value-only token is unreachable from this encoder
			// (the check rides only with mid-inner state); reject rather
			// than misread it as a fresh scan.
			return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join (check value without positions)"}
		}
		return fmc.GetOuterContinuation(), nil, nil
	}
	tup, terr := tuple.Unpack(fmc.GetInnerContinuation())
	if terr != nil || len(tup) < 3 || len(tup) > 4 {
		return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join (malformed inner position payload)"}
	}
	idx, iok := tup[0].(int64)
	matched, mok := tup[1].(int64)
	kind, kok := tup[2].(int64)
	if !iok || !mok || !kok || idx < 0 {
		return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join (malformed inner position payload)"}
	}
	var innerPK []byte
	if len(tup) == 4 && tup[3] != nil {
		pk, pkOK := tup[3].([]byte)
		if !pkOK {
			return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join (malformed inner position payload)"}
		}
		innerPK = pk
	}
	switch kind {
	case nljKindMidInner:
	case nljKindFullOuter:
		return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join FULL OUTER (does not resume)"}
	case nljKindUnresumable:
		return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join (outer child provides no resume position)"}
	default:
		return nil, nil, &UnsupportedContinuationError{Shape: "nested loop join (malformed inner position payload)"}
	}
	return fmc.GetOuterContinuation(), &nljResumeState{
		innerIdx:     int(idx),
		outerMatched: matched != 0,
		check:        fmc.GetCheckValue(),
		innerPK:      innerPK,
	}, nil
}

// armResume wires a decoded page continuation into the cursor: the mid-inner
// state (nil = fresh scan or between-outer resume) and the saved outer
// position. Seeding lastOuterCont matters even without mid-inner state: the
// first outer advance copies it into priorOuterCont — the position AT the
// resumed row — which every mid-inner continuation taken on this page pairs
// with its check value. Without the seed, a token taken from a RESUMED page
// encodes an EMPTY outer position (restart from scratch) and the next resume
// silently replays the outer prefix (duplicate rows). The flatMap resume
// seeds identically.
func (c *nljCursor) armResume(outerContinuation []byte, rs *nljResumeState) {
	if len(outerContinuation) > 0 {
		c.lastOuterCont = recordlayer.NewBytesContinuation(outerContinuation)
	}
	if rs == nil {
		return
	}
	c.resumePending = true
	c.resumeInnerIdx = rs.innerIdx
	c.resumeOuterMatched = rs.outerMatched
	c.resumeCheck = rs.check
	c.resumeInnerPK = rs.innerPK
}
