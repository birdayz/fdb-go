// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// executeExplode mirrors Java RecordQueryExplodePlan.executePlan: the
// collection re-evaluates from the bindings and the cursor is
// RecordCursor.fromList(list, continuation) — resume is a list position.
func executeExplode(
	p *plans.RecordQueryExplodePlan,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	cv := p.GetCollectionValue()
	if cv == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	result, err := cv.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return applySkipLimit(recordlayer.Empty[QueryResult](), props.Skip, props.ReturnedRowLimit), nil
	}
	// The WITH-ORDINALITY box's ordinal output type (`[_0,_1]`), recovered
	// once from the plan's result type so every emitted row carries a
	// PositionalRow of that schema. Nil unless with-ordinality.
	var ordType *values.RecordType
	var elementHandle values.ExactTypeHandle
	if p.IsWithOrdinality() {
		if rt, ok := p.GetResultType().(*values.RecordType); ok {
			ordType = rt
		}
	} else {
		// The result Value already owns the frozen element handle. Recover it
		// once, not by re-snapshotting an ordinary Type for every array element.
		elementHandle, err = values.ExactTypeForValue(p.GetResultValue())
		if err != nil {
			return nil, err
		}
	}
	list, ok := result.([]any)
	if !ok {
		// A non-list scalar yields a single row. With ordinality it gets the
		// first ordinal (1, the SQL standard's position of the sole element, or
		// 0 for a zero-based explode).
		if p.IsWithOrdinality() {
			return applySkipLimit(
				recordlayer.FromListWithContinuation([]QueryResult{explodeOrdinalityResult(ordType, result, p.FirstOrdinal())}, continuation),
				props.Skip, props.ReturnedRowLimit,
			), nil
		}
		return applySkipLimit(
			recordlayer.FromListWithContinuation([]QueryResult{{Positional: explodeElementRow(result, elementHandle)}}, continuation),
			props.Skip, props.ReturnedRowLimit,
		), nil
	}
	items := make([]QueryResult, len(list))
	for i, elem := range list {
		if p.IsWithOrdinality() {
			// WITH ORDINALITY: each element becomes a 2-field anonymous record
			// {_0: element, _1: first+i}. The ordinal is the element's position in
			// THIS array, from 1 (or 0 when zero-based), assigned over the whole
			// list before the continuation and skip/limit apply, so a resume never
			// renumbers (the cursor re-runs per outer row, so the counter resets
			// per outer binding). Mirrors RecordQueryExplodePlan.executePlan's
			// `firstOrdinal + i`.
			items[i] = explodeOrdinalityResult(ordType, elem, p.FirstOrdinal()+i)
			continue
		}
		row, rowErr := explodePlanElementRow(p, elementHandle, elem, i)
		if rowErr != nil {
			return nil, rowErr
		}
		items[i] = QueryResult{Positional: row}
	}
	return applySkipLimit(recordlayer.FromListWithContinuation(items, continuation), props.Skip, props.ReturnedRowLimit), nil
}

// explodePlanElementRow materializes an element against the frozen plan type.
// A literal constructor's fields must still match that type: broader storage
// admission must not hide a mutation of the source value graph. Its own stamped descriptor
// supplies a fast path; independent equivalent descriptors use ordinary storage
// admission, just as Java can evaluate one plan under different TypeRepositories.
func explodePlanElementRow(p *plans.RecordQueryExplodePlan, elementHandle values.ExactTypeHandle, elem any, elementIndex int) (*PositionalRow, error) {
	if p == nil {
		return explodeElementRow(elem, elementHandle), nil
	}
	message, isMessage := elem.(proto.Message)
	if !isMessage || message == nil || isNilProtoMessage(message) {
		return explodeElementRow(elem, elementHandle), nil
	}
	array, isArrayConstructor := p.GetCollectionValue().(*values.ArrayConstructorValue)
	if !isArrayConstructor || elementIndex < 0 || elementIndex >= len(array.Elements) {
		return explodeElementRow(elem, elementHandle), nil
	}
	constructor, isRecordConstructor := array.Elements[elementIndex].(*values.RecordConstructorValue)
	declared, isDeclaredRecord := values.SharedExactType(elementHandle).(*values.RecordType)
	if !isRecordConstructor || !isDeclaredRecord || declared == nil {
		return explodeElementRow(elem, elementHandle), nil
	}
	// A present constructor is non-null even when the array admits NULL
	// elements. Only root nullability is reconciled; field drift stays loud.
	if !values.WithNullability(constructor.Type(), true).Equals(values.WithNullability(declared, true)) {
		return nil, layoutBindingError(values.LayoutTypeMismatch, "explode source constructor changed its frozen element type")
	}
	if constructor.MessageDescriptor() == nil || constructor.MessageDescriptor() != message.ProtoReflect().Descriptor() {
		return explodeElementRow(elem, elementHandle), nil
	}
	row := protoToPositional(message)
	if row == nil || len(row.Slots) != len(declared.Fields) {
		return explodeElementRow(elem, elementHandle), nil
	}
	row.Type = declared
	return row, nil
}

