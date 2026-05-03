package recordlayer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// RecordStoreAlreadyExistsError
// ---------------------------------------------------------------------------

func TestRecordStoreAlreadyExistsError_Message(t *testing.T) {
	t.Parallel()
	err := &RecordStoreAlreadyExistsError{}
	if got := err.Error(); got != "record store already exists" {
		t.Fatalf("unexpected message: %q", got)
	}
}

func TestRecordStoreAlreadyExistsError_ErrorsAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("outer: %w", &RecordStoreAlreadyExistsError{})
	var target *RecordStoreAlreadyExistsError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed to match RecordStoreAlreadyExistsError")
	}
}

// ---------------------------------------------------------------------------
// RecordStoreDoesNotExistError
// ---------------------------------------------------------------------------

func TestRecordStoreDoesNotExistError_Message(t *testing.T) {
	t.Parallel()
	err := &RecordStoreDoesNotExistError{}
	if got := err.Error(); got != "record store does not exist" {
		t.Fatalf("unexpected message: %q", got)
	}
}

func TestRecordStoreDoesNotExistError_ErrorsAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("ctx: %w", &RecordStoreDoesNotExistError{})
	var target *RecordStoreDoesNotExistError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed to match RecordStoreDoesNotExistError")
	}
}

// ---------------------------------------------------------------------------
// RecordStoreNoInfoButNotEmptyError
// ---------------------------------------------------------------------------

func TestRecordStoreNoInfoButNotEmptyError_WithFirstKey(t *testing.T) {
	t.Parallel()
	err := &RecordStoreNoInfoButNotEmptyError{FirstKey: []byte{0x01, 0xAB, 0xFF}}
	got := err.Error()
	if !strings.Contains(got, "01abff") {
		t.Fatalf("expected hex of first key in message, got: %q", got)
	}
	if !strings.Contains(got, "not empty") {
		t.Fatalf("missing 'not empty' substring: %q", got)
	}
}

