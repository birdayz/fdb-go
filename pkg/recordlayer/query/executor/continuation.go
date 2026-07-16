package executor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"google.golang.org/protobuf/proto"
)

// The streaming-aggregate and in-memory-sort continuations are GO-INTERNAL: the
// aggregate reuses only the AggregateCursorContinuation / PartialAggregationResult
// proto message NAMES (never Java's payload structure — Go carries its own
// count + per-aggregate count/sum/int-sum/all-int/min/max accumulator model that
// Java's AggregateCursor cannot parse, and vice versa), and Java has no in-memory
// sort at all. Because neither payload ever interoperates with Java, both carry a
// lossless typed codec instead of encoding/json, which corrupts row values in two
// ways JSON cannot avoid:
//   - the aggregate group-break key is string(tuple.Pack(...)) — arbitrary bytes;
//     a tuple-packed int64>=128, any negative int, or any float64 is invalid UTF-8,
//     and json.Marshal rewrites invalid UTF-8 to U+FFFD, so the resumed key never
//     matches the recomputed key and one straddling group splits into two rows;
//   - every JSON number decodes as float64 and every []byte as a base64 string, so
//     a float64 / int64>2^53 / uint64 / []byte group key, a MIN/MAX partial, or an
//     ORDER BY sort slot loses its Go type and 64-bit precision.
//
// contVal* are the type tags for one continuation value. The codec encodes
// EXACTLY, with NO lossy fallback, every Go type the value layer can carry as a
// grouping key, a MIN/MAX partial, or a sort ORDER BY slot:
//   - nil (SQL NULL), bool, string;
//   - int64 / float64 (the record-materialized numerics) plus int / int32 /
//     float32 (the remaining widths isNumeric admits, so a defensively-typed
//     partial still round-trips exactly);
//   - []byte (BYTES/BINARY) and the neutral UUID [16]byte / tuple.UUID;
//   - uint64 — a positive index-sourced integer using bit 63 (in (2^63, 2^64))
//     decodes from the FDB tuple as uint64 (tuple.decodeInt) and
//     tupleElementToRowValue passes it through unchanged, so it is a reachable key;
//   - *big.Int / big.Int — an integer beyond the uint64 range a tuple key can
//     carry. Not producible from a proto scalar field today (those cap at uint64),
//     but encoded losslessly (sign + magnitude) so it can never silently round;
//   - time.Time — DATE/TIMESTAMP values flow through the row domain as STRINGS
//     today (so a time.Time key is not currently reachable), but a time.Time is
//     encoded losslessly (MarshalBinary) rather than left to round-trip wrong.
//
// A value whose Go type is NONE of the above — []any, a proto.Message (needs its
// descriptor to decode), a map, or any other non-scalar the codec cannot
// faithfully reconstruct — is a correct-or-loud ERROR on encode, NOT a silent
// JSON blob: a straddling group / sort buffer with such a key must FAIL the resume
// loudly, never resume with a mistyped value.
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
	contValInt     byte = 9  // platform int, restored as int
	contValUint64  byte = 10 // index-sourced integer in (2^63, 2^64)
	contValBigInt  byte = 11 // *big.Int / big.Int, sign byte + big-endian magnitude
	contValTime    byte = 12 // time.Time via MarshalBinary
)

