// Portions derived from FoundationDB Record Layer (StreamGrouping.java),
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"bytes"
	"fmt"
	"math"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The streaming aggregate's partial state in Java's own payload
// (StreamGrouping.getPartialAggregationResult): the grouping key is the
// grouping record serialized as a message of the type repository's descriptor
// for it, and each aggregate writes one AccumulatorState typed by its physical
// operator (NumericAccumulator, CountValue.SumAccumulator, ArrayAccumulator).
//
// Java writes no state for an accumulator that has seen no value, which leaves
// the next aggregate's state in its slot and fails the resume (upstream #4573).
// Go writes an empty AccumulatorState there, keeping every aggregate in its
// own slot, and keeps the group when every accumulator is still empty, where
// Java drops the whole partial.

// aggregateLane is the width suffix of Java's physical operator (_I, _L, _F,
// _D), the operand's static type; an operand without one (a hand-built plan)
// takes LONG for an all-integer stream and DOUBLE otherwise.
func aggregateLane(agg expressions.AggregateSpec, allInt bool) values.TypeCode {
	switch agg.OperandIntType {
	case values.TypeCodeInt, values.TypeCodeLong, values.TypeCodeFloat, values.TypeCodeDouble:
		return agg.OperandIntType
	}
	if allInt {
		return values.TypeCodeLong
	}
	return values.TypeCodeDouble
}

func typedState(lane values.TypeCode, v any) (*gen.OneOfTypedState, error) {
	switch lane {
	case values.TypeCodeInt:
		n, ok := asInt64(v)
		if !ok || n < math.MinInt32 || n > math.MaxInt32 {
			return nil, fmt.Errorf("aggregate state %v (%T) is not an INT", v, v)
		}
		return &gen.OneOfTypedState{State: &gen.OneOfTypedState_Int32State{Int32State: int32(n)}}, nil
	case values.TypeCodeLong:
		n, ok := asInt64(v)
		if !ok {
			return nil, fmt.Errorf("aggregate state %v (%T) is not a LONG", v, v)
		}
		return &gen.OneOfTypedState{State: &gen.OneOfTypedState_Int64State{Int64State: n}}, nil
	case values.TypeCodeFloat:
		f, ok := asFloat32(v)
		if !ok {
			return nil, fmt.Errorf("aggregate state %v (%T) is not a FLOAT", v, v)
		}
		return &gen.OneOfTypedState{State: &gen.OneOfTypedState_FloatState{FloatState: f}}, nil
	default:
		f, ok := asFloat64(v)
		if !ok {
			return nil, fmt.Errorf("aggregate state %v (%T) is not a DOUBLE", v, v)
		}
		return &gen.OneOfTypedState{State: &gen.OneOfTypedState_DoubleState{DoubleState: f}}, nil
	}
}

// laneValue reads a typed state of lane into the row domain (INT and LONG as
// int64, FLOAT and DOUBLE as float64).
func laneValue(lane values.TypeCode, s *gen.OneOfTypedState) (any, error) {
	switch v := s.GetState().(type) {
	case *gen.OneOfTypedState_Int32State:
		if lane == values.TypeCodeInt {
			return int64(v.Int32State), nil
		}
	case *gen.OneOfTypedState_Int64State:
		if lane == values.TypeCodeLong {
			return v.Int64State, nil
		}
	case *gen.OneOfTypedState_FloatState:
		if lane == values.TypeCodeFloat {
			return float64(v.FloatState), nil
		}
	case *gen.OneOfTypedState_DoubleState:
		if lane == values.TypeCodeDouble {
			return v.DoubleState, nil
		}
	}
	return nil, fmt.Errorf("aggregate state %v does not hold a %s", s, lane)
}

func int64State(n int64) *gen.OneOfTypedState {
	return &gen.OneOfTypedState{State: &gen.OneOfTypedState_Int64State{Int64State: n}}
}

// aggregateStateTypes builds the descriptors the payload is written with: the
// grouping record's and each ARRAY_AGG's element wrapper.
type aggregateStateTypes struct {
	groupKey protoreflect.MessageDescriptor
	arrays   []protoreflect.MessageDescriptor
}