func TestRecordStoreNoInfoButNotEmptyError_NilFirstKey(t *testing.T) {
	t.Parallel()
	err := &RecordStoreNoInfoButNotEmptyError{FirstKey: nil}
	want := "record store has no info but is not empty"
	if got := err.Error(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRecordStoreNoInfoButNotEmptyError_EmptyFirstKey(t *testing.T) {
	t.Parallel()
	// Empty slice (non-nil but zero-length) -- still non-nil, so the hex path fires.
	err := &RecordStoreNoInfoButNotEmptyError{FirstKey: []byte{}}
	got := err.Error()
	if !strings.Contains(got, "first key") {
		t.Fatalf("expected 'first key' substring for non-nil empty slice: %q", got)
	}
}

func TestRecordStoreNoInfoButNotEmptyError_ErrorsAs(t *testing.T) {
	t.Parallel()
	orig := &RecordStoreNoInfoButNotEmptyError{FirstKey: []byte{0xDE}}
	wrapped := fmt.Errorf("op: %w", orig)
	var target *RecordStoreNoInfoButNotEmptyError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
	if len(target.FirstKey) != 1 || target.FirstKey[0] != 0xDE {
		t.Fatalf("FirstKey mismatch: %x", target.FirstKey)
	}
}

// ---------------------------------------------------------------------------
// RecordStoreStateNotLoadedError
// ---------------------------------------------------------------------------

func TestRecordStoreStateNotLoadedError_Message(t *testing.T) {
	t.Parallel()
	err := &RecordStoreStateNotLoadedError{}
	if got := err.Error(); got != "record store state not loaded" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestRecordStoreStateNotLoadedError_ErrorsAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("x: %w", &RecordStoreStateNotLoadedError{})
	var target *RecordStoreStateNotLoadedError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
}

// ---------------------------------------------------------------------------
// IndexNotReadableError
// ---------------------------------------------------------------------------

func TestIndexNotReadableError_Message(t *testing.T) {
	t.Parallel()
	err := &IndexNotReadableError{IndexName: "my_idx", CurrentState: IndexStateWriteOnly}
	got := err.Error()
	if !strings.Contains(got, "my_idx") {
		t.Fatalf("missing index name: %q", got)
	}
	if !strings.Contains(got, "WRITE_ONLY") {
		t.Fatalf("missing state string: %q", got)
	}
}

func TestIndexNotReadableError_ZeroState(t *testing.T) {
	t.Parallel()
	// IndexState(0) == IndexStateReadable => "READABLE"
	err := &IndexNotReadableError{IndexName: "idx", CurrentState: IndexState(0)}
	got := err.Error()
	if !strings.Contains(got, "READABLE") {
		t.Fatalf("expected READABLE for zero state: %q", got)
	}
}

func TestIndexNotReadableError_ErrorsAs(t *testing.T) {
	t.Parallel()
	orig := &IndexNotReadableError{IndexName: "idx", CurrentState: IndexStateDisabled}
	wrapped := fmt.Errorf("scan: %w", orig)
	var target *IndexNotReadableError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
	if target.IndexName != "idx" {
		t.Fatalf("IndexName mismatch: %q", target.IndexName)
	}
	if target.CurrentState != IndexStateDisabled {
		t.Fatalf("CurrentState mismatch: %v", target.CurrentState)
	}
}

// ---------------------------------------------------------------------------
// IndexNotFoundError
// ---------------------------------------------------------------------------

func TestIndexNotFoundError_Message(t *testing.T) {
	t.Parallel()
	err := &IndexNotFoundError{IndexName: "missing_idx"}
	got := err.Error()
	if !strings.Contains(got, "missing_idx") {
		t.Fatalf("missing index name: %q", got)
	}
	if !strings.Contains(got, "not found") {
		t.Fatalf("missing 'not found': %q", got)
	}
}

func TestIndexNotFoundError_ErrorsAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("get: %w", &IndexNotFoundError{IndexName: "foo"})
	var target *IndexNotFoundError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
	if target.IndexName != "foo" {
		t.Fatalf("IndexName mismatch: %q", target.IndexName)
	}
}

// ---------------------------------------------------------------------------
// IndexNotBuiltError
// ---------------------------------------------------------------------------

func TestIndexNotBuiltError_Message(t *testing.T) {
	t.Parallel()
	err := &IndexNotBuiltError{IndexName: "unbuilt"}
	got := err.Error()
	if !strings.Contains(got, "unbuilt") {
		t.Fatalf("missing index name: %q", got)
	}
	if !strings.Contains(got, "not built") {
		t.Fatalf("missing 'not built': %q", got)
	}
}

func TestIndexNotBuiltError_ErrorsAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("mark: %w", &IndexNotBuiltError{IndexName: "x"})
	var target *IndexNotBuiltError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
}

// ---------------------------------------------------------------------------
// MetaDataError
// ---------------------------------------------------------------------------

func TestMetaDataError_Message(t *testing.T) {
	t.Parallel()
	err := &MetaDataError{Message: "bad schema version"}
	if got := err.Error(); got != "bad schema version" {
		t.Fatalf("got %q, want %q", got, "bad schema version")
	}
}

func TestMetaDataError_EmptyMessage(t *testing.T) {
	t.Parallel()
	err := &MetaDataError{Message: ""}
	if got := err.Error(); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestMetaDataError_ErrorsAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("build: %w", &MetaDataError{Message: "oops"})
	var target *MetaDataError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
	if target.Message != "oops" {
		t.Fatalf("Message mismatch: %q", target.Message)
	}
}

// ---------------------------------------------------------------------------
// UnsupportedFormatVersionError
// ---------------------------------------------------------------------------

