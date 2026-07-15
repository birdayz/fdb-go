package executor

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

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
// the sort-buffer row-slot continuation payload.
func jsonSafeSlice(in []any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = jsonSafeContinuationValue(v)
	}
	return out
}

// The streaming-aggregate continuation is GO-INTERNAL: it reuses only the
// AggregateCursorContinuation / PartialAggregationResult proto message NAMES,
// never Java's payload structure. Java serializes the group key as a protobuf
// DynamicMessage and one typed accumulator state per aggregate; Go carries its
// own accumulator model (count + per-aggregate count/sum/int-sum/all-int/min/max)
// that Java's AggregateCursor cannot parse — and Go cannot parse Java's. Because
// the two never interoperate, the payload is free to use a lossless typed codec
// instead of encoding/json, which corrupts the state in two ways JSON cannot
// avoid:
//   - the group-break key is string(tuple.Pack(...)) — arbitrary bytes; a
//     tuple-packed int64>=128, any negative int, or any float64 is invalid UTF-8,
//     and json.Marshal rewrites invalid UTF-8 to U+FFFD, so the resumed key never
//     matches the recomputed key and one straddling group splits into two rows;
//   - every JSON number decodes as float64 and every []byte as a base64 string,
//     so a float64 / int64>2^53 / []byte group key or a MIN/MAX partial loses its
//     Go type and 64-bit precision.
//
// contVal* are the type tags for one continuation value. The whitelist covers
// every type the value layer actually produces as a grouping key
// (nil/int64/float64/bool/string/[]byte and the neutral UUID [16]byte) and as a
// MIN/MAX partial (int64/float64), plus the remaining numeric widths isNumeric
// admits (int32/float32/int) so a defensively-typed partial still round-trips
// exactly. A type outside the whitelist falls back to a JSON blob (contValJSON),
// no worse than the prior all-JSON behavior; the whitelisted types never reach it.
const (
	contValNil     byte = 0
	contValInt64   byte = 1
	contValFloat64 byte = 2
	contValString  byte = 3
	contValBytes   byte = 4
	contValBool    byte = 5
	contValUUID    byte = 6 // neutral [16]byte / tuple.UUID
	contValInt32   byte = 7
	contValFloat32 byte = 8
	contValInt     byte = 9 // platform int, restored as int
	contValJSON    byte = 15
)

// appendContValue appends v to buf in the typed, lossless continuation encoding.
func appendContValue(buf []byte, v any) []byte {
	switch t := v.(type) {
	case nil:
		return append(buf, contValNil)
	case int64:
		return binary.BigEndian.AppendUint64(append(buf, contValInt64), uint64(t))
	case int:
		return binary.BigEndian.AppendUint64(append(buf, contValInt), uint64(int64(t)))
	case int32:
		return binary.BigEndian.AppendUint32(append(buf, contValInt32), uint32(t))
	case float64:
		return binary.BigEndian.AppendUint64(append(buf, contValFloat64), math.Float64bits(t))
	case float32:
		return binary.BigEndian.AppendUint32(append(buf, contValFloat32), math.Float32bits(t))
	case bool:
		b := byte(0)
		if t {
			b = 1
		}
		return append(buf, contValBool, b)
	case string:
		buf = append(buf, contValString)
		buf = binary.AppendUvarint(buf, uint64(len(t)))
		return append(buf, t...)
	case []byte:
		buf = append(buf, contValBytes)
		buf = binary.AppendUvarint(buf, uint64(len(t)))
		return append(buf, t...)
	case [16]byte:
		return append(append(buf, contValUUID), t[:]...)
	case tuple.UUID:
		return append(append(buf, contValUUID), t[:]...)
	default:
		// A type outside the typed whitelist (e.g. time.Time). JSON keeps the
		// prior behavior without a hard failure; whitelisted types never reach here.
		j, _ := json.Marshal(v)
		buf = append(buf, contValJSON)
		buf = binary.AppendUvarint(buf, uint64(len(j)))
		return append(buf, j...)
	}
}

