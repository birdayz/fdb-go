package executor

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file ports Java's recursive-union continuation machinery
// (RecursiveUnionCursor + RecordQueryRecursiveLevelUnionPlan's
// RecursiveStateManagerImpl + TempTableInsertCursor): the recursion
// STREAMS, and every emitted row carries a RecursiveCursorContinuation
// {isInitialState, activeStateContinuation, tempTable} where tempTable
// is the CURRENT SCAN buffer (the level frontier). A mid-level resume
// restores the scan buffer at the union level and the partially-filled
// INSERT buffer through the leg's own TempTableInsertCursor
// continuation (the owning-insert arm serializes its table).
//
// The proto SHAPE mirrors Java's RecordCursorProto messages, but the
// row payload inside PTempTable uses the Go-internal lossless
// continuation codec (appendContValue) — SQL statement tokens are
// ENGINE-PRIVATE per the RFC-181 C2 decision, and a Java-minted
// PTempTable's message-bytes payload fails Go's row decode LOUDLY
// rather than misreading.

// --- row / temp-table codec ---

// encodeRecursiveRow serializes a positional row as (names, slots) in
// the lossless continuation codec. Rows in recursive temp tables are
// positional by construction (the CTE legs project positional rows); a
// row without positional content cannot round-trip and errors loudly.
func encodeRecursiveRow(qr QueryResult) ([]byte, error) {
	if qr.Positional == nil {
		return nil, fmt.Errorf("recursive continuation: temp-table row carries no positional content — cannot serialize")
	}
	names := qr.Positional.TypeNames()
	namesAny := make([]any, len(names))
	for i, n := range names {
		namesAny[i] = n
	}
	buf, err := appendContValue(nil, namesAny)
	if err != nil {
		return nil, fmt.Errorf("recursive continuation: row layout: %w", err)
	}
	buf, err = appendContValue(buf, qr.Positional.Slots)
	if err != nil {
		return nil, fmt.Errorf("recursive continuation: row slots: %w", err)
	}
	return buf, nil
}

func decodeRecursiveRow(b []byte, resolve protoDescriptorResolver) (QueryResult, error) {
	namesVal, rest, err := readContValue(b)
	if err != nil {
		return QueryResult{}, fmt.Errorf("recursive continuation: row layout: %w", err)
	}
	namesAny, ok := namesVal.([]any)
	if !ok {
		return QueryResult{}, fmt.Errorf("recursive continuation: row layout is %T, want list", namesVal)
	}
	names := make([]string, len(namesAny))
	for i, v := range namesAny {
		s, sok := v.(string)
		if !sok {
			return QueryResult{}, fmt.Errorf("recursive continuation: row layout entry %d is %T, want string", i, v)
		}
		names[i] = s
	}
	slotsVal, _, err := readContValue(rest)
	if err != nil {
		return QueryResult{}, fmt.Errorf("recursive continuation: row slots: %w", err)
	}
	slotsAny, ok := slotsVal.([]any)
	if !ok {
		return QueryResult{}, fmt.Errorf("recursive continuation: row slots are %T, want list", slotsVal)
	}
	resolved, err := resolvePendingProtoValues(slotsAny, resolve)
	if err != nil {
		return QueryResult{}, err
	}
	slots, ok := resolved.([]any)
	if !ok {
		return QueryResult{}, fmt.Errorf("recursive continuation: resolved slots are %T, want list", resolved)
	}
	if len(slots) != len(names) {
		return QueryResult{}, fmt.Errorf("recursive continuation: %d slots for %d columns", len(slots), len(names))
	}
	return QueryResult{Positional: &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}}, nil
}