func TestUnsupportedFormatVersionError_Message(t *testing.T) {
	t.Parallel()
	err := &UnsupportedFormatVersionError{Version: 99, MaxVersion: 14}
	got := err.Error()
	if !strings.Contains(got, "99") {
		t.Fatalf("missing version 99: %q", got)
	}
	if !strings.Contains(got, "14") {
		t.Fatalf("missing max version 14: %q", got)
	}
	if !strings.Contains(got, "unsupported") {
		t.Fatalf("missing 'unsupported': %q", got)
	}
}

func TestUnsupportedFormatVersionError_ZeroVersions(t *testing.T) {
	t.Parallel()
	err := &UnsupportedFormatVersionError{Version: 0, MaxVersion: 0}
	got := err.Error()
	// Should still format without panic.
	if !strings.Contains(got, "0") {
		t.Fatalf("expected '0' in message: %q", got)
	}
}

func TestUnsupportedFormatVersionError_ErrorsAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("open: %w", &UnsupportedFormatVersionError{Version: 20, MaxVersion: 14})
	var target *UnsupportedFormatVersionError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
	if target.Version != 20 {
		t.Fatalf("Version: got %d, want 20", target.Version)
	}
	if target.MaxVersion != 14 {
		t.Fatalf("MaxVersion: got %d, want 14", target.MaxVersion)
	}
}

// ---------------------------------------------------------------------------
// RecordSerializationError
// ---------------------------------------------------------------------------

func TestRecordSerializationError_Message(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("proto: bad wire type")
	err := &RecordSerializationError{Cause: cause}
	got := err.Error()
	if !strings.Contains(got, "proto: bad wire type") {
		t.Fatalf("missing cause in message: %q", got)
	}
	if !strings.Contains(got, "serialize") {
		t.Fatalf("missing 'serialize': %q", got)
	}
}

func TestRecordSerializationError_NilCause(t *testing.T) {
	t.Parallel()
	err := &RecordSerializationError{Cause: nil}
	got := err.Error()
	// Should not panic; nil cause formats as "<nil>".
	if !strings.Contains(got, "serialize") {
		t.Fatalf("missing 'serialize': %q", got)
	}
}

func TestRecordSerializationError_Unwrap(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("inner")
	err := &RecordSerializationError{Cause: cause}
	unwrapped := errors.Unwrap(err)
	if unwrapped != cause {
		t.Fatalf("Unwrap returned %v, want %v", unwrapped, cause)
	}
}

func TestRecordSerializationError_UnwrapNil(t *testing.T) {
	t.Parallel()
	err := &RecordSerializationError{Cause: nil}
	if got := errors.Unwrap(err); got != nil {
		t.Fatalf("Unwrap of nil Cause should be nil, got %v", got)
	}
}

func TestRecordSerializationError_ErrorsAs(t *testing.T) {
	t.Parallel()
	inner := &RecordSerializationError{Cause: fmt.Errorf("x")}
	wrapped := fmt.Errorf("save: %w", inner)
	var target *RecordSerializationError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
}

func TestRecordSerializationError_ErrorsIs_ThroughCause(t *testing.T) {
	t.Parallel()
	sentinel := fmt.Errorf("root cause")
	err := &RecordSerializationError{Cause: fmt.Errorf("mid: %w", sentinel)}
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is should find sentinel through Unwrap chain")
	}
}

// ---------------------------------------------------------------------------
// RecordDeserializationError
// ---------------------------------------------------------------------------

func TestRecordDeserializationError_WithPrimaryKey(t *testing.T) {
	t.Parallel()
	err := &RecordDeserializationError{PrimaryKey: "pk-42", Cause: fmt.Errorf("bad proto")}
	got := err.Error()
	if !strings.Contains(got, "pk-42") {
		t.Fatalf("missing primary key: %q", got)
	}
	if !strings.Contains(got, "bad proto") {
		t.Fatalf("missing cause: %q", got)
	}
}

