package executor

import (
	"encoding/json"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"google.golang.org/protobuf/proto"
)

// encodeAggregateContinuation serializes the streaming aggregate
// cursor's partial state using Java's AggregateCursorContinuation proto.
// Carries the inner cursor position + the single in-progress group's
// partial accumulator state.
func encodeAggregateContinuation(
	innerCont recordlayer.RecordCursorContinuation,
	groupKey string,
	keyVals []any,
	gs *groupState,
	aggregates []expressions.AggregateSpec,
) ([]byte, error) {
	var innerBytes []byte
	if innerCont != nil {
		var err error
		innerBytes, err = innerCont.ToBytes()
		if err != nil {
			return nil, err
		}
	}

	msg := &gen.AggregateCursorContinuation{
		Continuation: innerBytes,
	}

	if gs != nil {
		var states []*gen.AccumulatorState
		as := &gen.AccumulatorState{}

		// Pack: count, then per-aggregate (count_i, sum_i, sumsI_i, allInt_i, min_i, max_i)
		as.State = append(as.State, &gen.OneOfTypedState{
			State: &gen.OneOfTypedState_Int64State{Int64State: gs.count},
		})
		for i := range aggregates {
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: gs.counts[i]},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_DoubleState{DoubleState: gs.sums[i]},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: gs.sumsI[i]},
			})
			allIntVal := int64(0)
			if gs.allInt[i] {
				allIntVal = 1
			}
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: allIntVal},
			})
			// min_i: JSON-encoded bytes (nil → empty BytesState)
			minBytes, _ := json.Marshal(gs.mins[i])
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: minBytes},
			})
			// max_i: JSON-encoded bytes (nil → empty BytesState)
			maxBytes, _ := json.Marshal(gs.maxs[i])
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: maxBytes},
			})
		}
		states = append(states, as)

		// Serialize groupKey + keyVals into group_key bytes.
		// We JSON-encode a struct containing both so that keyVals
		// (needed by finalizeGroup) survive the continuation round-trip.
		type groupKeyPayload struct {
			GroupKey string `json:"g"`
			KeyVals  []any  `json:"k"`
		}
		groupKeyBytes, _ := json.Marshal(groupKeyPayload{GroupKey: groupKey, KeyVals: keyVals})

		msg.PartialAggregationResults = &gen.PartialAggregationResult{
			GroupKey:          groupKeyBytes,
			AccumulatorStates: states,
		}
	}

	return proto.Marshal(msg)
}

