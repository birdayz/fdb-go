// Portions derived from FoundationDB Record Layer (RelationalException.java),
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package api

import "fdb.dev/pkg/relational/sqlapi"

// Error is the Go equivalent of Java's
// com.apple.foundationdb.relational.api.exceptions.RelationalException.
type Error = sqlapi.Error

// RelationalException aliases Error to match Java's RelationalException name.
type RelationalException = sqlapi.RelationalException

// NewError returns a new Error with the given code and message.
func NewError(code ErrorCode, message string) *Error {
	return sqlapi.NewError(code, message)
}

// NewErrorf is NewError with fmt.Sprintf formatting.
func NewErrorf(code ErrorCode, format string, args ...any) *Error {
	return sqlapi.NewErrorf(code, format, args...)
}

// WrapError wraps cause with a relational error code and message.
func WrapError(code ErrorCode, message string, cause error) *Error {
	return sqlapi.WrapError(code, message, cause)
}

// WrapStatedError is WrapError for a message that already states the cause.
func WrapStatedError(code ErrorCode, message string, cause error) *Error {
	return sqlapi.WrapStatedError(code, message, cause)
}

// WrapErrorf is WrapError with fmt.Sprintf formatting for the message.
func WrapErrorf(cause error, code ErrorCode, format string, args ...any) *Error {
	return sqlapi.WrapErrorf(cause, code, format, args...)
}

// AsError extracts an *Error from the chain, or returns nil if there is none.
func AsError(err error) *Error {
	return sqlapi.AsError(err)
}

// ErrorCodeOf returns the ErrorCode from the first *Error in err's chain,
// or empty string if err does not carry one.
func ErrorCodeOf(err error) ErrorCode {
	return sqlapi.ErrorCodeOf(err)
}

// IsRetryable reports whether err carries a SQLSTATE code whose transaction
// definitely did not commit and is safe to retry in a new transaction.
func IsRetryable(err error) bool {
	return sqlapi.IsRetryable(err)
}
