package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func (b *boundScanRangeSet) spec() scanRangeSetSpec {
	counts := make([]uint32, len(b.components))
	for i := range b.components {
		counts[i] = uint32(len(b.components[i].alternatives))
	}
	// A plural tail is one more odometer dimension, so the continuation stays
	// flat and O(k) and the cursor needs no new concept to resume inside it.
	if len(b.tails) > 1 {
		counts = append(counts, uint32(len(b.tails)))
	}
	if b.empty {
		return scanRangeSetSpec{
			fingerprint:            b.fingerprint(),
			compatibleFingerprints: b.compatibleFingerprints(),
			alternativeCounts:      counts,
			empty:                  true,
		}
	}
	return scanRangeSetSpec{
		fingerprint:            b.fingerprint(),
		compatibleFingerprints: b.compatibleFingerprints(),
		alternativeCounts:      counts,
		materialize:            b.materialize,
		keyFilter:              b.keyFilter,
	}
}

func (b *boundScanRangeSet) compatibleFingerprints() [][]byte {
	if len(b.compatibleFingerprintSalts) == 0 {
		return nil
	}
	result := make([][]byte, 0, len(b.compatibleFingerprintSalts))
	for _, salt := range b.compatibleFingerprintSalts {
		fingerprint := b.fingerprintWithSalt(salt)
		if bytes.Equal(fingerprint, b.fingerprint()) {
			continue
		}
		duplicate := false
		for _, existing := range result {
			if bytes.Equal(existing, fingerprint) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, fingerprint)
		}
	}
	return result
}

func (b *boundScanRangeSet) materialize(choices []uint32) (recordlayer.TupleRange, error) {
	if b.empty {
		return recordlayer.TupleRange{}, fmt.Errorf("cannot materialize an empty scan range set")
	}
	wantArity := len(b.components)
	if len(b.tails) > 1 {
		wantArity++
	}
	if len(choices) != wantArity {
		return recordlayer.TupleRange{}, fmt.Errorf(
			"scan range choices have arity %d, want %d", len(choices), wantArity,
		)
	}
	tail := b.tails[0]
	if len(b.tails) > 1 {
		tailChoice := choices[len(b.components)]
		if tailChoice >= uint32(len(b.tails)) {
			return recordlayer.TupleRange{}, fmt.Errorf(
				"scan range tail choice %d is out of bounds (tails=%d)",
				tailChoice, len(b.tails),
			)
		}
		tail = b.tails[tailChoice]
		choices = choices[:len(b.components)]
	}

	prefix := make(tuple.Tuple, len(b.components))
	for i, component := range b.components {
		choice := choices[i]
		if choice >= uint32(len(component.alternatives)) {
			return recordlayer.TupleRange{}, fmt.Errorf(
				"scan range choice %d for component %d is out of bounds (alternatives=%d)",
				choice, i, len(component.alternatives),
			)
		}
		prefix[i] = component.alternatives[choice]
	}

	if b.terminalZeroIndex >= 0 {
		i := b.terminalZeroIndex
		base := append(tuple.Tuple(nil), prefix[:i]...)
		zero := prefix[i]
		low := append(append(tuple.Tuple(nil), base...), negativeZeroLike(zero))
		high := append(append(tuple.Tuple(nil), base...), positiveZeroLike(zero))
		return recordlayer.TupleRange{
			Low: low, High: high,
			LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
			HighEndpoint: recordlayer.EndpointTypeRangeInclusive,
		}, nil
	}

	switch tail.kind {
	case boundRangeTailAllOf:
		return recordlayer.TupleRangeAllOf(prefix), nil
	case boundRangeTailEmpty:
		return recordlayer.TupleRange{
			Low: prefix, High: prefix,
			LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
			HighEndpoint: recordlayer.EndpointTypeRangeExclusive,
		}, nil
	case boundRangeTailStartsWith:
		start := append(append(tuple.Tuple(nil), prefix...), tail.startsWith)
		return recordlayer.TupleRange{
			Low: start, High: start,
			LowEndpoint:  recordlayer.EndpointTypePrefixString,
			HighEndpoint: recordlayer.EndpointTypePrefixString,
		}, nil
	case boundRangeTailInequality:
		return b.materializeInequality(prefix, tail), nil
	default:
		return recordlayer.TupleRange{}, fmt.Errorf("unknown bound scan tail kind %d", tail.kind)
	}
}

func (b *boundScanRangeSet) materializeInequality(
	prefix tuple.Tuple,
	tail boundRangeTail,
) recordlayer.TupleRange {
	lowEndpoint := recordlayer.EndpointTypeTreeStart
	highEndpoint := recordlayer.EndpointTypeTreeEnd
	if len(prefix) > 0 {
		lowEndpoint = recordlayer.EndpointTypeRangeInclusive
		highEndpoint = recordlayer.EndpointTypeRangeInclusive
	}
	if tail.hasLow {
		lowEndpoint = tail.lowEndpoint
	}
	if tail.hasHigh {
		highEndpoint = tail.highEndpoint
	}

	var low, high tuple.Tuple
	switch {
	case tail.hasLow && tail.lowItem != nil:
		low = append(append(tuple.Tuple(nil), prefix...), tail.lowItem)
	case tail.hasLow && tail.lowIsNullBoundary:
		low = append(append(tuple.Tuple(nil), prefix...), nil)
	case len(prefix) > 0:
		low = append(tuple.Tuple(nil), prefix...)
	}
	if tail.hasHigh && tail.highItem != nil {
		high = append(append(tuple.Tuple(nil), prefix...), tail.highItem)
	} else if tail.hasHigh && tail.highIsNullBoundary {
		high = append(append(tuple.Tuple(nil), prefix...), nil)
	} else if len(prefix) > 0 {
		high = append(tuple.Tuple(nil), prefix...)
	}
	return recordlayer.TupleRange{
		Low: low, High: high,
		LowEndpoint: lowEndpoint, HighEndpoint: highEndpoint,
	}
}

