package executor

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"google.golang.org/protobuf/proto"
)

// uuidContinuationTag marks a hex-encoded UUID inside a JSON-serialized
// continuation payload. A UUID flows through the value layer as a neutral
// [16]byte (RFC-162); unlike a []byte slice (which encoding/json base64-encodes),
// a fixed [16]byte array marshals to a LOSSY JSON number list ([85,14,…]) and
// decodes as []float64 — so a UUID sort/group key straddling a continuation
// boundary would re-emerge mis-typed (wrong sort order, and rendered as a
// number list instead of the canonical string). Tag it on encode and rebuild
// the [16]byte on decode so the round-trip is type-preserving.
const uuidContinuationTag = "__uuid16"

// jsonSafeContinuationValue replaces a [16]byte / tuple.UUID with a tagged,
// JSON-round-trippable form; all other values pass through unchanged.
func jsonSafeContinuationValue(v any) any {
	switch u := v.(type) {
	case [16]byte:
		return map[string]any{uuidContinuationTag: hex.EncodeToString(u[:])}
	case tuple.UUID:
		return map[string]any{uuidContinuationTag: hex.EncodeToString(u[:])}
	default:
		return v
	}
}

// restoreContinuationValue is the decode inverse: a {uuidContinuationTag: hex}
// object becomes the neutral [16]byte again; everything else is returned as-is.
func restoreContinuationValue(v any) any {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return v
	}
	s, ok := m[uuidContinuationTag].(string)
	if !ok {
		return v
	}
	raw, derr := hex.DecodeString(s)
	if derr != nil || len(raw) != 16 {
		return v
	}
	var b [16]byte
	copy(b[:], raw)
	return b
}

// jsonSafeSlice / jsonSafeMap apply jsonSafeContinuationValue element-wise for
// the two continuation payload shapes (aggregate keyVals; sort-buffer Datum).
func jsonSafeSlice(in []any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = jsonSafeContinuationValue(v)
	}
	return out
}

func jsonSafeDatum(d any) any {
	in, ok := d.(map[string]any)
	if !ok {
		return d
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = jsonSafeContinuationValue(v)
	}
	return out
}

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
		groupKeyBytes, _ := json.Marshal(groupKeyPayload{GroupKey: groupKey, KeyVals: jsonSafeSlice(keyVals)})

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
		if jErr := json.Unmarshal(par.GroupKey, &payload); jErr != nil {
			// The encoder has always written this JSON payload (the shape and
			// this decoder shipped in the same commit — there is no raw-string
			// legacy format), so unparseable bytes are a corrupt continuation
			// and must error: silently coercing them to a raw group key string
			// would resume aggregation under a key that never matches the
			// recomputed keys (wrong results, no error).
			return nil, "", nil, fmt.Errorf("failed to unmarshal group key in aggregate continuation: %w", jErr)
		}
		groupKey = payload.GroupKey
		keyVals = payload.KeyVals
		// Restore tagged UUIDs to [16]byte, then convert JSON float64
		// numbers back to int64 for integer values (Go SQL type system).
		for i, v := range keyVals {
			v = restoreContinuationValue(v)
			if f, ok := v.(float64); ok && f == float64(int64(f)) {
				v = int64(f)
			}
			keyVals[i] = v
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
		// min_i: JSON-encoded bytes. The len > 0 guard is the legitimate
		// "no MIN state yet" case; corrupt JSON in a present state must error,
		// not silently drop the partial MIN (which would return a wrong
		// aggregate on resume).
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_BytesState); ok && len(v.BytesState) > 0 {
			var minVal any
			if jErr := json.Unmarshal(v.BytesState, &minVal); jErr != nil {
				return nil, "", nil, fmt.Errorf("failed to unmarshal MIN state in aggregate continuation: %w", jErr)
			}
			if f, ok := minVal.(float64); ok && f == float64(int64(f)) {
				minVal = int64(f)
			}
			gs.mins[i] = minVal
		}
		idx++
		// max_i: JSON-encoded bytes (same contract as min_i above).
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_BytesState); ok && len(v.BytesState) > 0 {
			var maxVal any
			if jErr := json.Unmarshal(v.BytesState, &maxVal); jErr != nil {
				return nil, "", nil, fmt.Errorf("failed to unmarshal MAX state in aggregate continuation: %w", jErr)
			}
			if f, ok := maxVal.(float64); ok && f == float64(int64(f)) {
				maxVal = int64(f)
			}
			gs.maxs[i] = maxVal
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
		// Errors are PROPAGATED, never swallowed into a skipped record: a
		// dropped buffer row would silently vanish from the sorted output on
		// resume (wrong results, no error).
		jsonBytes, jErr := json.Marshal(jsonSafeDatum(qr.Datum))
		if jErr != nil {
			return nil, fmt.Errorf("failed to marshal sorted record for continuation: %w", jErr)
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
			return nil, fmt.Errorf("failed to marshal sorted record for continuation: %w", srErr)
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

	for i, srBytes := range msg.Records {
		// Errors are PROPAGATED, never swallowed into a skipped record: a
		// corrupt buffered record must fail the resume, not silently drop a
		// row from the sorted output (wrong results, no error).
		sr := &gen.SortedRecord{}
		if pErr := proto.Unmarshal(srBytes, sr); pErr != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal sorted record %d in continuation: %w", i, pErr)
		}
		var datum map[string]any
		if jErr := json.Unmarshal(sr.Message, &datum); jErr != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal sorted record %d message in continuation: %w", i, jErr)
		}
		// Restore tagged UUIDs to [16]byte, then convert JSON float64 numbers
		// back to int64 for integer columns (matching the Go SQL type system).
		for k, v := range datum {
			v = restoreContinuationValue(v)
			if f, ok := v.(float64); ok && f == float64(int64(f)) {
				v = int64(f)
			}
			datum[k] = v
		}
		var pk tuple.Tuple
		if sr.PrimaryKey != nil {
			var pkErr error
			pk, pkErr = tuple.Unpack(sr.PrimaryKey)
			if pkErr != nil {
				return nil, nil, fmt.Errorf("failed to unpack sorted record %d primary key in continuation: %w", i, pkErr)
			}
		}
		buf = append(buf, QueryResult{Datum: datum, PrimaryKey: pk})
	}

	return msg.Continuation, buf, nil
}