func TestRecordDeserializationError_NilPrimaryKey(t *testing.T) {
	t.Parallel()
	err := &RecordDeserializationError{PrimaryKey: nil, Cause: fmt.Errorf("bad")}
	got := err.Error()
	// nil PK path should not include "nil" as a literal primary key representation.
	if strings.Contains(got, "<nil>") {
		t.Fatalf("nil PK should be omitted, got: %q", got)
	}
	if !strings.Contains(got, "deserialize") {
		t.Fatalf("missing 'deserialize': %q", got)
	}
}

func TestRecordDeserializationError_NilCause(t *testing.T) {
	t.Parallel()
	err := &RecordDeserializationError{PrimaryKey: "pk", Cause: nil}
	// Should not panic.
	got := err.Error()
	if !strings.Contains(got, "pk") {
		t.Fatalf("missing primary key: %q", got)
	}
}

func TestRecordDeserializationError_BothNil(t *testing.T) {
	t.Parallel()
	err := &RecordDeserializationError{}
	// Zero value -- should not panic.
	got := err.Error()
	if !strings.Contains(got, "deserialize") {
		t.Fatalf("missing 'deserialize': %q", got)
	}
}

func TestRecordDeserializationError_Unwrap(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("inner")
	err := &RecordDeserializationError{Cause: cause}
	if got := errors.Unwrap(err); got != cause {
		t.Fatalf("Unwrap: got %v, want %v", got, cause)
	}
}

func TestRecordDeserializationError_UnwrapNil(t *testing.T) {
	t.Parallel()
	err := &RecordDeserializationError{Cause: nil}
	if got := errors.Unwrap(err); got != nil {
		t.Fatalf("Unwrap nil cause should be nil, got %v", got)
	}
}

func TestRecordDeserializationError_ErrorsAs(t *testing.T) {
	t.Parallel()
	inner := &RecordDeserializationError{PrimaryKey: "pk", Cause: fmt.Errorf("x")}
	wrapped := fmt.Errorf("load: %w", inner)
	var target *RecordDeserializationError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
	if target.PrimaryKey != "pk" {
		t.Fatalf("PrimaryKey mismatch: %v", target.PrimaryKey)
	}
}

func TestRecordDeserializationError_IntPrimaryKey(t *testing.T) {
	t.Parallel()
	// PrimaryKey is any -- verify non-string types work.
	err := &RecordDeserializationError{PrimaryKey: int64(999), Cause: fmt.Errorf("corrupt")}
	got := err.Error()
	if !strings.Contains(got, "999") {
		t.Fatalf("expected int PK in message: %q", got)
	}
}

// ---------------------------------------------------------------------------
// KeyExpressionError
// ---------------------------------------------------------------------------

func TestKeyExpressionError_Message(t *testing.T) {
	t.Parallel()
	err := &KeyExpressionError{Message: "field not found"}
	if got := err.Error(); got != "field not found" {
		t.Fatalf("got %q", got)
	}
}

func TestKeyExpressionError_EmptyMessage(t *testing.T) {
	t.Parallel()
	err := &KeyExpressionError{}
	if got := err.Error(); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestKeyExpressionError_ErrorsAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("eval: %w", &KeyExpressionError{Message: "x"})
	var target *KeyExpressionError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
	if target.Message != "x" {
		t.Fatalf("Message: got %q", target.Message)
	}
}

// ---------------------------------------------------------------------------
// PartlyBuiltError
// ---------------------------------------------------------------------------

func TestPartlyBuiltError_Message(t *testing.T) {
	t.Parallel()
	err := &PartlyBuiltError{
		IndexName:     "idx",
		SavedStamp:    "BY_RECORDS",
		ExpectedStamp: "BY_INDEX",
		Message:       "stamp mismatch",
	}
	got := err.Error()
	for _, want := range []string{"idx", "stamp mismatch", "BY_RECORDS", "BY_INDEX"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in: %q", want, got)
		}
	}
}

func TestPartlyBuiltError_EmptyFields(t *testing.T) {
	t.Parallel()
	err := &PartlyBuiltError{}
	// Should not panic with zero values.
	got := err.Error()
	if got == "" {
		t.Fatal("Error() returned empty string for zero-value PartlyBuiltError")
	}
}

