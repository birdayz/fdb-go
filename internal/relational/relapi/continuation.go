// Portions derived from FoundationDB Record Layer (Continuation.java),
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package api

import "fdb.dev/pkg/relational/sqlapi"

// Continuation is a serialized pointer into a result set that a client
// can use to resume iteration in a later transaction.
type Continuation = sqlapi.Continuation

// ContinuationReason tags the cause of a continuation.
type ContinuationReason = sqlapi.ContinuationReason

const (
	// ContinuationUserRequested means getContinuation was called
	// before the ResultSet was exhausted.
	ContinuationUserRequested ContinuationReason = sqlapi.ContinuationUserRequested
	// ContinuationTransactionLimitReached fires when the byte / row /
	// time scan budget inside a transaction was hit.
	ContinuationTransactionLimitReached ContinuationReason = sqlapi.ContinuationTransactionLimitReached
	// ContinuationQueryExecutionLimitReached fires when a per-query
	// row cap was hit.
	ContinuationQueryExecutionLimitReached ContinuationReason = sqlapi.ContinuationQueryExecutionLimitReached
	// ContinuationCursorAfterLast indicates the result set was fully
	// exhausted (no more rows).
	ContinuationCursorAfterLast ContinuationReason = sqlapi.ContinuationCursorAfterLast
)

// AtBeginning reports whether the continuation is at the beginning of the cursor.
func AtBeginning(c Continuation) bool {
	return sqlapi.AtBeginning(c)
}

// AtEnd reports whether the continuation is past the last row.
func AtEnd(c Continuation) bool {
	return sqlapi.AtEnd(c)
}