// readContValue decodes one typed continuation value, returning it and the
// unconsumed tail. A truncated or unknown-tag payload is a corrupt continuation
// and errors — never a silent wrong resume.
func readContValue(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, fmt.Errorf("aggregate continuation: truncated typed value")
	}
	tag := b[0]
	b = b[1:]
	need := func(n int) error {
		if len(b) < n {
			return fmt.Errorf("aggregate continuation: truncated typed value (tag %d)", tag)
		}
		return nil
	}
	switch tag {
	case contValNil:
		return nil, b, nil
	case contValInt64:
		if err := need(8); err != nil {
			return nil, nil, err
		}
		return int64(binary.BigEndian.Uint64(b)), b[8:], nil
	case contValInt:
		if err := need(8); err != nil {
			return nil, nil, err
		}
		return int(int64(binary.BigEndian.Uint64(b))), b[8:], nil
	case contValInt32:
		if err := need(4); err != nil {
			return nil, nil, err
		}
		return int32(binary.BigEndian.Uint32(b)), b[4:], nil
	case contValFloat64:
		if err := need(8); err != nil {
			return nil, nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[8:], nil
	case contValFloat32:
		if err := need(4); err != nil {
			return nil, nil, err
		}
		return math.Float32frombits(binary.BigEndian.Uint32(b)), b[4:], nil
	case contValBool:
		if err := need(1); err != nil {
			return nil, nil, err
		}
		return b[0] != 0, b[1:], nil
	case contValString:
		run, rest, err := readContBytes(b, tag)
		if err != nil {
			return nil, nil, err
		}
		return string(run), rest, nil
	case contValBytes:
		run, rest, err := readContBytes(b, tag)
		if err != nil {
			return nil, nil, err
		}
		// Copy: run aliases the proto buffer; a []byte value must own its bytes.
		out := make([]byte, len(run))
		copy(out, run)
		return out, rest, nil
	case contValUUID:
		if err := need(16); err != nil {
			return nil, nil, err
		}
		var u [16]byte
		copy(u[:], b[:16])
		return u, b[16:], nil
	case contValJSON:
		run, rest, err := readContBytes(b, tag)
		if err != nil {
			return nil, nil, err
		}
		var v any
		if jErr := json.Unmarshal(run, &v); jErr != nil {
			return nil, nil, fmt.Errorf("aggregate continuation: bad JSON fallback value: %w", jErr)
		}
		return v, rest, nil
	default:
		return nil, nil, fmt.Errorf("aggregate continuation: unknown typed-value tag %d", tag)
	}
}

// readContBytes reads a uvarint-length-prefixed byte run, returning a subslice
// (the caller copies when it must own the bytes) and the tail.
func readContBytes(b []byte, tag byte) (run, rest []byte, err error) {
	n, m := binary.Uvarint(b)
	if m <= 0 {
		return nil, nil, fmt.Errorf("aggregate continuation: bad length prefix (tag %d)", tag)
	}
	b = b[m:]
	if uint64(len(b)) < n {
		return nil, nil, fmt.Errorf("aggregate continuation: truncated byte run (tag %d)", tag)
	}
	return b[:n], b[n:], nil
}

// encodeAggGroupKey packs the in-progress group's identity into the
// PartialAggregationResult.group_key bytes: the packed group-key bytes VERBATIM
// (raw, not a JSON string — arbitrary tuple bytes must survive to match the
// recomputed key on resume) followed by the typed keyVals (carried unchanged
// into finalizeGroup's output row, so their Go type and precision are preserved).
func encodeAggGroupKey(groupKey string, keyVals []any) []byte {
	buf := binary.AppendUvarint(nil, uint64(len(groupKey)))
	buf = append(buf, groupKey...)
	buf = binary.AppendUvarint(buf, uint64(len(keyVals)))
	for _, v := range keyVals {
		buf = appendContValue(buf, v)
	}
	return buf
}