// explodeOrdinalityResult builds a WITH-ORDINALITY box output row: a
// PositionalRow of the box's `[_0,_1]` schema (element at slot 0, 1-based
// ordinal at slot 1), so a downstream reference over the box (a pushed-down
// WHERE predicate on the AS element / AT ordinal, evaluated by the
// PredicatesFilter over the Explode) resolves by ORDINAL.
func explodeOrdinalityResult(ordType *values.RecordType, element any, ordinal int) QueryResult {
	if ordType == nil {
		ordType = positionalTypeFromNames([]string{"_0", "_1"})
	}
	pos := NewPositionalRow(ordType)
	pos.Set(0, element)
	pos.Set(1, int64(ordinal))
	return QueryResult{Positional: pos}
}

// scalarPositionalRow wraps a BARE scalar UNNEST element (RFC-142: `t.arr AS x`
// flows a raw int64, not a row) as a 1-slot PositionalRow named `_0` so it flows in
// the ordinal model. The FlatMap binds QOV(alias) to it and concatenates the leg
// into the merged row; a downstream read of the element resolves against slot 0.
func scalarPositionalRow(v any) *PositionalRow {
	return scalarPositionalRowOfType(v, values.UnknownType)
}

func scalarPositionalRowOfType(v any, typ values.Type) *PositionalRow {
	if typ == nil {
		typ = values.UnknownType
	}
	return &PositionalRow{
		Type:          &values.RecordType{Fields: []values.Field{{Name: "_0", FieldType: typ, Ordinal: 0}}},
		Slots:         []any{v},
		transportKind: values.OrdinalCarrierScalar,
	}
}

// explodeElementRow wraps one Explode/Stream element as a PositionalRow: a
// ROW-shaped element (a map[string]any for a constant/VALUES record, or a
// proto.Message for a repeated stored MESSAGE field) becomes a multi-column
// row laid out in the element type's
// DECLARED field order — the order the plan-time ordinals were baked against.
// A baked ordinal reads by POSITION, so the row MUST match declared order, not
// an alphabetical (or map-iteration) order, or a reference baked at ordinal i
// silently reads a different field. Any other (scalar) element wraps in the
// 1-slot `_0` bare-scalar shape. elemType is the array element type (declared
// RecordType recovered from its exact handle); nil/non-record falls back to
// sorted names (best-effort — a baked
// ordinal against an unknown declared order cannot be made sound here).
func explodeElementRow(elem any, elementHandle values.ExactTypeHandle) *PositionalRow {
	elemType := values.SharedExactType(elementHandle)
	m, ok := elem.(map[string]any)
	if ok {
		if rt, isRT := elemType.(*values.RecordType); isRT && len(rt.Fields) > 0 {
			slots := make([]any, len(rt.Fields))
			for i, f := range rt.Fields {
				slots[i] = mapFieldFold(m, f.Name)
			}
			return &PositionalRow{Type: rt, Slots: slots}
		}
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		slots := make([]any, len(names))
		for i, n := range names {
			slots[i] = m[n]
		}
		return &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}
	}

	// ProtoFieldToRowValue preserves a repeated MESSAGE element as a
	// proto.Message. Treating it as a scalar would wrap ELEM in `_0`, while the
	// Explode plan's exact carrier is the ELEM record itself. Materialize it in
	// descriptor order using the same record-row authority as a stored scan.
	// The field reader owns descriptor compatibility, including nullable array
	// wrappers and logical nullability that protobuf presence cannot express.
	// Incompatible declarations keep the descriptor-derived carrier and fail
	// normal layout attachment rather than being silently stamped.
	if message, isMessage := elem.(proto.Message); isMessage && message != nil && !isNilProtoMessage(message) {
		row := protoToPositional(message)
		declared, isRecord := elemType.(*values.RecordType)
		if row == nil || row.Type == nil || !isRecord || declared == nil {
			return row
		}
		if values.ProtoRecordDescriptorCompatible(message.ProtoReflect().Descriptor(), elementHandle) {
			row.Type = declared
		}
		return row
	}

	return scalarPositionalRowOfType(elem, elemType)
}

// mapFieldFold looks up a record field's value in a name-keyed element map,
// case-insensitively (the element map may be keyed by the raw or upper-cased
// field name); an absent field is a NULL slot.
func mapFieldFold(m map[string]any, field string) any {
	if v, ok := m[field]; ok {
		return v
	}
	for k, v := range m {
		if unicode15.EqualFold(k, field) {
			return v
		}
	}
	return nil
}

// arrayElementType returns the declared element type of an array-typed value —
// the order plan-time ordinals were baked against — or nil when unknown.
func arrayElementType(v values.Value) values.Type {
	if v == nil {
		return nil
	}
	if at, ok := v.Type().(*values.ArrayType); ok {
		return at.ElementType
	}
	return nil
}

