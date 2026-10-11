// Portions derived from FoundationDB Record Layer (KeyValueCursorBase.java, TupleRange.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// kvScanBounds is the state KeyValueCursorBase.Builder.prepare derives from a
// range (KeyValueCursorBase.java:296-346): the packed bounds, their endpoint
// types, and the prefix length continuations strip.
type kvScanBounds struct {
	lowBytes, highBytes       []byte
	lowEndpoint, highEndpoint EndpointType
	prefixLength              int
}

func newKVScanBounds(ss subspace.Subspace, low, high tuple.Tuple, lowEndpoint, highEndpoint EndpointType) kvScanBounds {
	b := kvScanBounds{
		lowBytes:     packScanBound(ss, low),
		highBytes:    packScanBound(ss, high),
		lowEndpoint:  lowEndpoint,
		highEndpoint: highEndpoint,
	}
	b.prefixLength = continuationPrefixLength(len(ss.Bytes()), b.lowBytes, b.highBytes)
	// Both PREFIX_STRING: the prefix stops before the packed string's \x00 terminator.
	if lowEndpoint == EndpointTypePrefixString && highEndpoint == EndpointTypePrefixString {
		b.prefixLength--
	}
	return b
}

// keyRange is the byte range to read. A continuation's inner bytes follow
// lowBytes[:prefixLength] and replace the bound the scan direction resumes from.
func (b kvScanBounds) keyRange(continuation []byte, reverse bool) (fdb.KeyRange, error) {
	lowBytes, highBytes := b.lowBytes, b.highBytes
	lowEndpoint, highEndpoint := b.lowEndpoint, b.highEndpoint
	if continuation != nil {
		if b.prefixLength < 0 {
			return fdb.KeyRange{}, fmt.Errorf("PREFIX_STRING scan bound %x has no string to prefix", lowBytes)
		}
		inner := unwrapContinuation(continuation)
		resume := make([]byte, 0, b.prefixLength+len(inner))
		resume = append(append(resume, lowBytes[:b.prefixLength]...), inner...)
		if reverse {
			highBytes, highEndpoint = resume, EndpointTypeContinuation
		} else {
			lowBytes, lowEndpoint = resume, EndpointTypeContinuation
		}
	}
	return tupleBoundsToRange(lowBytes, highBytes, lowEndpoint, highEndpoint)
}

// keySuffix is Java's lastKey.substring(prefixLength) (KeyValueCursorBase.java:233-235):
// a key equal to the prefix is an empty but present suffix. Every key a scan returns
// starts with lowBytes[:prefixLength], so a shorter key is a broken invariant, which
// Java's substring also rejects.
func keySuffix(key []byte, prefixLength int) ([]byte, error) {
	if prefixLength < 0 || len(key) < prefixLength {
		return nil, fmt.Errorf("continuation key %x is shorter than the scan prefix length %d", key, prefixLength)
	}
	return key[prefixLength:], nil
}

// packScanBound is KeyValueCursorBase.Builder.setLow/setHigh(Tuple): a nil bound
// is the bare subspace, whatever its endpoint type.
func packScanBound(ss subspace.Subspace, t tuple.Tuple) []byte {
	if t == nil {
		return bytes.Clone(ss.Bytes())
	}
	return ss.Pack(t)
}

// continuationPrefixLength is KeyValueCursorBase.Builder.calculatePrefixLength
// (KeyValueCursorBase.java:425-432): the subspace plus the common prefix of the
// packed bounds.
func continuationPrefixLength(subspaceLength int, lowBytes, highBytes []byte) int {
	n := subspaceLength
	for n < len(lowBytes) && n < len(highBytes) && lowBytes[n] == highBytes[n] {
		n++
	}
	return n
}

