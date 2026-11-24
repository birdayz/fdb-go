package recordlayer

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/internal/recordlayer/textcmp"
	"fdb.dev/pkg/recordlayer/javanum"
)

// IsMetaDataException tests the outermost Java exception, as Java's
// `re instanceof MetaDataException` tests the exception thrown: a
// MetaDataException subclass through any number of fmt wrappers is one, and a
// MetaDataError that is only the cause of another Java exception is not.
func TestIsMetaDataExceptionTestsTheOutermostJavaException(t *testing.T) {
	t.Parallel()
	mde := &MetaDataError{Message: "bad"}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"a MetaDataError", mde, true},
		{"wrapped by fmt", fmt.Errorf("load: %w", fmt.Errorf("index i: %w", mde)), true},
		{"a subclass", &MetaDataProtoDeserializationError{Cause: errors.New("x")}, true},
		{"a subclass that unwraps to its parent", &UnknownIndexTypeError{IndexName: "i", IndexType: "t"}, true},
		// ProtoUtils.InvalidNameException extends MetaDataException; this
		// package owns InvalidNameError (converted from protoname at the boundary).
		{"an invalid name", fmt.Errorf("column: %w", &InvalidNameError{Message: "name cannot be empty string"}), true},
		{"a textcmp MetaDataError", fmt.Errorf("tokenizer: %w", &textcmp.MetaDataError{Message: "unrecognized text tokenizer"}), true},
		{"a textcmp ArgumentError", &textcmp.ArgumentError{Message: "bad"}, false},
		{"a textcmp MetaDataError as cause of RecordCoreError", &RecordCoreError{Message: "outer", Cause: &textcmp.MetaDataError{Message: "bad"}}, false},
		{"the cause of a RecordCoreError", &RecordCoreError{Message: "outer", Cause: mde}, false},
		{"a key-expression refusal", &KeyExpressionDeserializationError{Message: "k"}, false},
		{"no Java exception", errors.New("plain"), false},
		{"nil", nil, false},
	} {
		if got := IsMetaDataException(c.err); got != c.want {
			t.Errorf("%s: IsMetaDataException = %t, want %t", c.name, got, c.want)
		}
	}
	// MetaDataException extends RecordCoreException: a caller catching
	// RecordCoreError catches it, and still reaches its cause.
	cause := errors.New("root")
	var core *RecordCoreError
	if !errors.As(&MetaDataError{Message: "m", Cause: cause}, &core) || core.Message != "m" {
		t.Errorf("a MetaDataError is not a RecordCoreError: %v", core)
	}
	if !errors.Is(&MetaDataError{Message: "m", Cause: cause}, cause) {
		t.Error("a MetaDataError no longer reaches its cause")
	}
	var ine *InvalidNameError
	_, err := ToProtoBufCompliantName("")
	if !errors.As(err, &ine) || ine.Message != "name cannot be empty string" {
		t.Errorf("ToProtoBufCompliantName empty: got %T (%v), want *recordlayer.InvalidNameError", err, err)
	}
	if !IsMetaDataException(err) {
		t.Errorf("ToProtoBufCompliantName empty: IsMetaDataException = false, want true")
	}
	var rce RecordCoreException
	if !errors.As(err, &rce) {
		t.Errorf("ToProtoBufCompliantName empty: does not implement RecordCoreException")
	}
	err = CheckValidProtoBufCompliantName("9bad")
	if !errors.As(err, &ine) || ine.Message != "9bad it not a valid protobuf identifier" {
		t.Errorf("CheckValidProtoBufCompliantName: got %T (%v), want *recordlayer.InvalidNameError", err, err)
	}
	if !IsMetaDataException(err) {
		t.Errorf("CheckValidProtoBufCompliantName: IsMetaDataException = false, want true")
	}

	// Boundary conversion preserves message, cause, Unwrap, and OutermostJavaError.
	wrappedArg := convertJavaNumError(fmt.Errorf("rtree: sibling 0: %w", &javanum.IllegalArgumentError{Message: "bad node", Cause: cause}))
	if wrappedArg.Error() != "rtree: sibling 0: bad node" {
		t.Errorf("wrapped IllegalArgumentError message = %q", wrappedArg.Error())
	}
	var iae *IllegalArgumentError
	if !errors.As(wrappedArg, &iae) || iae.Message != "bad node" || !errors.Is(wrappedArg, cause) {
		t.Errorf("wrapped IllegalArgumentError lost type or cause: %v", wrappedArg)
	}
	if outer := OutermostJavaError(wrappedArg); outer != iae {
		t.Errorf("OutermostJavaError(wrappedArg) = %T (%v), want *IllegalArgumentError", outer, outer)
	}
	if IsMetaDataException(wrappedArg) {
		t.Errorf("IsMetaDataException(wrappedArg) = true, want false")
	}

	directState := convertJavaNumError(&javanum.IllegalStateError{Message: "packing of non-root is out of valid range"})
	var ise *IllegalStateError
	if !errors.As(directState, &ise) || ise.Error() != "packing of non-root is out of valid range" {
		t.Errorf("direct IllegalStateError = %T (%v)", directState, directState)
	}
	if outer := OutermostJavaError(directState); outer != ise {
		t.Errorf("OutermostJavaError(directState) = %T (%v), want *IllegalStateError", outer, outer)
	}
}
