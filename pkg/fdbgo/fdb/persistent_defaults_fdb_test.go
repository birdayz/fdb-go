package fdb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

// Each route must inherit persistent defaults once, not overwrite overrides when
// OnError re-enters the callback (ReadYourWrites.actor.cpp:2680-2754).
func persistentDefaultsRoute(t *testing.T, owner, route string) (fdb.Database, func(context.Context, func(fdb.ReadTransaction) (any, error)) (any, error)) {
	t.Helper()
	db := openTestDB(t)
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
