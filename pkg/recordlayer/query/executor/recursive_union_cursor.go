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

// encodeTempTableProto snapshots a temp table into the PTempTable shape.
func encodeTempTableProto(tt *TempTable) (*gen.PTempTable, error) {
	rows := tt.GetList()
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
// metadata (record types and their nested messages) for dynamic-message
// row slots. A nil store resolves nothing (loud downstream).
func storeDescriptorResolver(store *recordlayer.FDBRecordStore) protoDescriptorResolver {
	return func(fullName string) (protoreflect.MessageDescriptor, error) {
		if store == nil {
			return nil, fmt.Errorf("recursive continuation: no store metadata to resolve message type %q", fullName)
		}
		md := store.GetRecordMetaData()
		if md == nil {
			return nil, fmt.Errorf("recursive continuation: no record metadata to resolve message type %q", fullName)
		}
		for _, rt := range md.RecordTypes() {
			if rt.Descriptor == nil {
				continue
			}
			if string(rt.Descriptor.FullName()) == fullName {
				return rt.Descriptor, nil
			}
			if d := findNestedDescriptor(rt.Descriptor, fullName); d != nil {
				return d, nil
			}
		}
		return nil, fmt.Errorf("recursive continuation: message type %q not found in record metadata", fullName)
	}
}

func findNestedDescriptor(d protoreflect.MessageDescriptor, fullName string) protoreflect.MessageDescriptor {
	msgs := d.Messages()
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		if string(m.FullName()) == fullName {
			return m
		}
		if n := findNestedDescriptor(m, fullName); n != nil {
			return n
		}
	}
	return nil
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

func (c *tempTableInsertCursor) wrapContinuation(childCont recordlayer.RecordCursorContinuation) (recordlayer.RecordCursorContinuation, error) {
	childBytes, err := childCont.ToBytes()
	if err != nil {
		return nil, err
	}
	snapshot, err := encodeTempTableProto(c.table)
	if err != nil {
		return nil, err
	}
	msg := &gen.TempTableInsertContinuation{ChildContinuation: childBytes, TempTable: snapshot}
	b, err := msg.MarshalVT()
	if err != nil {
		return nil, err
	}
	return recordlayer.NewBytesContinuation(b), nil
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
		cont, cerr := c.wrapContinuation(r.GetContinuation())
		if cerr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, cerr
		}
		return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), cont), nil
	}
	v := r.GetValue()
	if err := c.table.Add(v); err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	cont, cerr := c.wrapContinuation(r.GetContinuation())
	if cerr != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, cerr
	}
	return recordlayer.NewResultWithValue(v, cont), nil
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
	levels         int
	closed         bool
}

// maxRecursionDepth mirrors the eager path's cycle bound.
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
			return nil, err
		}
	}
	legPlan := p.GetRecursiveState()
	if c.isInitialState {
		legPlan = p.GetInitialState()
	}
	active, err := c.legCursor(ctx, legPlan, cont.GetActiveStateContinuation())
	if err != nil {
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

func (c *recursiveUnionCursor) wrapContinuation(childCont recordlayer.RecordCursorContinuation) (recordlayer.RecordCursorContinuation, error) {
	childBytes, err := childCont.ToBytes()
	if err != nil {
		return nil, err
	}
	snapshot, err := encodeTempTableProto(c.scanTable)
	if err != nil {
		return nil, err
	}
	initial := c.isInitialState
	msg := &gen.RecursiveCursorContinuation{
		IsInitialState:          &initial,
		TempTable:               snapshot,
		ActiveStateContinuation: childBytes,
	}
	b, err := msg.MarshalVT()
	if err != nil {
		return nil, err
	}
	return recordlayer.NewBytesContinuation(b), nil
}

func (c *recursiveUnionCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
		r, err := c.active.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if r.HasNext() {
			cont, cerr := c.wrapContinuation(r.GetContinuation())
			if cerr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, cerr
			}
			return recordlayer.NewResultWithValue(r.GetValue(), cont), nil
		}
		if !r.GetNoNextReason().IsSourceExhausted() {
			// Out-of-band stop (limit): checkpoint (Java wrapLastResult).
			cont, cerr := c.wrapContinuation(r.GetContinuation())
			if cerr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, cerr
			}
			return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), cont), nil
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
		c.levels++
		if c.levels >= maxStreamingRecursionDepth {
			return recordlayer.RecordCursorResult[QueryResult]{}, &RecursiveCTEDepthExceededError{MaxDepth: maxStreamingRecursionDepth}
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
