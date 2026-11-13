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

// tupleOrderingPack matches Java's TupleOrdering.pack(Tuple, Direction).
// An incomplete versionstamp is a recoverable error, not a vanilla tuple-pack panic.
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