func (b *boundScanRangeSet) fingerprint() []byte {
	return b.fingerprintWithSalt(b.fingerprintSalt)
}

func (b *boundScanRangeSet) fingerprintWithSalt(salt string) []byte {
	h := sha256.New()
	writeFingerprintBytes(h, []byte("scan-range-set-v1"))
	writeFingerprintBytes(h, []byte(salt))
	if b.reverse {
		writeFingerprintBytes(h, []byte{1})
	} else {
		writeFingerprintBytes(h, []byte{0})
	}
	if b.empty {
		writeFingerprintBytes(h, []byte{1})
	} else {
		writeFingerprintBytes(h, []byte{0})
	}
	writeFingerprintUint32(h, uint32(b.terminalZeroIndex+1))
	writeFingerprintUint32(h, uint32(len(b.components)))
	for _, component := range b.components {
		code := values.TypeCodeUnknown
		if component.physicalType != nil {
			code = component.physicalType.Code()
		}
		writeFingerprintUint32(h, uint32(code))
		writeFingerprintUint32(h, uint32(len(component.alternatives)))
		for _, alternative := range component.alternatives {
			writeFingerprintBytes(h, tuple.Tuple{alternative}.Pack())
		}
	}
	writeFingerprintUint32(h, uint32(len(b.tails)))
	for _, t := range b.tails {
		writeFingerprintUint32(h, uint32(t.kind))
		writeFingerprintTailBounds(h, t)
	}
	// Only a filtered scan folds its filter, so every unfiltered scan keeps
	// the fingerprint its continuations already carry.
	if b.keyFilter != nil {
		writeFingerprintBytes(h, []byte("key-filter-v1"))
		writeFingerprintUint32(h, uint32(len(b.keyFilter.components)))
		for _, component := range b.keyFilter.components {
			writeFingerprintUint32(h, uint32(component.position))
			writeFingerprintUint32(h, uint32(len(component.comparisons)))
			for _, comparison := range component.comparisons {
				writeFingerprintUint32(h, uint32(comparison.Type))
				writeFingerprintBytes(h, []byte(fmt.Sprintf("%T", keyFilterOperandIdentity(comparison))))
				writeFingerprintBytes(h, []byte(fmt.Sprintf("%#v", keyFilterOperandIdentity(comparison))))
			}
		}
	}
	if !b.empty {
		choices := make([]uint32, len(b.components))
		if len(b.tails) > 1 {
			choices = append(choices, 0)
		}
		if materialized, err := b.materialize(choices); err == nil {
			writeFingerprintBytes(h, materialized.Low.Pack())
			writeFingerprintBytes(h, materialized.High.Pack())
			writeFingerprintUint32(h, uint32(materialized.LowEndpoint))
			writeFingerprintUint32(h, uint32(materialized.HighEndpoint))
		}
	}
	return h.Sum(nil)
}

// writeFingerprintTailBounds folds one tail's endpoints into the continuation
// fingerprint. Every tail must contribute: a plural float tail differs from its
// siblings only in these bounds, so hashing the kind alone would let a resumed
// scan reopen a DIFFERENT range under a fingerprint it considers unchanged.
func writeFingerprintTailBounds(h hash.Hash, t boundRangeTail) {
	writeFingerprintBool(h, t.floatOrdered)
	writeFingerprintBool(h, t.hasLow)
	writeFingerprintBool(h, t.lowIsNullBoundary)
	writeFingerprintUint32(h, uint32(t.lowEndpoint))
	writeFingerprintBytes(h, tuple.Tuple{t.lowItem}.Pack())
	writeFingerprintBool(h, t.hasHigh)
	writeFingerprintBool(h, t.highIsNullBoundary)
	writeFingerprintUint32(h, uint32(t.highEndpoint))
	writeFingerprintBytes(h, tuple.Tuple{t.highItem}.Pack())
	writeFingerprintBytes(h, tuple.Tuple{t.startsWith}.Pack())
}

func writeFingerprintBool(h hash.Hash, value bool) {
	if value {
		writeFingerprintBytes(h, []byte{1})
		return
	}
	writeFingerprintBytes(h, []byte{0})
}

func writeFingerprintBytes(h hash.Hash, value []byte) {
	writeFingerprintUint32(h, uint32(len(value)))
	_, _ = h.Write(value)
}

func writeFingerprintUint32(h hash.Hash, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = h.Write(encoded[:])
}
