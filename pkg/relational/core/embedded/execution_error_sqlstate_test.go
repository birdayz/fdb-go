package embedded

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// The executor names its SQL-visible failures; translateExecError gives each
// the SQLSTATE and message the executor used to build itself.
func TestTranslateExecErrorMapsExecutorFailures(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err         error
		wantCode    api.ErrorCode
		wantMessage string
	}{
		{&executor.ExecutionError{Kind: executor.ExecIncompatibleType, Message: "incompatible"}, api.ErrCodeCannotConvertType, "incompatible"},
		{&executor.ExecutionError{Kind: executor.ExecNotNullViolation, Message: "null field"}, api.ErrCodeNotNullViolation, "null field"},
		{&executor.ExecutionError{Kind: executor.ExecCardinalityViolation, Message: "two rows"}, api.ErrCodeCardinalityViolation, "two rows"},
		{&executor.ExecutionError{Kind: executor.ExecSubqueryColumnCount, Message: "two columns"}, api.ErrCodeSyntaxError, "two columns"},
		{&executor.ExecutionError{Kind: executor.ExecInternalError, Message: "invariant"}, api.ErrCodeInternalError, "invariant"},
		{&values.RangeBoundsError{Message: "only positive step is allowed in range"}, api.ErrCodeUnknown, "only positive step is allowed in range"},
		{&values.InvalidUUIDValueError{Value: "x"}, api.ErrCodeInternalError, (&values.InvalidUUIDValueError{Value: "x"}).Error()},
		{&values.InvalidEnumValueError{Value: "x"}, api.ErrCodeInternalError, (&values.InvalidEnumValueError{Value: "x"}).Error()},
	} {
		t.Run(fmt.Sprintf("%T/%s", c.err, c.wantCode), func(t *testing.T) {
			t.Parallel()
			got := translateExecError(fmt.Errorf("executor: converting update value for %q: %w", "COL", c.err))
			var apiErr *api.Error
			if !errors.As(got, &apiErr) || apiErr.Code != c.wantCode || apiErr.Message != c.wantMessage {
				t.Fatalf("translateExecError = %v, want %s %q", got, c.wantCode, c.wantMessage)
			}
		})
	}
}
