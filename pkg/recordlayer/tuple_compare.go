// Portions derived from the FoundationDB Java bindings (TupleUtil.java,
// IterableComparator.java, Versionstamp.java),
// Copyright 2013-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"math/big"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// CompareTuples orders two tuples as Java's Tuple.compareTo does: -1, 0 or 1.
func CompareTuples(a, b tuple.Tuple) int { return compareTuples(a, b) }

// compareTuples orders two tuples as Java's Tuple.compareTo does for tuples
// without cached packed bytes: element by element with TupleUtil.compareItems,
// a shorter tuple first when it is a prefix of the longer
// (IterableComparator.compare). Nothing is encoded. The order is the order of
// the tuples' packed bytes, except that this also orders tuples Pack refuses
// (incomplete versionstamps, out-of-range integers), as Java does.
// Returns -1, 0 or 1.
func compareTuples(a, b tuple.Tuple) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareTupleItems(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// tupleItemCode is TupleUtil.getCodeFor: the type code that orders items of
// different types. Every integer type shares the code of zero, and both
// booleans that of false.
func tupleItemCode(item tuple.TupleElement) int {
	switch item.(type) {
	case nil:
		return 0x00
	case []byte:
		return 0x01
	case string:
		return 0x02
	case tuple.Tuple:
		return 0x05
	case int, int64, uint, uint64, *big.Int, big.Int:
		return 0x14
	case float32:
		return 0x20
	case float64:
		return 0x21
	case bool:
		return 0x26
	case tuple.UUID:
		return 0x30
	case tuple.Versionstamp:
		return 0x33
	case fdb.KeyConvertible:
		// Packed as bytes (tuple.go encodeElement); a Tuple matched above.
		return 0x01
	}
	panic(fmt.Sprintf("unsupported tuple element type %T", item))
}

// compareTupleItems is TupleUtil.compareItems (TupleUtil.java:592-668).
func compareTupleItems(a, b tuple.TupleElement) int {
	codeA, codeB := tupleItemCode(a), tupleItemCode(b)
	if codeA != codeB {
		return cmp.Compare(codeA, codeB)
	}
	switch codeA {
	case 0x00:
		return 0
	case 0x01:
		return bytes.Compare(tupleItemBytes(a), tupleItemBytes(b))
	case 0x02:
		// StringUtil.compareUtf8: the order of the UTF-8 encodings.
		return strings.Compare(a.(string), b.(string))
	case 0x05:
		return compareTuples(a.(tuple.Tuple), b.(tuple.Tuple))
	case 0x14:
		return compareTupleIntegers(a, b)
	case 0x20:
		return cmp.Compare(tupleFloatBits(a.(float32)), tupleFloatBits(b.(float32)))
	case 0x21:
		return cmp.Compare(tupleDoubleBits(a.(float64)), tupleDoubleBits(b.(float64)))
	case 0x26:
		// Boolean.compare: false before true.
		x, y := a.(bool), b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		default:
			return 1
		}
	case 0x30:
		// Unsigned most then least significant bits: the bytes in order.
		x, y := a.(tuple.UUID), b.(tuple.UUID)
		return bytes.Compare(x[:], y[:])
	default:
		return compareVersionstamps(a.(tuple.Versionstamp), b.(tuple.Versionstamp))
	}
}

func tupleItemBytes(item tuple.TupleElement) []byte {
	if b, ok := item.([]byte); ok {
		return b
	}
	return item.(fdb.KeyConvertible).FDBKey()
}

// tupleFloatBits and tupleDoubleBits are TupleUtil.encodeFloatBits and
// encodeDoubleBits (TupleUtil.java:179-187), compared unsigned so that NaNs
// order as their encodings do.
func tupleFloatBits(f float32) uint32 {
	bits := math.Float32bits(f)
	if bits&(1<<31) != 0 {
		return ^bits
	}
	return bits ^ (1 << 31)
}

func tupleDoubleBits(d float64) uint64 {
	bits := math.Float64bits(d)
	if bits&(1<<63) != 0 {
		return ^bits
	}
	return bits ^ (1 << 63)
}

// compareTupleIntegers orders integers of any width by value: Long.compare
// when both fit a long, BigInteger.compareTo otherwise.
func compareTupleIntegers(a, b tuple.TupleElement) int {
	x, xSmall := tupleSmallInt(a)
	y, ySmall := tupleSmallInt(b)
	if xSmall && ySmall {
		return cmp.Compare(x, y)
	}
	return tupleBigInt(a).Cmp(tupleBigInt(b))
}

// tupleSmallInt is the value of an integer element that fits an int64.
func tupleSmallInt(item tuple.TupleElement) (int64, bool) {
	switch v := item.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), uint64(v) <= math.MaxInt64
	case uint64:
		return int64(v), v <= math.MaxInt64
	case *big.Int:
		return v.Int64(), v.IsInt64()
	case big.Int:
		return v.Int64(), v.IsInt64()
	}
	return 0, false
}

func tupleBigInt(item tuple.TupleElement) *big.Int {
	switch v := item.(type) {
	case int:
		return big.NewInt(int64(v))
	case int64:
		return big.NewInt(v)
	case uint:
		return new(big.Int).SetUint64(uint64(v))
	case uint64:
		return new(big.Int).SetUint64(v)
	case *big.Int:
		return v
	default:
		bi := item.(big.Int)
		return &bi
	}
}

// compareVersionstamps is Versionstamp.compareTo (Versionstamp.java:306-318):
// complete ones by their bytes, an incomplete one after every complete one,
// incomplete ones by user version. Go marks an incomplete versionstamp by its
// all-0xFF transaction version (tuple.IncompleteVersionstamp).
func compareVersionstamps(a, b tuple.Versionstamp) int {
	incompleteA, incompleteB := versionstampIncomplete(a), versionstampIncomplete(b)
	switch {
	case !incompleteA && !incompleteB:
		if c := bytes.Compare(a.TransactionVersion[:], b.TransactionVersion[:]); c != 0 {
			return c
		}
		return cmp.Compare(a.UserVersion, b.UserVersion)
	case !incompleteA:
		return -1
	case !incompleteB:
		return 1
	default:
		return cmp.Compare(a.UserVersion, b.UserVersion)
	}
}

func versionstampIncomplete(v tuple.Versionstamp) bool {
	for _, b := range v.TransactionVersion {
		if b != 0xFF {
			return false
		}
	}
	return true
}