// tupleBoundsToRange is TupleRange.toRange(byte[], byte[], EndpointType, EndpointType)
// (TupleRange.java:471-532).
func tupleBoundsToRange(lowBytes, highBytes []byte, lowEndpoint, highEndpoint EndpointType) (fdb.KeyRange, error) {
	if lowEndpoint == EndpointTypePrefixString || highEndpoint == EndpointTypePrefixString {
		if err := verifyPrefixStringSemantics(lowBytes, highBytes, lowEndpoint, highEndpoint); err != nil {
			return fdb.KeyRange{}, err
		}
	}
	switch lowEndpoint {
	case EndpointTypeTreeStart, EndpointTypeRangeInclusive:
	case EndpointTypeRangeExclusive:
		inc, err := fdb.Strinc(lowBytes)
		if err != nil {
			return fdb.KeyRange{}, fmt.Errorf("strinc for exclusive low endpoint: %w", err)
		}
		lowBytes = inc
	case EndpointTypeContinuation:
		// The very next key, including prefix matches.
		lowBytes = append(lowBytes[:len(lowBytes):len(lowBytes)], 0x00)
	case EndpointTypePrefixString:
		lowBytes = lowBytes[:len(lowBytes)-1]
	default:
		return fdb.KeyRange{}, fmt.Errorf("incorrect low endpoint: %d", lowEndpoint)
	}
	switch highEndpoint {
	case EndpointTypeRangeExclusive, EndpointTypeContinuation:
	case EndpointTypeTreeEnd, EndpointTypeRangeInclusive:
		// 0xFF escapes a \x00 inside packed strings/bytes, so such suffixed keys
		// sort after the high tuple and stay outside a tuple-oriented range.
		highBytes = append(highBytes[:len(highBytes):len(highBytes)], 0xFF)
	case EndpointTypePrefixString:
		n := len(highBytes) - 1
		for n >= 1 && highBytes[n-1] == 0xFF {
			n--
		}
		if n == 0 {
			highBytes = []byte{0xFF}
		} else {
			dest := bytes.Clone(highBytes[:n])
			dest[n-1]++
			highBytes = dest
		}
	default:
		return fdb.KeyRange{}, fmt.Errorf("incorrect high endpoint: %d", highEndpoint)
	}
	return fdb.KeyRange{Begin: fdb.Key(lowBytes), End: fdb.Key(highBytes)}, nil
}

// verifyPrefixStringSemantics is TupleRange.verifyPrefixStringSemantics
// (TupleRange.java:534-578).
func verifyPrefixStringSemantics(lowBytes, highBytes []byte, lowEndpoint, highEndpoint EndpointType) error {
	if lowBytes == nil || highBytes == nil {
		return fmt.Errorf("PREFIX_STRING must specify non-null endpoints")
	}
	isPrefixOrCont := func(e EndpointType) bool {
		return e == EndpointTypePrefixString || e == EndpointTypeContinuation
	}
	if !isPrefixOrCont(lowEndpoint) || !isPrefixOrCont(highEndpoint) {
		return fmt.Errorf("PREFIX_STRING must be specified for both low and high endpoints")
	}
	var bound []byte
	switch {
	case lowEndpoint == EndpointTypeContinuation:
		if !hasCommonPrefix(lowBytes, highBytes, len(highBytes)-1) {
			return fmt.Errorf("PREFIX_STRING continuation is not consistent with low endpoint")
		}
		bound = highBytes
	case highEndpoint == EndpointTypeContinuation:
		if !hasCommonPrefix(highBytes, lowBytes, len(lowBytes)-1) {
			return fmt.Errorf("PREFIX_STRING continuation is not consistent with high endpoint")
		}
		bound = lowBytes
	default:
		if !bytes.Equal(lowBytes, highBytes) {
			return fmt.Errorf("PREFIX_STRING must be provide identical string prefixes")
		}
		bound = lowBytes
	}
	if len(bound) == 0 || bound[len(bound)-1] != 0 {
		return fmt.Errorf("PREFIX_STRING bound %x is not a string", bound)
	}
	return nil
}

// hasCommonPrefix is ByteArrayUtil2.hasCommonPrefix: the first n bytes of b are a prefix of a.
func hasCommonPrefix(a, b []byte, n int) bool {
	n = max(n, 0)
	return n <= len(a) && n <= len(b) && bytes.Equal(a[:n], b[:n])
}
