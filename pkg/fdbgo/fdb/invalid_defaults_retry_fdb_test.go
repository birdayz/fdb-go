package fdb_test

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

func TestFDB_InheritedTimeoutValidationAcrossCallbackRetry(t *testing.T) {
	t.Parallel()
	const invalidTimeout = int64(math.MaxInt32) + 1
	for _, owner := range []string{"database", "tenant"} {
		for _, route := range []string{"Transact", "TransactCtx", "ReadTransact", "ReadTransactCtx"} {
			t.Run(owner+"/"+route, func(t *testing.T) {
				t.Parallel()
				db, run := persistentDefaultsRoute(t, owner, route)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				write := route == "Transact" || route == "TransactCtx"
				for _, tc := range []struct {
					name      string
					initial   int64
					update    bool
					next      int64
					reset     bool
					txTimeout int64
					wantCode  int
					calls     int
				}{
					{name: "invalid_inherited", initial: invalidTimeout, wantCode: 2006, calls: 1},
					{name: "invalid_then_db_valid", initial: invalidTimeout, update: true, next: 10000, wantCode: 2006, calls: 1},
					{name: "invalid_Reset", initial: invalidTimeout, reset: true, wantCode: 2006, calls: 1},
					{name: "invalid_then_valid_Reset", initial: invalidTimeout, update: true, next: 10000, reset: true, calls: 2},
					{name: "valid_inherited", initial: 10000, calls: 2},
					{name: "valid_Reset", initial: 10000, reset: true, calls: 2},
					{name: "valid_then_db_invalid", initial: 10000, update: true, next: invalidTimeout, calls: 2},
					{name: "valid_then_invalid_Reset", initial: 10000, update: true, next: invalidTimeout, reset: true, wantCode: 2006, calls: 1},
					{name: "once_override_then_db_invalid", initial: 10000, update: true, next: invalidTimeout, txTimeout: 60000, calls: 2},
					{name: "per_tx_invalid_clears", initial: 10000, txTimeout: invalidTimeout, calls: 2},
				} {
					t.Run(tc.name, func(t *testing.T) {
						key := fdb.Key(t.Name())
						if err := db.Options().SetTransactionTimeout(tc.initial); err != nil {
							t.Fatal(err)
						}
						if err := db.Options().SetTransactionRetryLimit(2); err != nil {
							t.Fatal(err)
						}
						calls := 0
						_, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
							calls++
							if calls == 1 {
								if tc.txTimeout != 0 {
									if err := tx.Options().SetTimeout(tc.txTimeout); err != nil {
										return nil, err
									}
								}
								if tc.update {
									if err := db.Options().SetTransactionTimeout(tc.next); err != nil {
										return nil, err
									}
								}
								if tc.reset {
									tx.(fdb.Transaction).Reset()
								}
								// Do not perform a data operation that would expose 2006 before OnError.
								return nil, fdb.Error{Code: 1020}
							}
							if write {
								tx.(fdb.WritableTransaction).Set(key, []byte("committed"))
								return nil, nil
							}
							return tx.Get(key).Get()
						})
						if tc.wantCode == 0 && err != nil || tc.wantCode != 0 && !isFDBCode(err, tc.wantCode) || calls != tc.calls {
							t.Errorf("calls=%d error=%v, want calls=%d code=%d", calls, err, tc.calls, tc.wantCode)
						}

						// Observe persisted state through a fresh transaction with valid defaults.
						if err := db.Options().SetTransactionTimeout(0); err != nil {
							t.Fatal(err)
						}
						stored, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) { return tx.Get(key).Get() })
						if err != nil {
							t.Fatalf("fresh persisted-state read: %v", err)
						}
						got, ok := stored.([]byte)
						var want []byte
						if write && tc.wantCode == 0 {
							want = []byte("committed")
						}
						if !ok || !bytes.Equal(got, want) {
							t.Errorf("persisted value=%q (%T), want %q", got, stored, want)
						}
						if write {
							if _, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
								tx.(fdb.WritableTransaction).Clear(key)
								return nil, nil
							}); err != nil {
								t.Fatalf("clear persisted-state probe: %v", err)
							}
						}
					})
				}
			})
		}
	}
}
