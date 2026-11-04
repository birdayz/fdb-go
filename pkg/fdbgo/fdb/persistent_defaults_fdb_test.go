package fdb_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

// Each route must inherit persistent defaults once, not overwrite overrides when
// OnError re-enters the callback (ReadYourWrites.actor.cpp:2680-2754).
func persistentDefaultsRoute(t *testing.T, owner, route string) (fdb.Database, func(context.Context, func(fdb.ReadTransaction) (any, error)) (any, error)) {
	t.Helper()
	return persistentDefaultsRouteForDatabase(t, openTestDB(t), owner, route)
}

func persistentDefaultsRouteForDatabase(t *testing.T, db fdb.Database, owner, route string) (fdb.Database, func(context.Context, func(fdb.ReadTransaction) (any, error)) (any, error)) {
	t.Helper()
	var transactor fdb.Transactor = db
	create := db.CreateTransaction
	if owner == "tenant" {
		name := fdb.Key(t.Name())
		if err := db.CreateTenant(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = db.Options().SetTransactionTimeout(0)
			_ = db.Options().SetTransactionRetryLimit(10)
			if err := db.DeleteTenant(name); err != nil {
				t.Errorf("DeleteTenant: %v", err)
			}
		})
		tenant, err := db.OpenTenant(name)
		if err != nil {
			t.Fatal(err)
		}
		transactor, create = tenant, tenant.CreateTransaction
	}
	run := func(ctx context.Context, body func(fdb.ReadTransaction) (any, error)) (any, error) {
		switch route {
		case "Transact":
			return transactor.Transact(func(tx fdb.WritableTransaction) (any, error) { return body(tx) })
		case "TransactCtx":
			return transactor.(fdb.CtxTransactor).TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) { return body(tx) })
		case "ReadTransact":
			return transactor.ReadTransact(body)
		case "ReadTransactCtx":
			return transactor.(fdb.CtxReadTransactor).ReadTransactCtx(ctx, body)
		case "CreateTransaction":
			tx, err := create()
			if err != nil {
				return nil, err
			}
			defer tx.Cancel()
			for ctx.Err() == nil {
				result, err := body(tx)
				if err == nil {
					return result, tx.Commit().Get()
				}
				var fe fdb.Error
				if !errors.As(err, &fe) {
					return nil, err
				}
				if err := tx.OnError(fe).Get(); err != nil {
					return nil, err
				}
			}
			return nil, ctx.Err()
		default:
			panic("unknown transaction route")
		}
	}
	return db, run
}

// Read-only reflection avoids adding public inspection hooks for the facade's private native handle.
func nativeTransactionValue(t *testing.T, tx fdb.ReadTransaction) reflect.Value {
	t.Helper()
	v := reflect.ValueOf(tx)
	for _, field := range []string{"t", "inner"} {
		if !v.IsValid() || v.Kind() != reflect.Struct {
			t.Fatalf("native inspection: %T has no struct containing %q", tx, field)
		}
		v = v.FieldByName(field)
		if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
			t.Fatalf("native inspection: %T field %q is not a non-nil pointer", tx, field)
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		t.Fatalf("native inspection: %T native handle is not a struct", tx)
	}
	return v
}

// Inspect the computed next backoff after OnError, not the configured cap or elapsed time.
func effectiveRetryBackoff(t *testing.T, tx fdb.ReadTransaction) time.Duration {
	t.Helper()
	v := nativeTransactionValue(t, tx).FieldByName("backoff")
	if !v.IsValid() || v.Kind() != reflect.Int64 {
		t.Fatalf("effective backoff: %T native backoff is not an int64 duration", tx)
	}
	return time.Duration(v.Int())
}

func effectiveTransactionTimeout(t *testing.T, tx fdb.ReadTransaction) time.Duration {
	t.Helper()
	v := nativeTransactionValue(t, tx).FieldByName("timeoutNs")
	if !v.IsValid() || v.Kind() != reflect.Struct {
		t.Fatalf("effective timeout: %T native timeoutNs is not an atomic struct", tx)
	}
	v = v.FieldByName("v")
	if !v.IsValid() || v.Kind() != reflect.Int64 {
		t.Fatalf("effective timeout: %T atomic timeout value is not an int64 duration", tx)
	}
	return time.Duration(v.Int())
}

