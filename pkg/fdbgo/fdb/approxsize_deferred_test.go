package fdb_test

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// TestGetApproximateSize_DeferredErrorIsFDBError pins the facade error contract for
// GetApproximateSize's deferred-error gate: like every other facade method, the error
// must surface as fdb.Error (via convertError), matchable with errors.As — never a raw
// client error type escaping through the ready future.
func TestGetApproximateSize_DeferredErrorIsFDBError(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	k := fdb.Key(t.Name() + "_k")

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tx := tr.(fdb.Transaction)
		if _, gerr := tx.Get(k).Get(); gerr != nil {
			return nil, gerr
		}
		// RYW-disable after a read → deferred client_invalid_operation (2000, RFC-059).
		if oerr := tx.Options().SetReadYourWritesDisable(); oerr != nil {
			return nil, oerr
		}
		_, sizeErr := tx.GetApproximateSize().Get()
		var fe fdb.Error
		if !errors.As(sizeErr, &fe) || fe.Code != 2000 {
			t.Fatalf("GetApproximateSize on a poisoned txn: want fdb.Error{Code:2000} via errors.As, got %[1]T %[1]v", sizeErr)
		}
		return nil, nil
	})
	// The poisoned commit then fails the Transact loop with the same non-retryable 2000.
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != 2000 {
		t.Fatalf("Transact over the poisoned txn: want fdb.Error{Code:2000}, got %[1]T %[1]v", err)
	}
}
