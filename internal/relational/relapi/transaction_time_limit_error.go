// Portions derived from FoundationDB Record Layer (ExceptionUtil.java,
// ErrorCode.java, FDBExceptions.java, LuceneExceptions.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package api

import (
	"time"

	"fdb.dev/pkg/relational/sqlapi"
)

// TimeLimitSource names which ceiling stopped the transaction.
type TimeLimitSource = sqlapi.TimeLimitSource

const (
	// TimeLimitPreempted is the driver's read-budget pre-emption.
	TimeLimitPreempted TimeLimitSource = sqlapi.TimeLimitPreempted
	// TimeLimitFDBTooOld is FDB's own transaction_too_old (1007).
	TimeLimitFDBTooOld TimeLimitSource = sqlapi.TimeLimitFDBTooOld
)

// TransactionTimeLimitError marks an error as the driver pre-empting an explicit
// transaction whose reads have outlived FDB's 5-second MVCC window.
type TransactionTimeLimitError = sqlapi.TransactionTimeLimitError

// NewTransactionTimeLimitError builds the 40001 the driver returns when an
// explicit transaction's read budget is exhausted.
func NewTransactionTimeLimitError(elapsed, limit time.Duration) *Error {
	return sqlapi.NewTransactionTimeLimitError(elapsed, limit)
}

// MarkFDBTransactionTooOld wraps FDB's own transaction_too_old (1007) with
// the TransactionTimeLimitError marker.
func MarkFDBTransactionTooOld(cause error) *Error {
	return sqlapi.MarkFDBTransactionTooOld(cause)
}

// IsTransactionTimeLimit reports whether err says the transaction outlived FDB's MVCC window.
func IsTransactionTimeLimit(err error) bool {
	return sqlapi.IsTransactionTimeLimit(err)
}
