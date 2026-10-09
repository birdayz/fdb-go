package embedded

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/vectorindex"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// Every Go error type porting a Java RecordCoreException subclass reaches SQL
// with the code ExceptionUtil.recordCoreToRelationalException gives it: its
// specific arm where it has one, else ErrorCode.UNKNOWN (XXXXX). Java's generic
// Throwable branch gives Query.InvalidExpressionException XXXXX as well.
func TestRecordCoreExceptionsMapAsJavasExceptionUtil(t *testing.T) {
	t.Parallel()
	pk := tuple.Tuple{int64(1)}
	for _, c := range []struct {
		err  error
		want api.ErrorCode
	}{
		{&recordlayer.AggregateFunctionNotSupportedError{}, api.ErrCodeUnknown},
		{&recordlayer.UnknownOrElseCursorStateError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordStoreAlreadyExistsError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordStoreDoesNotExistError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordStoreNoInfoButNotEmptyError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordStoreStateNotLoadedError{}, api.ErrCodeUnknown},
		{&recordlayer.IndexNotReadableError{}, api.ErrCodeUnknown},
		{&recordlayer.UnsupportedFormatVersionError{}, api.ErrCodeUnknown},
		{&recordlayer.UnsupportedFeatureForFormatVersionError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordSerializationError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordSerializationValidationError{}, api.ErrCodeUnknown},
		{&recordlayer.ContinuationParseError{}, api.ErrCodeUnknown},
		{&recordlayer.ContinuationEncodeError{}, api.ErrCodeUnknown},
		{&recordlayer.QueryInvalidExpressionError{}, api.ErrCodeUnknown},
		{&recordlayer.KeyExpressionDeserializationError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordCoreError{Message: "x"}, api.ErrCodeUnknown},
		{&recordlayer.RecordCoreInternalError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordCoreStorageError{}, api.ErrCodeUnknown},
		{&recordlayer.FoundSplitOutOfOrderError{}, api.ErrCodeUnknown},
		{&recordlayer.FoundSplitWithoutStartError{}, api.ErrCodeUnknown},
		{&recordlayer.PartlyBuiltError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordTypeChangedError{}, api.ErrCodeUnknown},
		{&vectorindex.VectorIndexClusterTooLargeError{}, api.ErrCodeUnknown},
		{&vectorindex.NegativeTaskCountError{}, api.ErrCodeUnknown},
		{&recordlayer.IndexKeySizeError{}, api.ErrCodeUnknown},
		{&recordlayer.IndexValueSizeError{}, api.ErrCodeUnknown},
		{&recordlayer.KeyExpressionInvalidResultError{}, api.ErrCodeUnknown},
		{&recordlayer.UnsupportedValueTypeError{}, api.ErrCodeUnknown},
		{&recordlayer.TimeLimitExceededError{}, api.ErrCodeUnknown},
		{&recordlayer.IndexingValidationError{}, api.ErrCodeUnknown},
		{&recordlayer.UnexpectedReadableError{}, api.ErrCodeUnknown},
		{&recordlayer.RecordCoreArgumentError{Message: "Cannot execute plan at SNAPSHOT isolation level", Plan: "RecordQueryInsertPlan"}, api.ErrCodeUnknown},
		{&recordlayer.ScanLimitReachedError{}, api.ErrCodeUnknown},
		{&recordlayer.SlidingWindowCorruptionError{}, api.ErrCodeUnknown},
		{&recordlayer.StoreIsLockedForRecordUpdatesError{}, api.ErrCodeUnknown},
		{&recordlayer.StoreIsFullyLockedError{}, api.ErrCodeUnknown},
		{&recordlayer.UnknownStoreLockStateError{}, api.ErrCodeUnknown},
		{&recordlayer.StaleMetaDataVersionError{}, api.ErrCodeUnknown},
		{&recordlayer.RunnerClosedError{}, api.ErrCodeUnknown},
		{&cascades.PlannerBudgetExceededError{}, api.ErrCodeUnknown},
		{&values.ProtoTypeError{}, api.ErrCodeUnknown},
		{&values.RangeBoundsError{}, api.ErrCodeUnknown},
		// Their own arms.
		{&recordlayer.RecordDoesNotExistError{PrimaryKey: pk}, api.ErrCodeUnknown},
		{&recordlayer.RecordAlreadyExistsError{PrimaryKey: pk}, api.ErrCodeUniqueConstraintViolation},
		{&recordlayer.RecordIndexUniquenessViolationError{}, api.ErrCodeUniqueConstraintViolation},
		{&recordlayer.RecordDeserializationError{PrimaryKey: pk, Cause: errors.New("bad")}, api.ErrCodeDeserializationFailure},
		{&recordlayer.MetaDataError{Message: "m"}, api.ErrCodeSyntaxOrAccessViolation},
		{&recordlayer.IndexNotFoundError{}, api.ErrCodeSyntaxOrAccessViolation},
		{&recordlayer.MetaDataVersionMustIncreaseError{}, api.ErrCodeSyntaxOrAccessViolation},
		{&recordlayer.RecordTypeKeyTypeError{}, api.ErrCodeSyntaxOrAccessViolation},
		{&recordlayer.InvalidNameError{Message: "n"}, api.ErrCodeSyntaxOrAccessViolation},
		// Go-only: a save refusing a string that is not valid UTF-8, as every
		// save path reports it (inside RecordSerializationError).
		{&recordlayer.RecordSerializationError{Cause: &recordlayer.InvalidUTF8StringError{Field: "name"}}, api.ErrCodeCharacterNotInRepertoire},
	} {
		name := fmt.Sprintf("%T", c.err)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := translateFDBError(fmt.Errorf("context: %w", c.err))
			var apiErr *api.Error
			if !errors.As(got, &apiErr) || apiErr.Code != c.want {
				t.Fatalf("%s: got %v, want %s", name, got, c.want)
			}
			if target := reflect.New(reflect.TypeOf(c.err)); !errors.As(got, target.Interface()) {
				t.Fatalf("%s: the mapped error lost its cause", name)
			}
		})
	}
	// An error that ports no Java exception keeps passing through unchanged.
	plain := errors.New("plain")
	if got := translateFDBError(plain); got != plain {
		t.Fatalf("plain error: got %v", got)
	}
}