// encodeTempTableProto encodes an already-captured row snapshot into the
// PTempTable shape. Takes rows, not a *TempTable: the caller must capture the
// snapshot (TempTable.Snapshot) at the point that needs to be resumable —
// typically well before this runs, since both call sites reach it from a
// LAZY continuation's ToBytes (see tempTableInsertContinuation /
// recursiveUnionContinuation) that may run long after the table has moved on.
func encodeTempTableProto(rows []QueryResult) (*gen.PTempTable, error) {
	out := &gen.PTempTable{BufferItems: make([]*gen.PQueryResult, 0, len(rows))}
	for _, qr := range rows {
		b, err := encodeRecursiveRow(qr)
		if err != nil {
			return nil, err
		}
		out.BufferItems = append(out.BufferItems, &gen.PQueryResult{
			Datum: &gen.PQueryResult_Complex{Complex: b},
		})
	}
	return out, nil
}

// decodeTempTableInto re-adds a PTempTable snapshot's rows into dst.
func decodeTempTableInto(dst *TempTable, pt *gen.PTempTable, resolve protoDescriptorResolver) error {
	for i, item := range pt.GetBufferItems() {
		complexBytes := item.GetComplex()
		if complexBytes == nil {
			// A Java-minted token stores primitives/messages here; Go only
			// ever writes the Complex arm — loud, never a misread.
			return fmt.Errorf("recursive continuation: temp-table item %d carries no Go row payload (foreign token?)", i)
		}
		qr, err := decodeRecursiveRow(complexBytes, resolve)
		if err != nil {
			return err
		}
		if err := dst.Add(qr); err != nil {
			return err
		}
	}
	return nil
}

// storeDescriptorResolver resolves proto full names against the store's
// metadata for dynamic-message row slots, delegating to
// metadataMessageResolver — the WHOLE-FILE index (top-level siblings,
// nested messages, and transitive imports), not just record types and
// their nested messages: a buffered dynamic STRUCT's descriptor can be a
// top-level sibling of a record type or live in an imported file, and a
// record-type-only walk failed to resume a valid continuation with
// "message type not found". A nil store resolves nothing (loud
// downstream).
func storeDescriptorResolver(store *recordlayer.FDBRecordStore) protoDescriptorResolver {
	var inner protoDescriptorResolver
	return func(fullName string) (protoreflect.MessageDescriptor, error) {
		if store == nil {
			return nil, fmt.Errorf("recursive continuation: no store metadata to resolve message type %q", fullName)
		}
		md := store.GetRecordMetaData()
		if md == nil {
			return nil, fmt.Errorf("recursive continuation: no record metadata to resolve message type %q", fullName)
		}
		if inner == nil {
			inner = metadataMessageResolver(md)
		}
		return inner(fullName)
	}
}

// --- TempTableInsertCursor (Java cursors/TempTableInsertCursor) ---

// tempTableInsertCursor is the OWNING insert arm: each emitted row is
// added to the table, and every result's continuation snapshots the
// table alongside the child position (TempTableInsertContinuation), so
// a mid-stream resume restores the rows inserted so far. Exhaustion is
// plain exhaustion (the parent recursion snapshots the table itself).
type tempTableInsertCursor struct {
	table  *TempTable
	inner  recordlayer.RecordCursor[QueryResult]
	closed bool
}

func newTempTableInsertCursor(
	continuation []byte,
	table *TempTable,
	resolve protoDescriptorResolver,
	childFactory func(childCont []byte) (recordlayer.RecordCursor[QueryResult], error),
) (*tempTableInsertCursor, error) {
	var childCont []byte
	if len(continuation) > 0 {
		var cont gen.TempTableInsertContinuation
		if err := cont.UnmarshalVT(continuation); err != nil {
			return nil, &recordlayer.ContinuationParseError{RawBytes: continuation, Cause: err}
		}
		if cont.GetTempTable() != nil {
			if err := decodeTempTableInto(table, cont.GetTempTable(), resolve); err != nil {
				// The recursion binds this table fresh on resume, so its
				// tally is exactly the partial decode — release it (the
				// cursor whose Close would is never constructed).
				table.ReleaseCharges()
				return nil, err
			}
		}
		childCont = cont.GetChildContinuation()
	}
	inner, err := childFactory(childCont)
	if err != nil {
		return nil, err
	}
	return &tempTableInsertCursor{table: table, inner: inner}, nil
}

