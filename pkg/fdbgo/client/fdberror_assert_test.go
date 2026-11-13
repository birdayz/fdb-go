package client

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

func assertFDBErrorCode(t *testing.T, err error, wantCode int) {
	t.Helper()
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("error %v is not a wire.FDBError", err)
	}
	if fdbErr.Code != wantCode {
		t.Fatalf("error code = %d, want %d", fdbErr.Code, wantCode)
	}
}
