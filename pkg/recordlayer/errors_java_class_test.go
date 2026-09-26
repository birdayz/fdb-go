package recordlayer

import (
	"errors"
	"fmt"
	"testing"
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
		// ProtoUtils.InvalidNameException extends MetaDataException; its Go
		// type is protoname's, which this package aliases.
		{"an invalid name", fmt.Errorf("column: %w", &InvalidNameError{Message: "name cannot be empty string"}), true},
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
}