// tempTableInsertContinuation is LAZY, like Java's
// TempTableInsertCursor.Continuation: encoding the captured row snapshot into
// bytes happens only in ToBytes (thrown away for every row whose
// continuation the caller never serializes — the pager serializes once per
// page). What is NOT lazy, and must not be, is the SNAPSHOT itself: rows is
// captured (via TempTable.Snapshot, O(1) — see its doc comment) at wrap time,
// the moment this continuation object is constructed, not at ToBytes time.
// A live *TempTable pointer captured here instead would let an arbitrary
// number of intervening Add calls (this cursor's OWN inner keeps inserting
// into the same table for every later row of this same leg) change what a
// stale, not-yet-serialized continuation would encode — this is exactly the
// bug: "the pager serializes once per page" is caller DISCIPLINE, not a
// structural guarantee, and it breaks the moment any consumer peeks, retries,
// or holds a row's continuation across more than one OnNext call.
type tempTableInsertContinuation struct {
	rows      []QueryResult
	childCont recordlayer.RecordCursorContinuation
}

func (c *tempTableInsertContinuation) ToBytes() ([]byte, error) {
	childBytes, err := c.childCont.ToBytes()
	if err != nil {
		return nil, err
	}
	snapshot, err := encodeTempTableProto(c.rows)
	if err != nil {
		return nil, err
	}
	msg := &gen.TempTableInsertContinuation{ChildContinuation: childBytes, TempTable: snapshot}
	return msg.MarshalVT()
}

func (c *tempTableInsertContinuation) IsEnd() bool { return false }

func (c *tempTableInsertCursor) wrapContinuation(childCont recordlayer.RecordCursorContinuation) recordlayer.RecordCursorContinuation {
	return &tempTableInsertContinuation{rows: c.table.Snapshot(), childCont: childCont}
}

func (c *tempTableInsertCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	r, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	if !r.HasNext() {
		if r.GetNoNextReason().IsSourceExhausted() {
			return recordlayer.NewResultNoNext[QueryResult](
				recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
			), nil
		}
		return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), c.wrapContinuation(r.GetContinuation())), nil
	}
	v := r.GetValue()
	if err := c.table.Add(v); err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	return recordlayer.NewResultWithValue(v, c.wrapContinuation(r.GetContinuation())), nil
}

func (c *tempTableInsertCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *tempTableInsertCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*tempTableInsertCursor)(nil)

// --- RecursiveUnionCursor (Java cursors/RecursiveUnionCursor +
// RecursiveStateManagerImpl) ---

// recursiveUnionCursor streams the level-order recursion. Two temp
// tables ping-pong: the SCAN table (the "recursive union temp table" —
// the level frontier the recursive leg reads) and the INSERT table the
// active leg writes into via its owning TempTableInsertPlan. When the
// active cursor exhausts, buffers flip (the just-filled insert becomes
// the scan; the old scan is CLEARED and becomes the insert) and the
// recursive leg restarts — until the frontier is empty.
type recursiveUnionCursor struct {
	plan    *plans.RecordQueryRecursiveLevelUnionPlan
	store   *recordlayer.FDBRecordStore
	baseCtx *EvaluationContext
	props   recordlayer.ExecuteProperties
	resolve protoDescriptorResolver

	isInitialState bool
	scanTable      *TempTable // == Java's recursiveUnionTempTable
	insertTable    *TempTable
	active         recordlayer.RecordCursor[QueryResult]
	// levels is a per-cursor-instance FALLBACK level count: it always
	// increments (regardless of whether props.State is wired up) so a raw
	// ExecuteProperties without State still gets the pre-fix protection —
	// bounding runaway growth within one page/cursor lifetime. It is NOT the
	// authoritative count once props.State is available; see checkDepth.
	levels int
	closed bool
}

// maxStreamingRecursionDepth mirrors the eager path's cycle bound
// (1000 recursive legs).
const maxStreamingRecursionDepth = 1000

