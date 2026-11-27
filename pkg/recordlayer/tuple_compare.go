// Portions derived from the FoundationDB Java bindings (TupleUtil.java,
// IterableComparator.java, Versionstamp.java),
// Copyright 2013-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fdb.dev/internal/recordlayer/rlcollections"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// TupleComparisonError identifies an unsupported public comparator argument.
type TupleComparisonError = rlcollections.TupleComparisonError

// CompareTuples orders valid tuples as Java's Tuple.compareTo does: -1, 0 or 1.
// Unsupported elements return contextual errors rather than panicking.
func CompareTuples(a, b tuple.Tuple) (int, error) {
	return rlcollections.CompareTuples(a, b)
}

func compareTuples(a, b tuple.Tuple) int {
	return rlcollections.CompareTuplesUnchecked(a, b)
}