// resultFromValue wraps a single Value's binding-context evaluation into a
// Positional QueryResult: a RecordConstructor evaluates
// field-by-field into dense ordinal slots (a duplicate output name keeps both
// slots), any other (scalar) value wraps in a 1-slot `_0` row. Used by the
// default-on-empty producers (FirstOrDefault / DefaultOnEmpty).
func resultFromValue(v values.Value, rowCtx *values.RowEvalContext) (QueryResult, error) {
	if rc, ok := v.(*values.RecordConstructorValue); ok {
		resultType, err := exactRuntimeRecordType(
			"record-constructor result", rc.Type(), len(rc.Fields))
		if err != nil {
			return QueryResult{}, err
		}
		slots := make([]any, len(rc.Fields))
		for i, f := range rc.Fields {
			fv, err := f.Value.Evaluate(rowCtx)
			if err != nil {
				return QueryResult{}, recordlayer.TranslateTextCmpError(err)
			}
			slots[i] = fv
		}
		return QueryResult{Positional: &PositionalRow{Type: resultType, Slots: slots}}, nil
	}
	val, err := v.Evaluate(rowCtx)
	if err != nil {
		return QueryResult{}, recordlayer.TranslateTextCmpError(err)
	}
	return QueryResult{Positional: scalarPositionalRowOfType(val, v.Type())}, nil
}

// isBareScalarRow distinguishes a scalar transport envelope from a genuine
// record, including an anonymous record whose sole field is titled _0. The
// selected layout is authoritative; without one the producer's explicit
// transport provenance supplies the kind. Java QuantifiedObjectValue.eval
// likewise selects record versus datum from the result kind, never a title.
func isBareScalarRow(pos *PositionalRow) (bool, error) {
	if pos == nil {
		return false, nil
	}
	if pos.transportKind != values.OrdinalCarrierInvalid && pos.transportKind != values.OrdinalCarrierRecord && pos.transportKind != values.OrdinalCarrierScalar {
		return false, layoutBindingError(values.LayoutCarrierMismatch, "invalid positional transport kind")
	}
	kind := pos.OrdinalRowKind()
	if pos.Layout != nil {
		kind = pos.Layout.CarrierKind()
		if kind != values.OrdinalCarrierRecord && kind != values.OrdinalCarrierScalar {
			return false, layoutBindingError(values.LayoutCarrierMismatch, "invalid selected carrier kind")
		}
		if kind != pos.OrdinalRowKind() {
			return false, layoutBindingError(values.LayoutCarrierMismatch, "selected carrier kind disagrees with transport provenance")
		}
	}
	if kind != values.OrdinalCarrierScalar {
		return false, nil
	}
	if pos.Type == nil || len(pos.Type.Fields) != 1 || len(pos.Slots) != 1 {
		return false, layoutBindingError(values.LayoutRuntimeShape, "scalar transport requires exactly one typed slot")
	}
	return true, nil
}

func executeValues(
	p *plans.RecordQueryValuesPlan,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	cols := p.GetColumns()
	resultType, err := exactRuntimeRecordType(
		"RecordQueryValuesPlan result", p.GetResultType(), len(cols))
	if err != nil {
		return nil, err
	}
	slots := make([]any, len(cols))
	for i, col := range cols {
		v, err := col.Evaluate(evalCtx)
		if err != nil {
			return nil, recordlayer.TranslateTextCmpError(err)
		}
		slots[i] = v
	}
	// The plan's result Value is the output descriptor authority. Re-deriving
	// names from Value.Name here used "constant" while the plan declared the
	// projection spelling (for example "42"), and positionalTypeFromNames also
	// erased every exact field type to Unknown. Keep the Value-derived shape
	// intact here as well. This changes no continuation or storage bytes.
	pos := &PositionalRow{Type: resultType, Slots: slots}
	// Like Java's RecordQueryExplodePlan, apply request skip/limit outside the
	// continuation-resumed list, including when a parent delegates directly.
	cursor := recordlayer.FromListWithContinuation([]QueryResult{{Positional: pos}}, continuation)
	return applySkipLimit(cursor, props.Skip, props.ReturnedRowLimit), nil
}

// exactRuntimeRecordType snapshots the declared row type before a producer
// evaluates any slot. Runtime rows must carry the same exact descriptor that
// admitted their plan/value: synthesizing a name-only RecordType at execution
// loses field types and can make two type-incompatible branches appear alike.
func exactRuntimeRecordType(owner string, declared values.Type, width int) (*values.RecordType, error) {
	exact, err := values.SnapshotExactType(declared)
	if err != nil {
		return nil, fmt.Errorf("executor: %s type: %w", owner, err)
	}
	recordType, ok := exact.Type().(*values.RecordType)
	if !ok || recordType == nil {
		return nil, fmt.Errorf("executor: %s type is %T, want exact record", owner, exact.Type())
	}
	if len(recordType.Fields) != width {
		return nil, fmt.Errorf(
			"executor: %s width is %d, but producer evaluates %d slots",
			owner, len(recordType.Fields), width)
	}
	return recordType, nil
}
