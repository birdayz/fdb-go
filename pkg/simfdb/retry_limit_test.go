package simfdb_test

import (
	"errors"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

// The retry_limit option on the simulator, as the client honours it (client/transaction.go:2504,
// C++ Transaction::onError): OnError grants at most retryLimit retries, then returns the input
// error. The record layer's attempt loop sets 0 so that each of its attempts is ONE backend
// attempt; when SimDB ignored the option, every attempt on the simulator was up to 101 backend
// attempts and no DST run saw the bound.
func TestSimRetryLimit_TransactStopsAtTheLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		limit int64
		want  int // body runs
	}{
		{"zero: the first error escapes", 0, 1},
		{"two retries", 2, 3},
		{"negative: unlimited up to the simulator's backstop", -1, 101},
	} {
		db := simfdb.New(nil)
		runs := 0
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			runs++
			if err := tr.Options().SetRetryLimit(tc.limit); err != nil {
				return nil, err
			}
			return nil, fdb.Error{Code: 1020}
		})
		var fe fdb.Error
		if !errors.As(err, &fe) || fe.Code != 1020 {
			t.Fatalf("%s: err = %v, want 1020", tc.name, err)
		}
		if runs != tc.want {
			t.Fatalf("%s: body ran %d times, want %d", tc.name, runs, tc.want)
		}
	}
}

func TestSimRetryLimit_ReadTransactStopsAtTheLimit(t *testing.T) {
	t.Parallel()
	db := simfdb.New(nil)
	runs := 0
	_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		runs++
		if err := tr.Options().SetRetryLimit(1); err != nil {
			return nil, err
		}
		return nil, fdb.Error{Code: 1007}
	})
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != 1007 {
		t.Fatalf("err = %v, want 1007", err)
	}
	if runs != 2 {
		t.Fatalf("body ran %d times, want 2 (one retry)", runs)
	}
}

// The option is persistent across an OnError retry (set once on the first attempt, it still
// bounds the later ones), and a user Reset reverts it to unlimited and zeroes the count.
func TestSimRetryLimit_PersistsAcrossOnErrorAndClearsOnReset(t *testing.T) {
	t.Parallel()
	db := simfdb.New(nil)
	tr, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Options().SetRetryLimit(1); err != nil {
		t.Fatal(err)
	}
	if err := tr.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
		t.Fatalf("first OnError under limit 1 = %v, want a granted retry", err)
	}
	if err := tr.OnError(fdb.Error{Code: 1020}).Get(); err == nil {
		t.Fatal("second OnError under limit 1 granted a retry; the limit did not survive the first")
	}
	tr.Reset()
	for i := 0; i < 5; i++ {
		if err := tr.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
			t.Fatalf("OnError %d after Reset = %v; Reset must restore the unlimited default", i, err)
		}
	}
	if err := tr.Options().SetRetryLimit(0); err != nil {
		t.Fatal(err)
	}
	if err := tr.OnError(fdb.Error{Code: 1020}).Get(); err == nil {
		t.Fatal("limit 0 granted a retry")
	}
}

// wrappedConflict is a typed error a body puts around an FDB error, as the record layer's
// typed errors and %w wrappers do.
type wrappedConflict struct{ inner error }

func (w *wrappedConflict) Error() string { return "record layer: " + w.inner.Error() }
func (w *wrappedConflict) Unwrap() error { return w.inner }

// At the retry limit the loop returns the BODY's error, its chain intact, as the pure-Go
// client's OnError(ctx, err) returns the caller's error (client/transaction.go, the retryLimit
// check): the simulator's OnError takes the extracted fdb.Error and hands it back bare, and
// returning that dropped every wrapper around it.
func TestSimRetryLimit_DeclinedRetryKeepsTheBodysErrorChain(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, err error) {
		t.Helper()
		var w *wrappedConflict
		if !errors.As(err, &w) {
			t.Fatalf("err = %v (%T): the body's wrapper was dropped", err, err)
		}
		var fe fdb.Error
		if !errors.As(err, &fe) || fe.Code != 1020 {
			t.Fatalf("err = %v: the FDB code must stay reachable through the wrapper", err)
		}
	}
	t.Run("Transact", func(t *testing.T) {
		t.Parallel()
		db := simfdb.New(nil)
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			if err := tr.Options().SetRetryLimit(0); err != nil {
				return nil, err
			}
			return nil, &wrappedConflict{inner: fdb.Error{Code: 1020}}
		})
		check(t, err)
	})
	t.Run("ReadTransact", func(t *testing.T) {
		t.Parallel()
		db := simfdb.New(nil)
		_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			if err := tr.Options().SetRetryLimit(0); err != nil {
				return nil, err
			}
			return nil, &wrappedConflict{inner: fdb.Error{Code: 1020}}
		})
		check(t, err)
	})
}