func TestPartlyBuiltError_ErrorsAs(t *testing.T) {
	t.Parallel()
	inner := &PartlyBuiltError{IndexName: "foo", Message: "blocked"}
	wrapped := fmt.Errorf("build: %w", inner)
	var target *PartlyBuiltError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed")
	}
	if target.IndexName != "foo" {
		t.Fatalf("IndexName: got %q", target.IndexName)
	}
	if target.Message != "blocked" {
		t.Fatalf("Message: got %q", target.Message)
	}
}

// ---------------------------------------------------------------------------
// Cross-type: errors.As must NOT match unrelated types.
// ---------------------------------------------------------------------------

func TestErrorsAs_NoFalsePositives(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("outer: %w", &RecordStoreAlreadyExistsError{})
	var wrongTarget *IndexNotFoundError
	if errors.As(err, &wrongTarget) {
		t.Fatal("errors.As matched unrelated type")
	}
}

// ---------------------------------------------------------------------------
// error interface satisfaction (compile-time)
// ---------------------------------------------------------------------------

var (
	_ error = (*RecordStoreAlreadyExistsError)(nil)
	_ error = (*RecordStoreDoesNotExistError)(nil)
	_ error = (*RecordStoreNoInfoButNotEmptyError)(nil)
	_ error = (*RecordStoreStateNotLoadedError)(nil)
	_ error = (*IndexNotReadableError)(nil)
	_ error = (*IndexNotFoundError)(nil)
	_ error = (*IndexNotBuiltError)(nil)
	_ error = (*MetaDataError)(nil)
	_ error = (*UnsupportedFormatVersionError)(nil)
	_ error = (*RecordSerializationError)(nil)
	_ error = (*RecordDeserializationError)(nil)
	_ error = (*KeyExpressionError)(nil)
	_ error = (*PartlyBuiltError)(nil)
)

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkRecordStoreAlreadyExistsError_Error(b *testing.B) {
	err := &RecordStoreAlreadyExistsError{}
	var s string
	for b.Loop() {
		s = err.Error()
	}
	_ = s
}

func BenchmarkRecordStoreNoInfoButNotEmptyError_Error_WithKey(b *testing.B) {
	err := &RecordStoreNoInfoButNotEmptyError{FirstKey: []byte{0x01, 0x02, 0x03, 0x04}}
	var s string
	for b.Loop() {
		s = err.Error()
	}
	_ = s
}

func BenchmarkRecordStoreNoInfoButNotEmptyError_Error_NilKey(b *testing.B) {
	err := &RecordStoreNoInfoButNotEmptyError{}
	var s string
	for b.Loop() {
		s = err.Error()
	}
	_ = s
}

func BenchmarkIndexNotReadableError_Error(b *testing.B) {
	err := &IndexNotReadableError{IndexName: "my_index", CurrentState: IndexStateWriteOnly}
	var s string
	for b.Loop() {
		s = err.Error()
	}
	_ = s
}

func BenchmarkUnsupportedFormatVersionError_Error(b *testing.B) {
	err := &UnsupportedFormatVersionError{Version: 99, MaxVersion: 14}
	var s string
	for b.Loop() {
		s = err.Error()
	}
	_ = s
}

func BenchmarkRecordDeserializationError_Error_WithPK(b *testing.B) {
	err := &RecordDeserializationError{PrimaryKey: int64(42), Cause: fmt.Errorf("bad proto")}
	var s string
	for b.Loop() {
		s = err.Error()
	}
	_ = s
}

func BenchmarkPartlyBuiltError_Error(b *testing.B) {
	err := &PartlyBuiltError{
		IndexName:     "my_idx",
		SavedStamp:    "BY_RECORDS",
		ExpectedStamp: "BY_INDEX",
		Message:       "stamp mismatch",
	}
	var s string
	for b.Loop() {
		s = err.Error()
	}
	_ = s
}