// appendContValue appends v to buf in the typed, lossless continuation encoding.
// A type the codec cannot faithfully reconstruct is a correct-or-loud error, never
// a silent JSON blob (which would resume a straddling group / sort buffer with a
// mistyped value).
func appendContValue(buf []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return append(buf, contValNil), nil
	case int64:
		return binary.BigEndian.AppendUint64(append(buf, contValInt64), uint64(t)), nil
	case int:
		return binary.BigEndian.AppendUint64(append(buf, contValInt), uint64(int64(t))), nil
	case int32:
		return binary.BigEndian.AppendUint32(append(buf, contValInt32), uint32(t)), nil
	case uint64:
		return binary.BigEndian.AppendUint64(append(buf, contValUint64), t), nil
	case float64:
		return binary.BigEndian.AppendUint64(append(buf, contValFloat64), math.Float64bits(t)), nil
	case float32:
		return binary.BigEndian.AppendUint32(append(buf, contValFloat32), math.Float32bits(t)), nil
	case bool:
		b := byte(0)
		if t {
			b = 1
		}
		return append(buf, contValBool, b), nil
	case string:
		buf = append(buf, contValString)
		buf = binary.AppendUvarint(buf, uint64(len(t)))
		return append(buf, t...), nil
	case []byte:
		buf = append(buf, contValBytes)
		buf = binary.AppendUvarint(buf, uint64(len(t)))
		return append(buf, t...), nil
	case [16]byte:
		return append(append(buf, contValUUID), t[:]...), nil
	case tuple.UUID:
		return append(append(buf, contValUUID), t[:]...), nil
	case *big.Int:
		return appendContBigInt(buf, t), nil
	case big.Int:
		return appendContBigInt(buf, &t), nil
	case time.Time:
		mb, err := t.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("continuation: cannot encode time.Time value: %w", err)
		}
		buf = append(buf, contValTime)
		buf = binary.AppendUvarint(buf, uint64(len(mb)))
		return append(buf, mb...), nil
	default:
		// Correct-or-loud: a non-scalar the codec cannot faithfully reconstruct
		// (a proto.Message needs its descriptor to decode; []any / map have no
		// stable typed form) must FAIL the encode, so a straddling group / sort
		// buffer whose key or partial is such a value fails the resume loudly
		// rather than silently resuming with a mistyped value.
		return nil, fmt.Errorf("continuation: cannot encode value of type %T (unsupported continuation value)", v)
	}
}

// appendContBigInt encodes an arbitrary-precision integer as
// contValBigInt || sign byte (0 = zero/positive, 1 = negative) ||
// uvarint(len(magnitude)) || big-endian magnitude. big.Int.Bytes() yields the
// big-endian magnitude (empty for zero); the sign byte restores the sign on decode.
func appendContBigInt(buf []byte, i *big.Int) []byte {
	buf = append(buf, contValBigInt)
	sign := byte(0)
	if i.Sign() < 0 {
		sign = 1
	}
	buf = append(buf, sign)
	mag := i.Bytes()
	buf = binary.AppendUvarint(buf, uint64(len(mag)))
	return append(buf, mag...)
}

// readContValue decodes one typed continuation value, returning it and the
// unconsumed tail. A truncated or unknown-tag payload is a corrupt continuation
// and errors — never a silent wrong resume.
func readContValue(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, fmt.Errorf("continuation: truncated typed value")
	}
	tag := b[0]
	b = b[1:]
	need := func(n int) error {
		if len(b) < n {
			return fmt.Errorf("continuation: truncated typed value (tag %d)", tag)
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
	case contValUint64:
		if err := need(8); err != nil {
			return nil, nil, err
		}
		return binary.BigEndian.Uint64(b), b[8:], nil
	case contValInt32:
		if err := need(4); err != nil {
			return nil, nil, err
		}
		return int32(binary.BigEndian.Uint32(b)), b[4:], nil
	case contValBigInt:
		if err := need(1); err != nil {
			return nil, nil, err
		}
		sign := b[0]
		if sign > 1 {
			return nil, nil, fmt.Errorf("continuation: bad big.Int sign byte %d (tag %d)", sign, tag)
		}
		mag, rest, err := readContBytes(b[1:], tag)
		if err != nil {
			return nil, nil, err
		}
		n := new(big.Int).SetBytes(mag)
		if sign == 1 {
			n.Neg(n)
		}
		return n, rest, nil
	case contValTime:
		run, rest, err := readContBytes(b, tag)
		if err != nil {
			return nil, nil, err
		}
		var tm time.Time
		if uErr := tm.UnmarshalBinary(run); uErr != nil {
			return nil, nil, fmt.Errorf("continuation: bad time.Time value (tag %d): %w", tag, uErr)
		}
		return tm, rest, nil
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
	default:
		return nil, nil, fmt.Errorf("continuation: unknown typed-value tag %d", tag)
	}
}