func newRecursiveUnionCursor(
	ctx context.Context,
	p *plans.RecordQueryRecursiveLevelUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (*recursiveUnionCursor, error) {
	c := &recursiveUnionCursor{
		plan:    p,
		store:   store,
		baseCtx: evalCtx,
		props:   props.ClearSkipAndLimit(),
		resolve: storeDescriptorResolver(store),
	}
	c.scanTable = NewTempTableWithState(props.State)
	c.insertTable = NewTempTableWithState(props.State)

	if len(continuation) == 0 {
		c.isInitialState = true
		// A nil continuation is the invocation boundary: this cursor starts a
		// FRESH invocation of p, not a resume of one already in flight (a
		// resume — paging mid-recursion — always carries a non-nil
		// continuation; see checkDepth and ResetRecursionLevel's doc
		// comments). Reset the statement-scoped level count for p so an
		// earlier invocation's levels (e.g. a prior outer row of a
		// correlated subquery wrapping this CTE) don't carry over into this
		// one.
		c.props.State.ResetRecursionLevel(p)
		active, err := c.legCursor(ctx, p.GetInitialState(), nil)
		if err != nil {
			return nil, err
		}
		c.active = active
		return c, nil
	}

	var cont gen.RecursiveCursorContinuation
	if err := cont.UnmarshalVT(continuation); err != nil {
		return nil, &recordlayer.ContinuationParseError{RawBytes: continuation, Cause: err}
	}
	c.isInitialState = cont.GetIsInitialState()
	if cont.GetTempTable() != nil {
		if err := decodeTempTableInto(c.scanTable, cont.GetTempTable(), c.resolve); err != nil {
			// The owner-releases-once model: a failed resume releases the
			// partially-decoded frontier's charges here, since Close will
			// never run.
			c.scanTable.ReleaseCharges()
			return nil, err
		}
	}
	legPlan := p.GetRecursiveState()
	if c.isInitialState {
		legPlan = p.GetInitialState()
	}
	active, err := c.legCursor(ctx, legPlan, cont.GetActiveStateContinuation())
	if err != nil {
		c.scanTable.ReleaseCharges()
		c.insertTable.ReleaseCharges()
		return nil, err
	}
	c.active = active
	return c, nil
}

// legCursor executes a leg with the CURRENT scan/insert bindings.
func (c *recursiveUnionCursor) legCursor(ctx context.Context, legPlan plans.RecordQueryPlan, cont []byte) (recordlayer.RecordCursor[QueryResult], error) {
	levelCtx := c.baseCtx.
		WithBinding(c.plan.GetTempTableScanAlias(), c.scanTable).
		WithBinding(c.plan.GetTempTableInsertAlias(), c.insertTable)
	return ExecutePlan(ctx, legPlan, c.store, levelCtx, cont, c.props)
}

// recursiveUnionContinuation is LAZY like Java's
// RecursiveUnionCursor.Continuation: encoding to bytes happens only in
// ToBytes. The scan FRONTIER (scanRows) is captured (TempTable.Snapshot, O(1))
// at wrap time, not at ToBytes time — see tempTableInsertContinuation's doc
// comment for why a live *TempTable pointer is unsafe here. It matters even
// more for this cursor: c.scanTable and c.insertTable are SWAPPED at every
// level transition (OnNext, the ping-pong), and the table object that used to
// BE scanTable is then Cleared and refilled as the new insertTable — so a
// continuation issued mid-level that still held the live *scanTable pointer
// would, once that object got recycled into next level's insert buffer,
// serialize whatever the NEXT level happened to insert instead of the
// frontier the row was actually resumable against.
type recursiveUnionContinuation struct {
	isInitialState bool
	scanRows       []QueryResult
	childCont      recordlayer.RecordCursorContinuation
}

func (c *recursiveUnionContinuation) ToBytes() ([]byte, error) {
	childBytes, err := c.childCont.ToBytes()
	if err != nil {
		return nil, err
	}
	snapshot, err := encodeTempTableProto(c.scanRows)
	if err != nil {
		return nil, err
	}
	initial := c.isInitialState
	msg := &gen.RecursiveCursorContinuation{
		IsInitialState:          &initial,
		TempTable:               snapshot,
		ActiveStateContinuation: childBytes,
	}
	return msg.MarshalVT()
}

func (c *recursiveUnionContinuation) IsEnd() bool { return false }

func (c *recursiveUnionCursor) wrapContinuation(childCont recordlayer.RecordCursorContinuation) recordlayer.RecordCursorContinuation {
	return &recursiveUnionContinuation{
		isInitialState: c.isInitialState,
		scanRows:       c.scanTable.Snapshot(),
		childCont:      childCont,
	}
}

// checkDepth increments the level count and errors with
// RecursiveCTEDepthExceededError once it exceeds maxStreamingRecursionDepth.
// It combines two counters and uses whichever is larger:
//
//   - c.levels: a per-cursor-instance counter, always incremented, that
//     alone would only bound growth WITHIN one page (the pre-fix behavior —
//     every page rebuilds a fresh *recursiveUnionCursor via
//     newRecursiveUnionCursor, so this resets to zero on every resume).
//   - props.State.IncrementRecursionLevel(c.plan): the STATEMENT-scoped count
//     (see its doc comment on recordlayer.ExecuteState) that survives the
//     per-page rebuild, so a cyclic recursion that pages mid-recursion still
//     trips the cap instead of streaming forever. This count is scoped to
//     one INVOCATION of c.plan, not the whole statement: newRecursiveUnionCursor
//     resets it (ResetRecursionLevel) whenever it starts fresh (continuation
//     == nil) rather than resuming one already in flight, so independent
//     invocations of the same plan node within a statement (e.g. once per
//     outer row of a correlated subquery) each get their own budget instead
//     of accumulating into one shared total.
//
// A nil props.State (raw ExecuteProperties, as some executor-level tests
// use) makes IncrementRecursionLevel a no-op returning 0, so c.levels alone
// governs — identical to the pre-fix per-page-only bound. Taking the max
// (rather than only the state-backed count) means the guard is never weaker
// than before this fix, only stronger when props.State is wired up.
func (c *recursiveUnionCursor) checkDepth() error {
	c.levels++
	depth := c.levels
	if stateDepth := c.props.State.IncrementRecursionLevel(c.plan); stateDepth > depth {
		depth = stateDepth
	}
	// > (not >=): recursive legs 1..1000 run, matching the eager path's
	// level indices 0..999.
	if depth > maxStreamingRecursionDepth {
		return &RecursiveCTEDepthExceededError{MaxDepth: maxStreamingRecursionDepth}
	}
	return nil
}

func (c *recursiveUnionCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
		r, err := c.active.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if r.HasNext() {
			return recordlayer.NewResultWithValue(r.GetValue(), c.wrapContinuation(r.GetContinuation())), nil
		}
		if !r.GetNoNextReason().IsSourceExhausted() {
			// Out-of-band stop (limit): checkpoint (Java wrapLastResult).
			return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), c.wrapContinuation(r.GetContinuation())), nil
		}
		// Exhausted: transition (Java notifyCursorIsExhausted +
		// canTransitionToNewStep). The just-filled INSERT becomes the
		// scan frontier; the old scan clears and becomes the insert.
		_ = c.active.Close()
		c.isInitialState = false
		c.scanTable, c.insertTable = c.insertTable, c.scanTable
		c.insertTable.Clear()
		if len(c.scanTable.GetList()) == 0 {
			return recordlayer.NewResultNoNext[QueryResult](
				recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
			), nil
		}
		if err := c.checkDepth(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		active, aerr := c.legCursor(ctx, c.plan.GetRecursiveState(), nil)
		if aerr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, aerr
		}
		c.active = active
	}
}

func (c *recursiveUnionCursor) Close() error {
	c.closed = true
	err := c.active.Close()
	c.scanTable.ReleaseCharges()
	c.insertTable.ReleaseCharges()
	return err
}

func (c *recursiveUnionCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*recursiveUnionCursor)(nil)