func TestFDB_PersistentDatabaseDefaultsRespectOverrides(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"database", "tenant"} {
		for _, route := range []string{"Transact", "TransactCtx", "ReadTransact", "ReadTransactCtx", "CreateTransaction"} {
			t.Run(owner+"/"+route, func(t *testing.T) {
				t.Parallel()
				db, run := persistentDefaultsRoute(t, owner, route)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				for _, tc := range []struct {
					name     string
					dbLimit  int64
					override bool
					txLimit  int64
					every    bool
					want     int
				}{
					{"inherited", 1, false, 0, false, 2},
					{"shorter_once", 4, true, 1, false, 2},
					{"shorter_each", 4, true, 1, true, 2},
					{"longer_once", 0, true, 2, false, 3},
				} {
					t.Run(tc.name, func(t *testing.T) {
						if err := db.Options().SetTransactionRetryLimit(tc.dbLimit); err != nil {
							t.Fatal(err)
						}
						calls := 0
						_, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
							calls++
							if tc.override && (calls == 1 || tc.every) {
								if err := tx.Options().SetRetryLimit(tc.txLimit); err != nil {
									return nil, err
								}
							}
							if calls > 6 {
								return nil, errors.New("retry limit was not honored")
							}
							return nil, fdb.Error{Code: 1020}
						})
						if !isFDBCode(err, 1020) || calls != tc.want {
							t.Fatalf("calls=%d error=%v, want %d calls and 1020", calls, err, tc.want)
						}
					})
				}

				for _, override := range []bool{false, true} {
					name := "max_delay_inherited"
					if override {
						name = "max_delay_override_once"
					}
					t.Run(name, func(t *testing.T) {
						if err := db.Options().SetTransactionMaxRetryDelay(7); err != nil {
							t.Fatal(err)
						}
						if err := db.Options().SetTransactionRetryLimit(3); err != nil {
							t.Fatal(err)
						}
						want := 7 * time.Millisecond
						if override {
							want = 3 * time.Millisecond
						}
						calls := 0
						_, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
							calls++
							if calls == 1 && override {
								if err := tx.Options().SetMaxRetryDelay(3); err != nil {
									return nil, err
								}
							}
							if calls > 1 {
								if got := effectiveRetryBackoff(t, tx); got != want {
									t.Errorf("callback %d: effective next backoff=%v, want %v", calls, got, want)
									return nil, errors.New("maximum retry delay changed across callback reentry")
								}
							}
							if calls < 4 {
								return nil, fdb.Error{Code: 1020}
							}
							return nil, nil
						})
						if err != nil || calls != 4 {
							t.Fatalf("calls=%d error=%v, want four callbacks under the effective delay cap", calls, err)
						}
					})
				}

				t.Run("longer_timeout_once", func(t *testing.T) {
					if err := db.Options().SetTransactionTimeout(1000); err != nil {
						t.Fatal(err)
					}
					if err := db.Options().SetTransactionRetryLimit(1); err != nil {
						t.Fatal(err)
					}
					calls := 0
					_, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
						calls++
						if calls == 1 {
							if err := tx.Options().SetTimeout(10000); err != nil {
								return nil, err
							}
							// Cross the DB timeout while the longer per-tx budget is active.
							timer := time.NewTimer(1500 * time.Millisecond)
							defer timer.Stop()
							select {
							case <-timer.C:
							case <-ctx.Done():
								return nil, ctx.Err()
							}
							return nil, fdb.Error{Code: 1020}
						}
						return tx.Get(fdb.Key(t.Name())).Get()
					})
					if err != nil || calls != 2 {
						t.Fatalf("calls=%d error=%v; longer timeout must survive OnError", calls, err)
					}
				})
			})
		}
	}
}

func TestFDB_InvalidDatabaseTimeoutSurvivesReset(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"database", "tenant"} {
		for _, reset := range []bool{false, true} {
			name := owner + "/construction"
			if reset {
				name = owner + "/Reset"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				db, run := persistentDefaultsRoute(t, owner, "CreateTransaction")
				if err := db.Options().SetTransactionTimeout(int64(math.MaxInt32) + 1); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
					tr := tx.(fdb.Transaction)
					if reset {
						tr.Reset()
					}
					tr.Set(fdb.Key(t.Name()), []byte("must not commit"))
					return nil, nil
				})
				if !isFDBCode(err, 2006) {
					t.Fatalf("oversized inherited timeout: %v, want invalid_option_value (2006)", err)
				}
			})
		}
	}
}
