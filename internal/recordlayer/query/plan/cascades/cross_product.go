// Portions derived from FoundationDB Record Layer (CrossProduct.java),
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

// CrossProduct computes the Cartesian product of N lists. Each list
// provides alternatives for one "dimension"; the result is every
// combination of picking one element from each list.
//
// For input [[a, b], [x, y]], returns [[a, x], [a, y], [b, x], [b, y]].
// Empty input returns nil. Any empty inner list makes the product empty.
//
// Mirrors Java's CrossProduct.crossProduct utility.
func CrossProduct[T any](lists [][]T) [][]T {
	if len(lists) == 0 {
		return nil
	}
	result := [][]T{{}}
	for _, list := range lists {
		if len(list) == 0 {
			return nil
		}
		var next [][]T
		for _, existing := range result {
			for _, elem := range list {
				combo := make([]T, len(existing)+1)
				copy(combo, existing)
				combo[len(existing)] = elem
				next = append(next, combo)
			}
		}
		result = next
	}
	return result
}
