package fdb_test

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
)

func TestFDB_WrappedNativePoisonDoesNotBecomePersistentTimeoutError(t *testing.T) {
	t.Parallel()
	const invalidTimeout = int64(math.MaxInt32) + 1
	for _, owner := range []string{"database", "tenant"} {
		for _, route := range []string{"Transact", "TransactCtx", "ReadTransact", "ReadTransactCtx"} {
			t.Run(owner+"/"+route, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				native, err := client.OpenDatabaseFromConfig(ctx, sharedClusterFile, client.WithAPIVersion(730))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := native.Close(); err != nil {
						t.Errorf("close native database: %v", err)
					}
				})
				db, run := persistentDefaultsRouteForDatabase(t, fdb.WrapDatabase(native), owner, route)
				write := route == "Transact" || route == "TransactCtx"
				for _, tc := range []struct {
					name        string
					poison      bool
					timeout     int64
					update      bool
					next        int64
					reset       bool
					delayRepair bool
					manualRetry bool
					setTimeout  bool
					txTimeout   int64
					wantCode    int
				}{
					{name: "repair_native_size", poison: true, timeout: 10000},
					{name: "repair_with_db_timeout_changed", poison: true, timeout: 10000, update: true, next: invalidTimeout},
					{name: "repair_with_invalid_timeout", poison: true, timeout: invalidTimeout, wantCode: 2006},
					{name: "invalid_timeout_db_changed", poison: true, timeout: invalidTimeout, update: true, next: 10000, wantCode: 2006},
					{name: "repair_on_second_callback", poison: true, timeout: 10000, delayRepair: true},
					{name: "valid_unpoisoned_control", timeout: 10000},
					{name: "invalid_unpoisoned_control", timeout: invalidTimeout, wantCode: 2006},
					{name: "Reset_refreshes_pending_timeout", poison: true, timeout: 10000, update: true, next: 20000, reset: true},
					{name: "Reset_replaces_invalid_pending_timeout", poison: true, timeout: invalidTimeout, update: true, next: 10000, reset: true},
					{name: "manual_OnError_positive_override", poison: true, timeout: 10000, manualRetry: true, setTimeout: true, txTimeout: 60000},
					{name: "manual_OnError_disable_override", poison: true, timeout: 10000, manualRetry: true, setTimeout: true, txTimeout: 0},
					{name: "manual_OnError_invalid_override", poison: true, timeout: 10000, manualRetry: true, setTimeout: true, txTimeout: invalidTimeout},
					{name: "poisoned_positive_override_skipped", poison: true, timeout: 10000, setTimeout: true, txTimeout: 60000},
					{name: "poisoned_disable_override_skipped", poison: true, timeout: 10000, setTimeout: true, txTimeout: 0},
				} {
					t.Run(tc.name, func(t *testing.T) {
						key := fdb.Key(t.Name())
						native.SetTransactionSizeLimit(10000000)
						if tc.poison {
							native.SetTransactionSizeLimit(1)
						}
						if err := db.Options().SetTransactionTimeout(tc.timeout); err != nil {
							t.Fatal(err)
						}
						if err := db.Options().SetTransactionRetryLimit(4); err != nil {
							t.Fatal(err)
						}
						if err := db.Options().SetTransactionMaxRetryDelay(7); err != nil {
							t.Fatal(err)
						}
						calls := 0
						_, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
							calls++
							if calls == 1 {
								wantInitial := 10000 * time.Millisecond
								if tc.poison || tc.timeout == invalidTimeout {
									wantInitial = 0
									if _, err := tx.(fdb.Transaction).GetApproximateSize().Get(); !isFDBCode(err, 2006) {
										t.Errorf("initial native poison: %v, want 2006", err)
									}
								}
								if got := effectiveTransactionTimeout(t, tx); got != wantInitial {
									t.Errorf("initial timeout=%v, want %v", got, wantInitial)
								}
								if !tc.delayRepair {
									native.SetTransactionSizeLimit(10000000)
								}
								if tc.update {
									if err := db.Options().SetTransactionTimeout(tc.next); err != nil {
										return nil, err
									}
								}
								if tc.reset {
									tx.(fdb.Transaction).Reset()
								}
								limit := int64(2)
								if tc.manualRetry {
									if err := tx.(fdb.Transaction).OnError(fdb.Error{Code: 1020}).Get(); err != nil {
										return nil, err
									}
									limit++
								}
								if tc.setTimeout {
									if err := tx.Options().SetTimeout(tc.txTimeout); err != nil {
										return nil, err
									}
								}
								if err := tx.Options().SetRetryLimit(limit); err != nil {
									return nil, err
								}
								if err := tx.Options().SetMaxRetryDelay(3); err != nil {
									return nil, err
								}
								return nil, fdb.Error{Code: 1020}
							}
							if calls == 2 && tc.delayRepair {
								if got := effectiveTransactionTimeout(t, tx); got != 0 {
									t.Errorf("still-poisoned timeout=%v, want 0", got)
								}
								native.SetTransactionSizeLimit(10000000)
								return nil, fdb.Error{Code: 1020}
							}
							wantTimeout := time.Duration(tc.timeout) * time.Millisecond
							if tc.reset {
								wantTimeout = time.Duration(tc.next) * time.Millisecond
							}
							if tc.manualRetry && tc.setTimeout && tc.txTimeout >= 0 && tc.txTimeout <= math.MaxInt32 {
								wantTimeout = time.Duration(tc.txTimeout) * time.Millisecond
							}
							if got := effectiveTransactionTimeout(t, tx); got != wantTimeout {
								t.Errorf("callback %d: installed timeout=%v, want captured %v", calls, got, wantTimeout)
							}
							if got := effectiveRetryBackoff(t, tx); got != 3*time.Millisecond {
								t.Errorf("callback %d: next backoff=%v, want retained 3ms override", calls, got)
							}
							if calls < 3 {
								return nil, fdb.Error{Code: 1020}
							}
							if write {
								tx.(fdb.WritableTransaction).Set(key, []byte("committed"))
								return nil, nil
							}
							return tx.Get(key).Get()
						})
						wantCalls := 3
						if tc.wantCode != 0 {
							wantCalls = 1
						}
						if tc.wantCode == 0 && err != nil || tc.wantCode != 0 && !isFDBCode(err, tc.wantCode) || calls != wantCalls {
							t.Errorf("calls=%d error=%v, want calls=%d code=%d", calls, err, wantCalls, tc.wantCode)
						}

						native.SetTransactionSizeLimit(10000000)
						if err := db.Options().SetTransactionTimeout(0); err != nil {
							t.Fatal(err)
						}
						stored, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) { return tx.Get(key).Get() })
						if err != nil {
							t.Fatal(err)
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
								t.Fatal(err)
							}
						}
					})
				}
			})
		}
	}
}