// readContBytes reads a uvarint-length-prefixed byte run, returning a subslice
// (the caller copies when it must own the bytes) and the tail.
func readContBytes(b []byte, tag byte) (run, rest []byte, err error) {
	n, m := binary.Uvarint(b)
	if m <= 0 {
		return nil, nil, fmt.Errorf("continuation: bad length prefix (tag %d)", tag)
	}
	b = b[m:]
	if uint64(len(b)) < n {
		return nil, nil, fmt.Errorf("continuation: truncated byte run (tag %d)", tag)
	}
	return b[:n], b[n:], nil
}

// appendContSlice appends a uvarint count prefix followed by each value in the
// typed continuation encoding. Shared by the aggregate group-key keyVals and the
// sort buffer's ORDER BY slots. An unencodable element propagates the
// appendContValue error (correct-or-loud).
func appendContSlice(buf []byte, vals []any) ([]byte, error) {
	buf = binary.AppendUvarint(buf, uint64(len(vals)))
	for _, v := range vals {
		var err error
		buf, err = appendContValue(buf, v)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// readContSlice reads a count-prefixed run of typed continuation values written
// by appendContSlice, returning the values and the unconsumed tail.
func readContSlice(b []byte) ([]any, []byte, error) {
	cnt, m := binary.Uvarint(b)
	if m <= 0 {
		return nil, nil, fmt.Errorf("continuation: bad value-count prefix")
	}
	b = b[m:]
	// Each value consumes at least one tag byte, so a count exceeding the
	// remaining bytes is corrupt — reject before allocating.
	if cnt > uint64(len(b)) {
		return nil, nil, fmt.Errorf("continuation: value count %d exceeds remaining bytes", cnt)
	}
	out := make([]any, 0, cnt)
	for i := uint64(0); i < cnt; i++ {
		var v any
		var err error
		v, b, err = readContValue(b)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, v)
	}
	return out, b, nil
}

// encodeAggGroupKey packs the in-progress group's identity into the
// PartialAggregationResult.group_key bytes: the packed group-key bytes VERBATIM
// (raw, not a JSON string — arbitrary tuple bytes must survive to match the
// recomputed key on resume) followed by the typed keyVals (carried unchanged into
// finalizeGroup's output row, so their Go type and precision are preserved). An
// unencodable keyVal errors (correct-or-loud) rather than silently lose type.
func encodeAggGroupKey(groupKey string, keyVals []any) ([]byte, error) {
	buf := binary.AppendUvarint(nil, uint64(len(groupKey)))
	buf = append(buf, groupKey...)
	return appendContSlice(buf, keyVals)
}

// decodeAggGroupKey is the inverse of encodeAggGroupKey.
func decodeAggGroupKey(b []byte) (groupKey string, keyVals []any, err error) {
	n, m := binary.Uvarint(b)
	if m <= 0 {
		return "", nil, fmt.Errorf("continuation: bad group-key length prefix")
	}
	b = b[m:]
	if uint64(len(b)) < n {
		return "", nil, fmt.Errorf("continuation: truncated group key")
	}
	groupKey = string(b[:n])
	b = b[n:]
	keyVals, _, err = readContSlice(b)
	if err != nil {
		return "", nil, err
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
			// DOUBLE MIN/MAX's type on resume. An unencodable partial errors
			// (correct-or-loud) rather than silently lose its type on resume.
			minBytes, err := appendContValue(nil, gs.mins[i])
			if err != nil {
				return nil, fmt.Errorf("failed to encode MIN state for aggregate continuation: %w", err)
			}
			maxBytes, err := appendContValue(nil, gs.maxs[i])
			if err != nil {
				return nil, fmt.Errorf("failed to encode MAX state for aggregate continuation: %w", err)
			}
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: minBytes},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: maxBytes},
			})
		}
		states = append(states, as)

		gkBytes, err := encodeAggGroupKey(groupKey, keyVals)
		if err != nil {
			return nil, fmt.Errorf("failed to encode group key for aggregate continuation: %w", err)
		}
		msg.PartialAggregationResults = &gen.PartialAggregationResult{
			GroupKey:          gkBytes,
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
// positional data (field names + a typed-codec slot blob) as JSON in that
// Go-owned field.
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
		// The Positional row is the SOLE runtime output row (there is no
		// name-keyed row), so a resumed sort buffer MUST carry it or the reader
		// rejects every buffered row after a page boundary. Slot 0 (formerly the
		// JSON datum) is null, slot 1 is the retired-Complete placeholder, slot 2
		// is the positional {n:[field names], b:<typed-codec slot blob>}. The slots
		// ride the typed codec (appendContSlice), NOT JSON: JSON collapses []byte to
		// a base64 string, every number to float64 (int64>2^53 rounds, a whole
		// DOUBLE re-narrows to int64), corrupting a BYTES/DOUBLE ORDER BY key whose
		// buffer straddles a page. `b` is a []byte, so json.Marshal base64-encodes
		// the blob (invalid UTF-8 in the typed bytes survives). Decode reconstructs
		// the PositionalRow via positionalTypeFromNames + readContSlice. A
		// pre-positional binary's object / 2-element payload, or an OLD 3-element
		// {n, s:[JSON slots]} payload (no `b`), carries no typed blob and is rejected
		// on decode (fail-closed — its slots are unreconstructable losslessly).
		payload := []any{nil, false}
		if qr.Positional != nil && qr.Positional.Type != nil {
			names := make([]string, len(qr.Positional.Type.Fields))
			for fi, f := range qr.Positional.Type.Fields {
				names[fi] = f.Name
			}
			blob, bErr := appendContSlice(nil, qr.Positional.Slots)
			if bErr != nil {
				return nil, fmt.Errorf("failed to encode sorted record slots for continuation: %w", bErr)
			}
			payload = append(payload, map[string]any{
				"n": names,
				"b": blob, // typed-codec slot blob; json.Marshal base64-encodes []byte
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
				// Positional payload {n:[names], b:<typed-codec slot blob>}. `b` is a
				// base64 JSON string that json unmarshals back into the []byte blob;
				// readContSlice decodes the typed slots losslessly. Reconstruct the
				// PositionalRow — the sole runtime output row the reader consumes.
				var pp struct {
					N []string `json:"n"`
					B []byte   `json:"b"` // typed-codec slot blob (base64 in JSON)
				}
				if jErr := json.Unmarshal(wrapper[2], &pp); jErr != nil {
					return nil, nil, fmt.Errorf("failed to unmarshal sorted record %d positional payload in continuation: %w", i, jErr)
				}
				// A 3-element array with no typed blob is an OLD binary's
				// {n, s:[JSON slots]} payload (the lossy form F33 retired): its slots
				// cannot be reconstructed without type/precision loss, so FAIL the
				// resume loudly (fail-closed) rather than decode wrong values. A
				// present positional always encodes at least the uvarint slot count,
				// so len(pp.B)==0 unambiguously means "no typed blob".
				if len(pp.B) == 0 {
					return nil, nil, fmt.Errorf("sorted record %d has no typed slot blob (a legacy sort continuation predating the typed slot codec); its slots cannot be resumed losslessly — restart the query", i)
				}
				slots, _, sErr := readContSlice(pp.B)
				if sErr != nil {
					return nil, nil, fmt.Errorf("failed to decode sorted record %d slots in continuation: %w", i, sErr)
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
