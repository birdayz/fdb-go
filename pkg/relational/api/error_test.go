package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorCodeFromString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want ErrorCode
	}{
		{"00000", ErrCodeSuccess},
		{"42F01", ErrCodeUndefinedTable},
		{"42703", ErrCodeUndefinedColumn},
		{"XX000", ErrCodeInternalError},
		{"not-a-real-code", ErrCodeUnknown},
		{"", ErrCodeUnknown},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			if got := ErrorCodeFromString(c.in); got != c.want {
				t.Fatalf("ErrorCodeFromString(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestErrorCodeClass(t *testing.T) {
	t.Parallel()
	if got := ErrCodeUndefinedTable.Class(); got != "42" {
		t.Fatalf("Class() = %q, want %q", got, "42")
	}
	if got := ErrCodeInternalError.Class(); got != "XX" {
		t.Fatalf("Class() = %q, want %q", got, "XX")
	}
	// Malformed code shorter than 2 characters.
	if got := ErrorCode("").Class(); got != "" {
		t.Fatalf("Class() on empty = %q, want empty", got)
	}
}

func TestErrorBasic(t *testing.T) {
	t.Parallel()
	e := NewError(ErrCodeUndefinedTable, "table 'orders' does not exist")
	if e.Code != ErrCodeUndefinedTable {
		t.Fatalf("Code = %q", e.Code)
	}
	if !strings.Contains(e.Error(), "42F01") {
		t.Fatalf("Error() missing SQLSTATE: %q", e.Error())
	}
	if !strings.Contains(e.Error(), "orders") {
		t.Fatalf("Error() missing message: %q", e.Error())
	}
}

func TestErrorWrapping(t *testing.T) {
	t.Parallel()
	cause := errors.New("underlying failure")
	e := WrapError(ErrCodeInternalError, "something broke", cause)

	if !errors.Is(e, cause) {
		t.Fatal("errors.Is failed to find wrapped cause")
	}

	var got *Error
	if !errors.As(e, &got) {
		t.Fatal("errors.As failed to extract *Error")
	}
	if got.Code != ErrCodeInternalError {
		t.Fatalf("extracted Code = %q", got.Code)
	}

	wrapped := fmt.Errorf("context: %w", e)
	if AsError(wrapped) == nil {
		t.Fatal("AsError failed through fmt.Errorf %w chain")
	}
}

// TestWrapErrorf pins formatted-message wrapping at API boundaries.
// fmt.Errorf("context: %w", err) doesn't carry a SQLSTATE the way
// errors.As does on *Error — WrapErrorf produces one that does.
func TestWrapErrorf(t *testing.T) {
	t.Parallel()
	cause := errors.New("io failure")
	e := WrapErrorf(cause, ErrCodeInternalError, "context %s for table=%q", "load", "orders")

	if e.Code != ErrCodeInternalError {
		t.Fatalf("Code: got %q, want %q", e.Code, ErrCodeInternalError)
	}
	if e.Message != `context load for table="orders"` {
		t.Fatalf("formatted Message: got %q", e.Message)
	}
	if !errors.Is(e, cause) {
		t.Fatal("WrapErrorf should propagate %%w chain")
	}
}

// TestNewErrorf pins the formatted constructor for the no-cause
// case. Symmetric to NewError.
func TestNewErrorf(t *testing.T) {
	t.Parallel()
	e := NewErrorf(ErrCodeUndefinedTable, "unknown table %q in schema %s", "T", "S")
	if e.Code != ErrCodeUndefinedTable {
		t.Fatalf("Code: got %q", e.Code)
	}
	if e.Message != `unknown table "T" in schema S` {
		t.Fatalf("formatted Message: got %q", e.Message)
	}
	if e.Cause != nil {
		t.Fatal("NewErrorf should leave Cause nil")
	}
}

func TestErrorWithContextImmutable(t *testing.T) {
	t.Parallel()
	base := NewError(ErrCodeUndefinedColumn, "unknown column 'foo'")
	withTable := base.WithContext("table", "orders")
	withColumn := withTable.WithContext("column", "foo")

	if len(base.Context) != 0 {
		t.Fatalf("base was mutated: %+v", base.Context)
	}
	if withTable.Context["table"] != "orders" {
		t.Fatalf("withTable missing table: %+v", withTable.Context)
	}
	if _, ok := withTable.Context["column"]; ok {
		t.Fatalf("withTable should not have column: %+v", withTable.Context)
	}
	if withColumn.Context["table"] != "orders" || withColumn.Context["column"] != "foo" {
		t.Fatalf("withColumn wrong: %+v", withColumn.Context)
	}

	// Context is rendered in Error() with keys sorted alphabetically
	// so output is deterministic across calls / processes.
	msg := withColumn.Error()
	if !strings.Contains(msg, "column=foo") || !strings.Contains(msg, "table=orders") {
		t.Fatalf("Error() missing context fields: %q", msg)
	}
	// Deterministic ordering — column before table alphabetically.
	columnIdx := strings.Index(msg, "column=foo")
	tableIdx := strings.Index(msg, "table=orders")
	if columnIdx > tableIdx {
		t.Errorf("context keys not sorted: %q", msg)
	}
	// Repeated calls produce the same string (Go's map iteration
	// is randomised — without a sort this would flake).
	for i := 0; i < 20; i++ {
		if got := withColumn.Error(); got != msg {
			t.Fatalf("Error() non-deterministic: %q vs %q", msg, got)
		}
	}
}

func TestAsErrorMiss(t *testing.T) {
	t.Parallel()
	if AsError(nil) != nil {
		t.Fatal("AsError(nil) should be nil")
	}
	if AsError(errors.New("plain")) != nil {
		t.Fatal("AsError(plain error) should be nil")
	}
}

// TestEveryCodeRoundTrips ensures every declared ErrorCode is indexed
// in ErrorCodeFromString (catches forget-to-update-init-list bugs).
func TestEveryCodeRoundTrips(t *testing.T) {
	t.Parallel()
	codes := []ErrorCode{
		ErrCodeSuccess, ErrCodeNoResultSet,
		ErrCodeUnableToEstablishSQLConnection, ErrCodeConnectionDoesNotExist,
		ErrCodeInvalidPath, ErrCodeCannotCommitRollbackWithAutocommit,
		ErrCodeUnsupportedOperation, ErrCodeUnsupportedQuery, ErrCodeUnsupportedSort,
		ErrCodeCannotConvertType, ErrCodeInvalidRowCountInLimitClause,
		ErrCodeInvalidParameter, ErrCodeArrayElementError,
		ErrCodeInvalidBinaryRepresentation, ErrCodeInvalidArgumentForFunction,
		ErrCodeInvalidCast, ErrCodeCopySerializationError, ErrCodeCopyImportValidationError,
		ErrCodeNotNullViolation, ErrCodeUniqueConstraintViolation,
		ErrCodeInvalidCursorState, ErrCodeInvalidContinuation,
		ErrCodeInvalidTransactionState, ErrCodeTransactionInactive,
		ErrCodeSerializationFailure, ErrCodeSyntaxOrAccessViolation,
		ErrCodeInsufficientPrivilege, ErrCodeSyntaxError, ErrCodeInvalidName,
		ErrCodeColumnAlreadyExists, ErrCodeAmbiguousColumn, ErrCodeUndefinedColumn,
		ErrCodeDuplicateAlias, ErrCodeDuplicateFunction, ErrCodeGroupingError,
		ErrCodeDatatypeMismatch, ErrCodeWrongObjectType, ErrCodeUndefinedFunction,
		ErrCodeUndefinedDatabase, ErrCodeUndefinedTable, ErrCodeUndefinedParameter,
		ErrCodeDatabaseAlreadyExists, ErrCodeSchemaAlreadyExists, ErrCodeTableAlreadyExists,
		ErrCodeInvalidColumnReference, ErrCodeInvalidFunctionDefinition,
		ErrCodeInvalidTableDefinition, ErrCodeUnknownType, ErrCodeInvalidRecursion,
		ErrCodeIncompatibleTableAlias, ErrCodeWindowingError,
		ErrCodeSchemaMappingAlreadyExists, ErrCodeUndefinedSchema, ErrCodeUndefinedIndex,
		ErrCodeUnknownSchemaTemplate, ErrCodeAnnotationAlreadyExists,
		ErrCodeIndexAlreadyExists, ErrCodeIncorrectMetadataTableVersion,
		ErrCodeInvalidSchemaTemplate, ErrCodeInvalidPreparedStatementParameter,
		ErrCodeExecuteUpdateReturnedResultSet, ErrCodeDuplicateSchemaTemplate,
		ErrCodeUnknownDatabase, ErrCodeUnionIncorrectColumnCount,
		ErrCodeUnionIncompatibleColumns, ErrCodeInvalidDatabase,
		ErrCodeTransactionTimeout, ErrCodeTooManyColumns, ErrCodeExecutionLimitReached,
		ErrCodeStatementClosed, ErrCodeUndefinedFile,
		ErrCodeUnknown, ErrCodeInternalError, ErrCodeDeserializationFailure,
	}
	for _, c := range codes {
		if got := ErrorCodeFromString(string(c)); got != c {
			t.Errorf("roundtrip failed for %q: got %q", c, got)
		}
	}
}
