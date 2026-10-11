// Portions derived from FoundationDB Record Layer (ExceptionUtil.java,
// FDBExceptions.java), Copyright 2015-2024 Apple Inc. and the FoundationDB
// project authors. Licensed under the Apache License, Version 2.0; translated to Go and modified.

package api

import (
	"errors"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire"
)

// FDBErrorCode returns the FDB error code err's chain carries, in either form.
func FDBErrorCode(err error) (int, bool) {
	var fe fdb.Error
	if errors.As(err, &fe) {
		return fe.Code, true
	}
	var we *wire.FDBError
	if errors.As(err, &we) {
		return we.Code, true
	}
	return 0, false
}

// TranslateFDBError gives an FDB-coded error its SQLSTATE, the single mapping
// every relational entry point uses; an unmapped code is returned unchanged.
func TranslateFDBError(err error) error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return err
	}
	code, ok := FDBErrorCode(err)
	if !ok {
		return err
	}
	switch code {
	case 1031: // transaction_timed_out
		return WrapError(ErrCodeTransactionTimeout, "FDB transaction timed out", err)
	case 1020: // not_committed
		return WrapError(ErrCodeSerializationFailure, "FDB transaction conflict", err)
	case 1007: // transaction_too_old
		// SAME condition as paginatingRows.preflightTxBudget's pre-emption — the
		// transaction outlived FDB's 5-second MVCC window — reached by the other
		// of its two producers, so it carries the SAME marker. 40001 alone cannot
		// say which condition occurred, because 1020 above maps here too; the
		// marker is what separates a lost window from a genuine conflict, and
		// attaching it at both producers is what stops a consumer from having to
		// enumerate spellings (TransactionTimeLimitError).
		return MarkFDBTransactionTooOld(err)
	case 1021: // commit_unknown_result
		// 40003, NOT 40001: the transaction may or may not have committed and
		// the client cannot tell. The application must determine the outcome
		// before retrying — a blind retry double-applies any non-idempotent
		// statement on the applied branch (RFC-198 Decision 7).
		return WrapError(ErrCodeStatementCompletionUnknown,
			"FDB commit outcome unknown: the transaction may or may not have committed; determine the outcome before retrying", err)
	case 1025: // transaction_cancelled
		return WrapError(ErrCodeTransactionInactive, "FDB transaction cancelled", err)
	case 2017: // used_during_commit
		return WrapError(ErrCodeTransactionInactive, "FDB transaction used during commit", err)
	}
	return err
}
