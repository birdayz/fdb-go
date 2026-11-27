// Portions derived from FoundationDB Record Layer (RecordSerializationException.java,
// KeyExpression.java, RecordCursor.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// UnsupportedFormatVersionError is returned when a store header contains a format
// version higher than the maximum version this code supports.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.UnsupportedFormatVersionException.
type UnsupportedFormatVersionError struct {
	Version    int32
	MaxVersion int32
}

func (e *UnsupportedFormatVersionError) Error() string {
	return fmt.Sprintf("unsupported format version %d (max supported: %d)", e.Version, e.MaxVersion)
}

func (*UnsupportedFormatVersionError) JavaRecordCoreException() {}

// UnsupportedFeatureForFormatVersionError is returned when a version-gated store
// feature is used on a store whose header format version predates it.
//
// Java raises a RecordCoreException with LogMessageKeys.FORMAT_VERSION at each
// such site — e.g. "Store does not support setting a store lock state"
// (FDBRecordStore.java:3478/:3494), "Store does not support updating record count
// state" (:3443), "Store does not support incarnation" (:3503/:3517), "cannot
// access header user fields at current format version" (:3222). The guard exists
// because these features WRITE new store-header fields: an instance still pinned
// at the older format can open the store and will simply not understand them, so
// a lock it cannot see is a lock it will not honour.
type UnsupportedFeatureForFormatVersionError struct {
	Feature         string
	Version         int32
	RequiredVersion int32
}

func (e *UnsupportedFeatureForFormatVersionError) Error() string {
	return fmt.Sprintf("store does not support %s at format version %d (requires >= %d)",
		e.Feature, e.Version, e.RequiredVersion)
}

func (*UnsupportedFeatureForFormatVersionError) JavaRecordCoreException() {}

// UnsupportedValueTypeError is the RecordCoreException Java's
// LiteralKeyExpression.toProtoValue throws for a value no Value field holds
// (LiteralKeyExpression.java:192-194), with its message: "Unsupported value
// type " and the value's Class (`class java.util.UUID`). JavaClass is the Java
// class the value would be in the target, or the Go type where it has none.
type UnsupportedValueTypeError struct {
	JavaClass string
}

func (e *UnsupportedValueTypeError) Error() string {
	return "Unsupported value type class " + e.JavaClass
}

func (*UnsupportedValueTypeError) JavaRecordCoreException() {}

// RecordSerializationError is Java's
// com.apple.foundationdb.record.provider.common.RecordSerializationException:
// a record's bytes could not be produced, or a transformation's could not be
// read back (a compression, encryption or prefix failure on either path).
// Message is the exception's message; one with only a cause (a protobuf
// marshal failure, Java's RecordSerializationException(Throwable)) renders as
// "failed to serialize record".
type RecordSerializationError struct {
	Message string
	Cause   error
	// RetryCount and RetryResult are the log keys Java's
	// decryptAndDecompressWithRetries adds to a read's failure: the configured
	// reattempt count and "failure" when every attempt failed, or the attempt
	// that succeeded and "success" when failOnDeserializeReattempt refuses a
	// reattempt's success. RetryResult is "" on every other error.
	RetryCount  int
	RetryResult string
	// MetaDataVersion is the reading store's metadata version on a refused
	// reattempt (Java's META_DATA_VERSION log key), when a store read it.
	MetaDataVersion int
	// generalSecurity marks the wrapping of a cipher failure, Java's
	// GeneralSecurityException, which the write-time encryption validation
	// catches and a key manager's own RecordSerializationException it does not.
	generalSecurity bool
}

func (e *RecordSerializationError) Error() string {
	switch {
	case e.Message != "" && e.Cause != nil:
		return e.Message + ": " + e.Cause.Error()
	case e.Message != "":
		return e.Message
	}
	return fmt.Sprintf("failed to serialize record: %v", e.Cause)
}

func (*RecordSerializationError) JavaRecordCoreException() {}

func (e *RecordSerializationError) Unwrap() error {
	return e.Cause
}

// RecordSerializationValidationError is Java's
// com.apple.foundationdb.record.provider.common.RecordSerializationValidationException:
// a sampled write-time validation found a record's stored bytes would not read
// back as the record ("cannot deserialize record", "record serialization
// mismatch") or its encryption would not decrypt to its input ("encryption
// validation error: ..."). Like Java's, it is a RecordCoreException and not a
// RecordSerializationException. RecordType and PrimaryKey are its log keys.
type RecordSerializationValidationError struct {
	Message    string
	RecordType string
	PrimaryKey tuple.Tuple
	Cause      error
}

