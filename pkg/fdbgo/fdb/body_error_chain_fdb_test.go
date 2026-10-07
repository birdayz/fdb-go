package fdb_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// splitWindowError stands in for a record-layer error that wraps a retryable
// FDB code (SPFreshSplitWindowError wraps not_committed).
type splitWindowError struct{ cause error }

func (e *splitWindowError) Error() string { return "split window: " + e.cause.Error() }
func (e *splitWindowError) Unwrap() error { return e.cause }

func boundedDB(t *testing.T) fdb.Database {
	t.Helper()
	db := openTestDB(t)
	if err := db.Options().SetTransactionRetryLimit(2); err != nil {
		t.Fatalf("SetTransactionRetryLimit: %v", err)
	}
	return db
}

func requireBodyChain(t *testing.T, err error, attempts int) {
	t.Helper()
	var sw *splitWindowError
	if !errors.As(err, &sw) {
		t.Fatalf("terminal error %v (%T) lost the body's chain; the Apple binding keeps the body's error when OnError re-raises its code", err, err)
	}
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != 1020 {
		t.Fatalf("terminal error %v must still carry fdb.Error 1020", err)
	}
	if attempts != 3 {
		t.Fatalf("body ran %d times, want 3 (first run + retry limit 2)", attempts)
	}
}

// TestFDB_TransactCtx_KeepsBodyErrorChain pins the Apple binding's retryable rule
// (bindings/go/src/fdb/database.go:163-191 at 7.3.77) on the pure-Go wrapper:
// when OnError re-raises the body's code at the retry limit, the caller gets the
// error the body returned, chain intact, not a bare fdb.Error{Code}.
func TestFDB_TransactCtx_KeepsBodyErrorChain(t *testing.T) {
	t.Parallel()
	db := boundedDB(t)
	attempts := 0
	_, err := db.TransactCtx(context.Background(), func(fdb.WritableTransaction) (any, error) {
		attempts++
		return nil, &splitWindowError{cause: fdb.Error{Code: 1020}}
	})
	requireBodyChain(t, err, attempts)
}

func TestFDB_ReadTransactCtx_KeepsBodyErrorChain(t *testing.T) {
	t.Parallel()
	db := boundedDB(t)
	attempts := 0
	_, err := db.ReadTransactCtx(context.Background(), func(fdb.ReadTransaction) (any, error) {
		attempts++
		return nil, fmt.Errorf("read body: %w", &splitWindowError{cause: fdb.Error{Code: 1020}})
	})
	requireBodyChain(t, err, attempts)
}

// TestFDB_TransactCtx_KeepsPanickedErrorChain covers the MustGet route: an error
// that reaches the loop as a panic is kept as the panic value.
func TestFDB_TransactCtx_KeepsPanickedErrorChain(t *testing.T) {
	t.Parallel()
	db := boundedDB(t)
	attempts := 0
	_, err := db.TransactCtx(context.Background(), func(fdb.WritableTransaction) (any, error) {
		attempts++
		panic(&splitWindowError{cause: fdb.Error{Code: 1020}})
	})
	requireBodyChain(t, err, attempts)
}

// TestFDB_TransactCtx_KeptErrorIsTheLastExecutions pins the per-execution rule:
// the kept error belongs to the final execution only. An earlier execution's
// typed error must not be paired with a later failure of the same code.
func TestFDB_TransactCtx_KeptErrorIsTheLastExecutions(t *testing.T) {
	t.Parallel()
	db := boundedDB(t)
	attempts := 0
	_, err := db.TransactCtx(context.Background(), func(fdb.WritableTransaction) (any, error) {
		attempts++
		if attempts == 1 {
			return nil, &splitWindowError{cause: fdb.Error{Code: 1020}}
		}
		return nil, fdb.Error{Code: 1020}
	})
	var sw *splitWindowError
	if errors.As(err, &sw) {
		t.Fatalf("terminal error %v carries execution 1's chain; the last execution returned a bare 1020", err)
	}
	if err != (fdb.Error{Code: 1020}) {
		t.Fatalf("terminal error = %v (%T), want fdb.Error{1020}", err, err)
	}
}
