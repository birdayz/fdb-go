package recordlayer

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/internal/recordlayer/javanum"
	"fdb.dev/internal/recordlayer/protoname"
	"fdb.dev/internal/recordlayer/textcmp"
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
		{"the cause of an IllegalArgumentError", &IllegalArgumentError{Message: "outer", Cause: mde}, false},
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
	// Owned IllegalArgumentError is now classified as the outermost Java exception.
	iaeWithMDE := &IllegalArgumentError{Message: "bad arg", Cause: mde}
	if outer := OutermostJavaError(iaeWithMDE); outer != iaeWithMDE {
		t.Errorf("OutermostJavaError(IllegalArgumentError{Cause: MetaDataError}) = %T (%v), want *IllegalArgumentError", outer, outer)
	}
}

func TestInvalidNameErrorBoundary(t *testing.T) {
	t.Parallel()

	_, err := ToProtoBufCompliantName("")
	var ine *InvalidNameError
	if !errors.As(err, &ine) || ine.Message != "name cannot be empty string" {
		t.Errorf("ToProtoBufCompliantName empty: got %T (%v), want *recordlayer.InvalidNameError", err, err)
	}
	var pnErr *protoname.InvalidNameError
	if !errors.As(err, &pnErr) || pnErr.Message != "name cannot be empty string" {
		t.Errorf("ToProtoBufCompliantName empty: lost *protoname.InvalidNameError match: %v", err)
	}
	if !IsMetaDataException(err) {
		t.Errorf("ToProtoBufCompliantName empty: IsMetaDataException = false, want true")
	}
	var rce RecordCoreException
	if !errors.As(err, &rce) {
		t.Errorf("ToProtoBufCompliantName empty: does not implement RecordCoreException")
	}
	if outer := OutermostJavaError(err); outer != ine {
		t.Errorf("ToProtoBufCompliantName empty: OutermostJavaError = %T (%v), want *recordlayer.InvalidNameError", outer, outer)
	}

	err = checkValidProtoBufCompliantName("9bad")
	ine = nil
	pnErr = nil
	if !errors.As(err, &ine) || ine.Message != "9bad it not a valid protobuf identifier" {
		t.Errorf("CheckValidProtoBufCompliantName: got %T (%v), want *recordlayer.InvalidNameError", err, err)
	}
	if !errors.As(err, &pnErr) {
		t.Errorf("CheckValidProtoBufCompliantName: lost *protoname.InvalidNameError match: %v", err)
	}
	if !IsMetaDataException(err) {
		t.Errorf("CheckValidProtoBufCompliantName: IsMetaDataException = false, want true")
	}
}

func TestJavaNumBoundaryConversion(t *testing.T) {
	t.Parallel()

	cause := errors.New("root")
	wrappedArg := convertJavaNumError(fmt.Errorf("rtree: sibling 0: %w", &javanum.IllegalArgumentError{Message: "bad node", Cause: cause}))
	if wrappedArg.Error() != "rtree: sibling 0: bad node" {
		t.Errorf("wrapped IllegalArgumentError message = %q", wrappedArg.Error())
	}
	var iae *IllegalArgumentError
	if !errors.As(wrappedArg, &iae) || iae.Message != "bad node" || !errors.Is(wrappedArg, cause) {
		t.Errorf("wrapped IllegalArgumentError lost type or cause: %v", wrappedArg)
	}
	var jnIae *javanum.IllegalArgumentError
	if !errors.As(wrappedArg, &jnIae) || jnIae.Message != "bad node" {
		t.Errorf("wrapped IllegalArgumentError lost *javanum.IllegalArgumentError match: %v", wrappedArg)
	}
	if outer := OutermostJavaError(wrappedArg); outer != iae {
		t.Errorf("OutermostJavaError(wrappedArg) = %T (%v), want *IllegalArgumentError", outer, outer)
	}
	if IsMetaDataException(wrappedArg) {
		t.Errorf("IsMetaDataException(wrappedArg) = true, want false")
	}

	// Bare errors also keep both recordlayer and javanum types matchable.
	bareArg := convertJavaNumError(&javanum.IllegalArgumentError{Message: "bad arg", Cause: cause})
	iae = nil
	jnIae = nil
	if !errors.As(bareArg, &iae) || !errors.As(bareArg, &jnIae) || !errors.Is(bareArg, cause) {
		t.Errorf("bare IllegalArgumentError lost dual match or cause: %v", bareArg)
	}
	if outer := OutermostJavaError(bareArg); outer != iae {
		t.Errorf("OutermostJavaError(bareArg) = %T (%v), want *IllegalArgumentError", outer, outer)
	}

	directState := convertJavaNumError(&javanum.IllegalStateError{Message: "packing of non-root is out of valid range"})
	var ise *IllegalStateError
	var jnIse *javanum.IllegalStateError
	if !errors.As(directState, &ise) || !errors.As(directState, &jnIse) || ise.Error() != "packing of non-root is out of valid range" {
		t.Errorf("direct IllegalStateError = %T (%v)", directState, directState)
	}
	if outer := OutermostJavaError(directState); outer != ise {
		t.Errorf("OutermostJavaError(directState) = %T (%v), want *IllegalStateError", outer, outer)
	}
}