func (e *RecordSerializationValidationError) Error() string {
	msg := e.Message
	if e.RecordType != "" {
		msg += fmt.Sprintf(" (record type %s, primary key %v)", e.RecordType, e.PrimaryKey)
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (*RecordSerializationValidationError) JavaRecordCoreException() {}

func (e *RecordSerializationValidationError) Unwrap() error {
	return e.Cause
}

// RecordDeserializationError is returned when a record fails to deserialize (unmarshal) from protobuf.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.RecordDeserializationException.
type RecordDeserializationError struct {
	PrimaryKey any // tuple.Tuple, but using any to avoid import cycle
	Cause      error
}

func (e *RecordDeserializationError) Error() string {
	if e.PrimaryKey != nil {
		return fmt.Sprintf("failed to deserialize record %v: %v", e.PrimaryKey, e.Cause)
	}
	return fmt.Sprintf("failed to deserialize record: %v", e.Cause)
}

func (*RecordDeserializationError) JavaRecordCoreException() {}

func (e *RecordDeserializationError) Unwrap() error {
	return e.Cause
}

// ContinuationParseError is returned when continuation bytes fail to parse as
// their wrapper proto or cursor-specific encoding. For proto wrappers, it matches
// Java's RecordCoreException("error parsing continuation") with the "raw_bytes"
// log info key (e.g. OrElseCursor's constructor, RecordCursor.fromList).
// Chained's bool-returning decoder cannot supply an underlying error, so Cause
// describes its rejection instead; Java propagates the decoder's exception.
// A corrupt continuation must surface, never silently become a fresh start.
//
// Message overrides the default "error parsing continuation" text for the Java
// classes whose RecordCoreException wording differs (ConcatCursor uses
// "Error parsing ConcatCursor continuation", DedupCursor uses
// "Error parsing continuation"). Leave empty for the common wording.
type ContinuationParseError struct {
	Message  string
	RawBytes []byte
	Cause    error
}

func (e *ContinuationParseError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "error parsing continuation"
	}
	return fmt.Sprintf("%s (raw_bytes=%x): %v", msg, e.RawBytes, e.Cause)
}

func (*ContinuationParseError) JavaRecordCoreException() {}

func (e *ContinuationParseError) Unwrap() error {
	return e.Cause
}

// ContinuationEncodeError reports a value whose encoder cannot supply a
// resumable continuation. Java requires a non-null ChainedCursor encoder and
// RecordCursorResult.withNextValue throws RecordCoreException if its encoding
// represents the end. Go reports these failures as errors rather than panics.
type ContinuationEncodeError struct {
	Message string
}

func (e *ContinuationEncodeError) Error() string {
	return e.Message
}

func (*ContinuationEncodeError) JavaRecordCoreException() {}

// QueryInvalidExpressionError is Java's
// com.apple.foundationdb.record.query.expressions.Query.InvalidExpressionException,
// an IllegalStateException (Query.java:273), not a RecordCoreException and not
// KeyExpression's: key validation raises it for a message field read as a
// scalar.
type QueryInvalidExpressionError struct {
	Message string
}

func (e *QueryInvalidExpressionError) Error() string {
	return e.Message
}

// KeyExpressionError is returned when a key expression evaluation fails.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.KeyExpression.InvalidExpressionException.
type KeyExpressionError struct {
	Message string
}

func (e *KeyExpressionError) Error() string {
	return e.Message
}

// KeyExpressionDeserializationError is Java's
// KeyExpression.DeserializationException (a RecordCoreException): a
// serialized key expression that cannot be read back, as one with no root or
// with several (KeyExpression.java:404-405), a field without its name or fan
// type (FieldKeyExpression.java:122-128), a nesting without its parent
// (NestingKeyExpression.java:69) or a then of fewer than two children
// (ThenKeyExpression.java:84).
type KeyExpressionDeserializationError struct {
	Message string
}

func (e *KeyExpressionDeserializationError) Error() string {
	return e.Message
}

func (*KeyExpressionDeserializationError) JavaRecordCoreException() {}

// Unwrap reports it as the RecordCoreError Java's exception is, so a caller
// matching RecordCoreError catches it as `catch (RecordCoreException)` does.
func (e *KeyExpressionDeserializationError) Unwrap() error {
	return &RecordCoreError{Message: e.Message}
}

// KeyExpressionInvalidResultError is Java's KeyExpression.InvalidResultException
// as Key.Evaluated.getObject raises it for an argument of the wrong class. The
// fields are the exception's addLogInfo keys ("index", expected_type,
// actual_type).
type KeyExpressionInvalidResultError struct {
	Function     string
	Index        int
	ExpectedType string
	ActualType   string
}

func (e *KeyExpressionInvalidResultError) Error() string {
	return fmt.Sprintf("Invalid type in value: function %s argument %d expected %s, got %s",
		e.Function, e.Index, e.ExpectedType, e.ActualType)
}

func (*KeyExpressionInvalidResultError) JavaRecordCoreException() {}
