// Portions derived from FoundationDB Record Layer (RangeSet.java, KeyRange.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fdb.dev/internal/recordlayer/rlcollections"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

var (
	rangeSetFirstKey = rlcollections.RangeSetFirstKey
	rangeSetFinalKey = rlcollections.RangeSetFinalKey
)

type (
	RangeSet                   = rlcollections.RangeSet
	RangeSetRange              = rlcollections.RangeSetRange
	RangeSetEmptyKeyError      = rlcollections.RangeSetEmptyKeyError
	RangeSetKeyTooLargeError   = rlcollections.RangeSetKeyTooLargeError
	RangeSetInvertedRangeError = rlcollections.RangeSetInvertedRangeError
)

// NewRangeSet creates a RangeSet that stores data in the given subspace.
func NewRangeSet(ss subspace.Subspace) *RangeSet {
	return rlcollections.NewRangeSet(ss)
}
