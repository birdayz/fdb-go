package catalog

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/relational/api"
)

// An FDB-coded catalog store failure keeps its FDB code, unconverted, so the
// connection's translateFDBError is its single mapper; anything else is XX000.
func TestStoreError_LeavesFDBCodesToTheConnection(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{
		fmt.Errorf("failed to read store info: %w", &wire.FDBError{Code: 1031}),
		fmt.Errorf("failed to read store info: %w", fdb.Error{Code: 1007}),
		&wire.FDBError{Code: 1025},
	} {
		err := storeError(cause, "open catalog store")
		var apiErr *api.Error
		if errors.As(err, &apiErr) || !errors.Is(err, cause) {
			t.Errorf("%v: got %v, want the FDB error wrapped without a SQLSTATE", cause, err)
		}
	}
	cause := errors.New("corrupt header")
	var apiErr *api.Error
	if err := storeError(cause, "open catalog store"); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInternalError || !errors.Is(err, cause) {
		t.Errorf("non-FDB failure: got %v, want XX000 wrapping the cause", err)
	}
}
