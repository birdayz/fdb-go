// Portions derived from FoundationDB Record Layer (SemanticAnalyzer.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

import "strings"

// ResolveAssignmentColumn resolves one name of a DML statement's assigned field
// (a segment of an UPDATE's SET column: a qualifier, a column or a struct
// field) among the names at that level, to its position. names are user
// identifiers: a descriptor field's storage name decoded once
// (FieldNameForProtoField). The caller (the SET column's resolution) walks
// the segments level by level, as Java's SemanticAnalyzer resolves the
// identifier against the target Type.Record, and the FieldPath it builds
// carries the resolved ordinals RecordQueryUpdatePlan keys its transformation
// trie by, so nothing looks the name up again. Nothing here may decode a name
// a second time: the storage escaping is not injective under it (`a$b` is
// stored as a__1b, `a__1b` as a__01b, and decoding `a__1b` again gives `a$b`).
//
// column is normalized (a quoted identifier keeps its case, an unquoted one is
// upper case). An exact match wins; otherwise one case-insensitive match does,
// the lookup extension DIVERGENCES.md declares ("Identifier resolution: Go
// over-resolves case"). hits is the number of columns the deciding pass
// matched: 1 resolved (idx is its position), 0 no such column, more than one
// ambiguous.
func ResolveAssignmentColumn(names []string, column string) (idx, hits int) {
	if column == "" {
		return -1, 0
	}
	idx = -1
	for i, n := range names {
		if n == column {
			hits++
			idx = i
		}
	}
	if hits > 0 {
		if hits > 1 {
			return -1, hits
		}
		return idx, 1
	}
	for i, n := range names {
		if strings.EqualFold(n, column) {
			hits++
			idx = i
		}
	}
	if hits != 1 {
		return -1, hits
	}
	return idx, 1
}