// decodeAggGroupKey is the inverse of encodeAggGroupKey.
func decodeAggGroupKey(b []byte) (groupKey string, keyVals []any, err error) {
	n, m := binary.Uvarint(b)
	if m <= 0 {
		return "", nil, fmt.Errorf("aggregate continuation: bad group-key length prefix")
	}
	b = b[m:]
	if uint64(len(b)) < n {
		return "", nil, fmt.Errorf("aggregate continuation: truncated group key")
	}
	groupKey = string(b[:n])
	b = b[n:]
	cnt, m2 := binary.Uvarint(b)
	if m2 <= 0 {
		return "", nil, fmt.Errorf("aggregate continuation: bad keyVals count prefix")
	}
	b = b[m2:]
	// Each value consumes at least one tag byte, so a count exceeding the
	// remaining bytes is corrupt — reject before allocating.
	if cnt > uint64(len(b)) {
		return "", nil, fmt.Errorf("aggregate continuation: keyVals count %d exceeds remaining bytes", cnt)
	}
	keyVals = make([]any, 0, cnt)
	for i := uint64(0); i < cnt; i++ {
		var v any
		v, b, err = readContValue(b)
		if err != nil {
			return "", nil, err
		}
		keyVals = append(keyVals, v)
	}
	return groupKey, keyVals, nil
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
			// min_i / max_i: one typed value each (nil → a lone nil tag). The
			// typed codec preserves int64/float64 exactly — JSON collapsed both to
			// float64 and then re-narrowed integral doubles to int64, flipping a
			// DOUBLE MIN/MAX's type on resume.
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: appendContValue(nil, gs.mins[i])},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: appendContValue(nil, gs.maxs[i])},
			})
		}
		states = append(states, as)

		msg.PartialAggregationResults = &gen.PartialAggregationResult{
			GroupKey:          encodeAggGroupKey(groupKey, keyVals),
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

	// Decode groupKey + keyVals from the typed payload. Unparseable bytes are a
	// corrupt continuation and must error: silently coercing them to a raw group
	// key would resume aggregation under a key that never matches the recomputed
	// keys (wrong results, no error).
	var keyVals []any
	if par.GroupKey != nil {
		var gkErr error
		groupKey, keyVals, gkErr = decodeAggGroupKey(par.GroupKey)
		if gkErr != nil {
			return nil, "", nil, fmt.Errorf("failed to decode group key in aggregate continuation: %w", gkErr)
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
		// min_i / max_i: one typed value each. The len > 0 guard is the legitimate
		// "no state" case (an absent field, 0 bytes); a present state always
		// carries at least the tag byte. Corrupt bytes must error, not silently
		// drop the partial (which would return a wrong aggregate on resume).
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_BytesState); ok && len(v.BytesState) > 0 {
			minVal, _, dErr := readContValue(v.BytesState)
			if dErr != nil {
				return nil, "", nil, fmt.Errorf("failed to decode MIN state in aggregate continuation: %w", dErr)
			}
			gs.mins[i] = minVal
		}
		idx++
		if v, ok := as.State[idx].State.(*gen.OneOfTypedState_BytesState); ok && len(v.BytesState) > 0 {
			maxVal, _, dErr := readContValue(v.BytesState)
			if dErr != nil {
				return nil, "", nil, fmt.Errorf("failed to decode MAX state in aggregate continuation: %w", dErr)
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
// Java uses protobuf-serialized records; Go serializes each buffered row's
// positional data (field names + slots) as JSON in that Go-owned field.
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
		//
		// PAYLOAD FORMAT (Go-owned): the SortedRecord proto mirrors Java's
		// byte-for-byte and cannot grow fields, but Go already stores a JSON
		// datum in `message` where Java stores record-proto bytes — the
		// payload inside the opaque field is ours to version. The first byte
		// discriminates: a JSON OBJECT is the legacy datum-only payload; a JSON
		// ARRAY is versioned by its element count — 2 = [_, complete] (v2),
		// 3 = [_, _, positional] (the current form). Slot 1 was the QueryResult
		// .Complete flag, now RETIRED — the field is deleted, decode IGNORES slot 1,
		// and it is written as a constant `false` dead placeholder purely to keep the
		// 3-slot array shape wire-identical (no format-version bump).
		//
		// The Positional row is the SOLE runtime output row (there is
		// no name-keyed row), so a resumed sort buffer MUST carry it or the
		// reader rejects every buffered row after a page boundary. Slot 0 (formerly
		// the JSON datum) is null, slot 1 is the retired-Complete placeholder, slot 2
		// is the positional {n:[field names], s:[slots]} (decode reconstructs the
		// PositionalRow via positionalTypeFromNames). A pre-positional binary's object
		// / 2-element payload carries no positional and is rejected on decode.
		payload := []any{nil, false}
		if qr.Positional != nil && qr.Positional.Type != nil {
			names := make([]string, len(qr.Positional.Type.Fields))
			for fi, f := range qr.Positional.Type.Fields {
				names[fi] = f.Name
			}
			payload = append(payload, map[string]any{
				"n": names,
				"s": jsonSafeSlice(qr.Positional.Slots),
			})
		}
		jsonBytes, jErr := json.Marshal(payload)
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
		// Payload discrimination (see encodeSortContinuation): a JSON ARRAY is
		// [_, _] or [_, _, positional] — slot 0 is the deleted datum and slot 1 the
		// retired-Complete placeholder, both IGNORED. A legacy bare JSON OBJECT
		// payload carries no positional (pre-migration).
		var positional *PositionalRow
		trimmed := bytes.TrimLeft(sr.Message, " \t\r\n")
		if len(trimmed) > 0 && trimmed[0] == '[' {
			var wrapper []json.RawMessage
			if jErr := json.Unmarshal(sr.Message, &wrapper); jErr != nil {
				return nil, nil, fmt.Errorf("failed to unmarshal sorted record %d array payload in continuation: %w", i, jErr)
			}
			// Slot 1 (the retired-Complete placeholder) is IGNORED. Only a
			// 3-element array carries a positional (slot 2); any other arity leaves
			// positional nil and is rejected by the positional==nil guard below.
			if len(wrapper) == 3 {
				// Positional payload {n:[names], s:[slots]}. Reconstruct the
				// PositionalRow — the sole runtime output row the reader consumes.
				var pp struct {
					N []string `json:"n"`
					S []any    `json:"s"`
				}
				if jErr := json.Unmarshal(wrapper[2], &pp); jErr != nil {
					return nil, nil, fmt.Errorf("failed to unmarshal sorted record %d positional payload in continuation: %w", i, jErr)
				}
				slots := make([]any, len(pp.S))
				for si, v := range pp.S {
					v = restoreContinuationValue(v)
					if f, ok := v.(float64); ok && f == float64(int64(f)) {
						v = int64(f)
					}
					slots[si] = v
				}
				positional = &PositionalRow{Type: positionalTypeFromNames(pp.N), Slots: slots}
			}
		} else {
			// Legacy bare-object payload (the deleted name-keyed datum): validate it
			// parses as JSON (a corrupt payload must fail the resume, not silently
			// decode). It carries no positional; the positional==nil guard below
			// rejects it — its name-keyed row is unreconstructable in the ordinal model.
			var throwaway map[string]any
			if jErr := json.Unmarshal(sr.Message, &throwaway); jErr != nil {
				return nil, nil, fmt.Errorf("failed to unmarshal sorted record %d message in continuation: %w", i, jErr)
			}
		}
		var pk tuple.Tuple
		if sr.PrimaryKey != nil {
			var pkErr error
			pk, pkErr = tuple.Unpack(sr.PrimaryKey)
			if pkErr != nil {
				return nil, nil, fmt.Errorf("failed to unpack sorted record %d primary key in continuation: %w", i, pkErr)
			}
		}
		// A resumed sort row MUST reconstruct a PositionalRow (the sole runtime row).
		// A legacy bare-object payload OR a v2 [_, complete] array (both pre-positional,
		// written by an older binary) leaves positional nil — resuming it would give
		// all-NULL sort keys and wrong/misordered rows, since the name-keyed
		// fallback that used to read them is deleted. Fail the resume loudly (the caller
		// restarts the query) rather than silently drop the data. A corrupt payload has
		// already failed above; this rejects the VALID-but-unreconstructable legacy shape.
		if positional == nil {
			return nil, nil, fmt.Errorf("sorted record %d has no positional payload (a legacy sort continuation predating the positional row model); it cannot be resumed under the ordinal row model — restart the query", i)
		}
		buf = append(buf, QueryResult{PrimaryKey: pk, Positional: positional})
	}

	return msg.Continuation, buf, nil
}