// decodeAggregateContinuation deserializes the AggregateCursorContinuation
// proto. Returns the inner continuation and the partial group state.
func decodeAggregateContinuation(data []byte, numAggs int) (
	innerContinuation []byte,
	groupKey string,
	gs *groupState,
	err error,
) {
	msg := &gen.AggregateCursorContinuation{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, "", nil, fmt.Errorf("failed to unmarshal aggregate continuation: %w", err)
	}

	innerContinuation = msg.Continuation

	if msg.PartialAggregationResults == nil {
		return innerContinuation, "", nil, nil
	}

	par := msg.PartialAggregationResults

	// Decode groupKey + keyVals from JSON payload.
	var keyVals []any
	if par.GroupKey != nil {
		type groupKeyPayload struct {
			GroupKey string `json:"g"`
			KeyVals  []any  `json:"k"`
		}
		var payload groupKeyPayload
		if jErr := json.Unmarshal(par.GroupKey, &payload); jErr == nil {
			groupKey = payload.GroupKey
			keyVals = payload.KeyVals
			// JSON deserializes numbers as float64; convert back to int64
			// for integer values (matching the Go SQL type system).
			for i, v := range keyVals {
				if f, ok := v.(float64); ok && f == float64(int64(f)) {
					keyVals[i] = int64(f)
				}
			}
		} else {
			// Fallback: treat as raw group key string (legacy format).
			groupKey = string(par.GroupKey)
		}
	}

	if len(par.AccumulatorStates) == 0 {
		return innerContinuation, groupKey, nil, nil
	}

	as := par.AccumulatorStates[0]
	gs = &groupState{
		keyVals: keyVals,
		counts:  make([]int64, numAggs),
		sums:    make([]float64, numAggs),
		sumsI:   make([]int64, numAggs),
		allInt:  make([]bool, numAggs),
		mins:    make([]any, numAggs),
		maxs:    make([]any, numAggs),
	}

	idx := 0
	if idx < len(as.State) {
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_Int64State); ok {
			gs.count = v.Int64State
		}
		idx++
	}
	for i := 0; i < numAggs && idx+5 < len(as.State); i++ {
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_Int64State); ok {
			gs.counts[i] = v.Int64State
		}
		idx++
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_DoubleState); ok {
			gs.sums[i] = v.DoubleState
		}
		idx++
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_Int64State); ok {
			gs.sumsI[i] = v.Int64State
		}
		idx++
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_Int64State); ok {
			gs.allInt[i] = v.Int64State != 0
		}
		idx++
		// min_i: JSON-encoded bytes
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_BytesState); ok && len(v.BytesState) > 0 {
			var minVal any
			if jErr := json.Unmarshal(v.BytesState, &minVal); jErr == nil {
				if f, ok := minVal.(float64); ok && f == float64(int64(f)) {
					minVal = int64(f)
				}
				gs.mins[i] = minVal
			}
		}
		idx++
		// max_i: JSON-encoded bytes
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_BytesState); ok && len(v.BytesState) > 0 {
			var maxVal any
			if jErr := json.Unmarshal(v.BytesState, &maxVal); jErr == nil {
				if f, ok := maxVal.(float64); ok && f == float64(int64(f)) {
					maxVal = int64(f)
				}
				gs.maxs[i] = maxVal
			}
		}
		idx++
	}

	return innerContinuation, groupKey, gs, nil
}

// encodeSortContinuation serializes the sort cursor's state using
// Java's MemorySortContinuation proto. The buffered records are
// serialized as JSON bytes in the SortedRecord.message field.
// Java uses protobuf-serialized records; Go uses JSON for the datum
// map since QueryResult.Datum is map[string]any.
func encodeSortContinuation(
	innerCont recordlayer.RecordCursorContinuation,
	buf []QueryResult,
) ([]byte, error) {
	var innerBytes []byte
	if innerCont != nil {
		var err error
		innerBytes, err = innerCont.ToBytes()
		if err != nil {
			return nil, err
		}
	}

	msg := &gen.MemorySortContinuation{
		Continuation: innerBytes,
	}

	for _, qr := range buf {
		jsonBytes, jErr := json.Marshal(qr.Datum)
		if jErr != nil {
			continue
		}
		var pkBytes []byte
		if qr.PrimaryKey != nil {
			pkBytes = qr.PrimaryKey.Pack()
		}
		sr := &gen.SortedRecord{
			PrimaryKey: pkBytes,
			Message:    jsonBytes,
		}
		srBytes, srErr := proto.Marshal(sr)
		if srErr != nil {
			continue
		}
		msg.Records = append(msg.Records, srBytes)
	}

	return proto.Marshal(msg)
}

// decodeSortContinuation deserializes the MemorySortContinuation proto.
func decodeSortContinuation(data []byte) (innerContinuation []byte, buf []QueryResult, err error) {
	msg := &gen.MemorySortContinuation{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal sort continuation: %w", err)
	}

	for _, srBytes := range msg.Records {
		sr := &gen.SortedRecord{}
		if pErr := proto.Unmarshal(srBytes, sr); pErr != nil {
			continue
		}
		var datum map[string]any
		if jErr := json.Unmarshal(sr.Message, &datum); jErr != nil {
			continue
		}
		// JSON unmarshals numbers as float64. Convert back to int64
		// for integer columns (matching the Go SQL type system).
		for k, v := range datum {
			if f, ok := v.(float64); ok && f == float64(int64(f)) {
				datum[k] = int64(f)
			}
		}
		var pk tuple.Tuple
		if sr.PrimaryKey != nil {
			pk, _ = tuple.Unpack(sr.PrimaryKey)
		}
		buf = append(buf, QueryResult{Datum: datum, PrimaryKey: pk})
	}

	return msg.Continuation, buf, nil
}