func newAggregateStateTypes(groupingKeys []values.Value, aggregates []expressions.AggregateSpec) (*aggregateStateTypes, error) {
	repository := values.NewTypeProtoRepository()
	types := &aggregateStateTypes{arrays: make([]protoreflect.MessageDescriptor, len(aggregates))}
	if len(groupingKeys) > 0 {
		fields := make([]values.Field, len(groupingKeys))
		for i, key := range groupingKeys {
			fields[i] = values.Field{Name: fmt.Sprintf("_%d", i), FieldType: values.WithNullability(key.Type(), true), Ordinal: i}
		}
		md, err := repository.MessageDescriptorFor(values.NewRecordType("", false, fields))
		if err != nil {
			return nil, fmt.Errorf("grouping key has no message form: %w", err)
		}
		types.groupKey = md
	}
	for i, agg := range aggregates {
		if agg.Function != expressions.AggArrayAgg || agg.Operand == nil {
			continue
		}
		element := agg.Operand.Type()
		if agg.IgnoreNulls {
			element = values.WithNullability(element, false)
		}
		md, err := repository.MessageDescriptorFor(&values.ArrayType{Nullable: true, ElementType: element})
		if err != nil {
			return nil, fmt.Errorf("ARRAY_AGG element has no message form: %w", err)
		}
		types.arrays[i] = md
	}
	return types, nil
}

// encodeJavaPartial is StreamGrouping.getPartialAggregationResult for gs.
func (t *aggregateStateTypes) encodeJavaPartial(keyVals []any, gs *groupState, aggregates []expressions.AggregateSpec) (*gen.PartialAggregationResult, error) {
	par := &gen.PartialAggregationResult{}
	if t.groupKey != nil {
		msg, err := values.MessageFromRowValues(t.groupKey, keyVals)
		if err != nil {
			return nil, fmt.Errorf("grouping key: %w", err)
		}
		key, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg.Interface())
		if err != nil {
			return nil, fmt.Errorf("grouping key: %w", err)
		}
		par.GroupKey = key
	}
	for i, agg := range aggregates {
		state, err := t.encodeAccumulator(i, agg, gs)
		if err != nil {
			return nil, fmt.Errorf("%s state: %w", agg.Function, err)
		}
		par.AccumulatorStates = append(par.AccumulatorStates, state)
	}
	return par, nil
}

func (t *aggregateStateTypes) encodeAccumulator(i int, agg expressions.AggregateSpec, gs *groupState) (*gen.AccumulatorState, error) {
	state := &gen.AccumulatorState{}
	switch agg.Function {
	case expressions.AggCount:
		// COUNT maps a NULL to 0, so its state exists from the first row.
		if gs.count > 0 {
			n := gs.counts[i]
			if expressions.IsCountStar(agg) {
				n = gs.count
			}
			state.State = []*gen.OneOfTypedState{int64State(n)}
		}
	case expressions.AggArrayAgg:
		if gs.count > 0 {
			msg, err := values.MessageFromRowValues(t.arrays[i], []any{append([]any{}, gs.arrays[i]...)})
			if err != nil {
				return nil, err
			}
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg.Interface())
			if err != nil {
				return nil, err
			}
			state.State = []*gen.OneOfTypedState{{State: &gen.OneOfTypedState_BytesState{BytesState: b}}}
		}
	default:
		if gs.counts[i] == 0 {
			return state, nil
		}
		lane := aggregateLane(agg, gs.allInt[i])
		var first any
		switch agg.Function {
		case expressions.AggSum, expressions.AggAvg:
			first = gs.sums[i]
			if lane == values.TypeCodeInt || lane == values.TypeCodeLong {
				if !gs.allInt[i] {
					return nil, fmt.Errorf("an integer sum holds a non-integer")
				}
				first = gs.sumsI[i]
			}
		case expressions.AggMin:
			first = gs.mins[i]
		case expressions.AggMax:
			first = gs.maxs[i]
		case expressions.AggBitmapConstructAgg:
			// BitSet.toByteArray: little-endian, trailing zero bytes dropped.
			state.State = []*gen.OneOfTypedState{{State: &gen.OneOfTypedState_BytesState{
				BytesState: bytes.TrimRight(gs.bitmaps[i], "\x00"),
			}}}
			return state, nil
		default:
			return nil, fmt.Errorf("no Java accumulator state")
		}
		s, err := typedState(lane, first)
		if err != nil {
			return nil, err
		}
		state.State = []*gen.OneOfTypedState{s}
		if agg.Function == expressions.AggAvg {
			state.State = append(state.State, int64State(gs.counts[i]))
		}
	}
	return state, nil
}

