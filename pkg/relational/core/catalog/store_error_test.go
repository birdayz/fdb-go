package catalog

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/relational/api"
)

// A catalog store failure caused by an FDB transaction timeout is 53F00, as
// Java's ExceptionUtil maps FDBStoreTransactionTimeoutException; anything else is XX000.
func TestStoreError_TransactionTimeoutIs53F00(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cause error
		want  api.ErrorCode
	}{
		{fmt.Errorf("failed to read store info: %w", &wire.FDBError{Code: 1031}), api.ErrCodeTransactionTimeout},
		{fmt.Errorf("failed to read store info: %w", fdb.Error{Code: 1031}), api.ErrCodeTransactionTimeout},
		{fmt.Errorf("failed to read store info: %w", &wire.FDBError{Code: 1020}), api.ErrCodeInternalError},
		{errors.New("corrupt header"), api.ErrCodeInternalError},
	} {
		err := storeError(tc.cause, "open catalog store")
		if err.Code != tc.want || !errors.Is(err, tc.cause) {
			t.Errorf("%v: got %v, want %s wrapping the cause", tc.cause, err, tc.want)
		}
	}
}
