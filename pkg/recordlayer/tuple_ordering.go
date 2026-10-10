// Portions derived from FoundationDB Record Layer (TupleOrdering.java),
// Copyright 2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/tupleordering"
)

// OrderDirection specifies how tuple values are encoded for ordered storage.
// Matches Java's TupleOrdering.Direction enum.
type OrderDirection = tupleordering.Direction

// The four order directions matching Java's TupleOrdering.Direction enum values.
var (
	// OrderAscNullsFirst: ascending order, nulls sort first (default tuple behavior).
	OrderAscNullsFirst = tupleordering.AscNullsFirst
	// OrderAscNullsLast: ascending order, nulls sort last (0xFE encoding).
	OrderAscNullsLast = tupleordering.AscNullsLast
	// OrderDescNullsFirst: descending order, nulls sort first.
	OrderDescNullsFirst = tupleordering.DescNullsFirst
	// OrderDescNullsLast: descending order, nulls sort last.
	OrderDescNullsLast = tupleordering.DescNullsLast
)

// tupleOrderingPack encodes a tuple according to the given direction.
// Matches Java's TupleOrdering.pack(Tuple, Direction).
//
// Both packing branches are VANILLA packs and therefore cannot represent an
// incomplete versionstamp: Java's packNullsLast raises
// IllegalArgumentException("Incomplete Versionstamp included in vanilla tuple
// pack") when one reaches it (TupleOrdering.java:174-180) and Tuple.pack does
// the same. Go's tuple.Pack PANICS in that situation, so the check happens here
// and the caller gets Java's exception as a Go error. An ordering wrapper over
// the __ROW_VERSION pseudo-field (`ORDER BY "__ROW_VERSION" DESC`) is the shape
// that reaches this from ordinary DDL.
func tupleOrderingPack(t tuple.Tuple, dir OrderDirection) ([]byte, error) {
	if tupleHasIncompleteVersionstamp(t) {
		return nil, &IncompleteVersionstampError{Context: "tuple ordering"}
	}
	return tupleordering.Pack(t, dir), nil
}

// tupleOrderingUnpack decodes bytes back into a tuple according to the given direction.
func tupleOrderingUnpack(packed []byte, dir OrderDirection) (tuple.Tuple, error) {
	return tupleordering.Unpack(packed, dir)
}

func packNullsLast(t tuple.Tuple) []byte               { return tupleordering.PackNullsLast(t) }
func unpackNullsLast(data []byte) (tuple.Tuple, error) { return tupleordering.UnpackNullsLast(data) }
func invertBytes(input []byte) []byte                  { return tupleordering.Invert(input) }
func uninvertBytes(inverted []byte) ([]byte, error)    { return tupleordering.Uninvert(inverted) }

const nullLastByte = byte(0xFE)

func tupleElementEndPos(data []byte, pos int) (int, error) {
	return tupleordering.ElementEndPos(data, pos)
}