// decodeJavaPartial restores the group StreamGrouping's partial describes.
func (t *aggregateStateTypes) decodeJavaPartial(par *gen.PartialAggregationResult, aggregates []expressions.AggregateSpec) ([]any, *groupState, error) {
	var keyVals []any
	if t.groupKey != nil {
		msg := dynamicpb.NewMessage(t.groupKey)
		if err := proto.Unmarshal(par.GetGroupKey(), msg); err != nil {
			return nil, nil, fmt.Errorf("grouping key: %w", err)
		}
		keyVals = values.RowValuesFromMessage(msg)
	} else if len(par.GetGroupKey()) > 0 {
		return nil, nil, fmt.Errorf("a grouping key for an ungrouped aggregation")
	}
	if len(par.GetAccumulatorStates()) != len(aggregates) {
		return nil, nil, fmt.Errorf("%d accumulator states for %d aggregates", len(par.GetAccumulatorStates()), len(aggregates))
	}
	gs := newGroupStateFor(len(aggregates))
	gs.keyVals = keyVals
	// The partial exists only for a group that has seen a row.
	gs.count = 1
	for i, agg := range aggregates {
		if err := t.decodeAccumulator(i, agg, gs, par.GetAccumulatorStates()[i]); err != nil {
			return nil, nil, fmt.Errorf("%s state: %w", agg.Function, err)
		}
	}
	return keyVals, gs, nil
}

func (t *aggregateStateTypes) decodeAccumulator(i int, agg expressions.AggregateSpec, gs *groupState, state *gen.AccumulatorState) error {
	slots := state.GetState()
	if len(slots) == 0 {
		return nil
	}
	want := 1
	if agg.Function == expressions.AggAvg {
		want = 2
	}
	if len(slots) != want {
		return fmt.Errorf("%d typed states, want %d", len(slots), want)
	}
	switch agg.Function {
	case expressions.AggCount:
		n, ok := slots[0].GetState().(*gen.OneOfTypedState_Int64State)
		if !ok || n.Int64State < 0 {
			return fmt.Errorf("COUNT state %v is not a count", slots[0])
		}
		if expressions.IsCountStar(agg) {
			gs.count = n.Int64State
		} else {
			gs.counts[i] = n.Int64State
		}
		return nil
	case expressions.AggArrayAgg:
		b, ok := slots[0].GetState().(*gen.OneOfTypedState_BytesState)
		if !ok {
			return fmt.Errorf("ARRAY_AGG state %v is not bytes", slots[0])
		}
		msg := dynamicpb.NewMessage(t.arrays[i])
		if err := proto.Unmarshal(b.BytesState, msg); err != nil {
			return err
		}
		elems, _ := values.RowValuesFromMessage(msg)[0].([]any)
		if agg.Limit != values.ArrayAggNoLimit && len(elems) > agg.Limit {
			return fmt.Errorf("%d elements past the limit %d", len(elems), agg.Limit)
		}
		gs.arrays[i] = append([]any{}, elems...)
		return nil
	case expressions.AggBitmapConstructAgg:
		b, ok := slots[0].GetState().(*gen.OneOfTypedState_BytesState)
		if !ok || len(b.BytesState) > 31250 {
			return fmt.Errorf("bitmap state %v is not a bitmap", slots[0])
		}
		size := len(b.BytesState)
		if size < 1250 {
			size = 1250
		}
		gs.bitmaps[i] = append(append(make([]byte, 0, size), b.BytesState...), make([]byte, size-len(b.BytesState))...)
		gs.counts[i] = 1
		return nil
	}
	lane := aggregateLane(agg, !isFloatState(slots[0]))
	v, err := laneValue(lane, slots[0])
	if err != nil {
		return err
	}
	gs.counts[i] = 1
	switch agg.Function {
	case expressions.AggMin:
		gs.mins[i] = v
	case expressions.AggMax:
		gs.maxs[i] = v
	case expressions.AggSum, expressions.AggAvg:
		if n, isInt := v.(int64); isInt {
			gs.sumsI[i], gs.sums[i], gs.allInt[i] = n, float64(n), true
		} else {
			gs.sums[i], gs.allInt[i] = v.(float64), false
		}
		if agg.Function == expressions.AggAvg {
			n, ok := slots[1].GetState().(*gen.OneOfTypedState_Int64State)
			if !ok || n.Int64State <= 0 {
				return fmt.Errorf("AVG count %v is not positive", slots[1])
			}
			gs.counts[i] = n.Int64State
		}
	default:
		return fmt.Errorf("no Java accumulator state")
	}
	return nil
}

func isFloatState(s *gen.OneOfTypedState) bool {
	switch s.GetState().(type) {
	case *gen.OneOfTypedState_FloatState, *gen.OneOfTypedState_DoubleState:
		return true
	}
	return false
}

// newGroupStateFor is a group with no rows yet, as aggregateCursor.newGroupState.
func newGroupStateFor(numAggs int) *groupState {
	allInt := make([]bool, numAggs)
	for i := range allInt {
		allInt[i] = true
	}
	return &groupState{
		counts:  make([]int64, numAggs),
		sums:    make([]float64, numAggs),
		sumsI:   make([]int64, numAggs),
		allInt:  allInt,
		mins:    make([]any, numAggs),
		maxs:    make([]any, numAggs),
		arrays:  make([][]any, numAggs),
		bitmaps: make([][]byte, numAggs),
	}
}
