package fdb_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

func awaitStamp(t *testing.T, f fdb.FutureKey) (fdb.Key, error) {
	t.Helper()
	type result struct {
		value fdb.Key
		err   error
	}
	done := make(chan result, 1)
	go func() { value, err := f.Get(); done <- result{value, err} }()
	select {
	case r := <-done:
		return r.value, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("versionstamp future remained unresolved")
		return nil, nil
	}
}

func requireFacadeStampCode(t *testing.T, err error, want int) {
	t.Helper()
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != want {
		t.Fatalf("got %v, want FDB %d", err, want)
	}
}

func checkManagedVersionstampAttempts(t *testing.T, owner fdb.CtxTransactor) {
	t.Helper()
	for _, mode := range []string{"retry", "terminal", "caller-exit", "no-write"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			key := fdb.Key(t.Name())
			var futures []fdb.FutureKey
			_, err := owner.TransactCtx(ctx, func(tr fdb.WritableTransaction) (any, error) {
				futures = append(futures, tr.GetVersionstamp())
				switch mode {
				case "retry":
					if len(futures) == 1 {
						return nil, fdb.Error{Code: 1020}
					}
				case "terminal":
					return nil, fdb.Error{Code: 2015}
				case "caller-exit":
					cancel()
					return nil, nil
				case "no-write":
					return nil, nil
				}
				tr.SetVersionstampedValue(key, make([]byte, 14))
				return nil, nil
			})
			if len(futures) == 0 {
				t.Fatal("callback did not execute")
			}
			switch mode {
			case "retry":
				if err != nil || len(futures) != 2 {
					t.Fatalf("managed retries: attempts=%d err=%v", len(futures), err)
				}
				_, firstErr := awaitStamp(t, futures[0])
				requireFacadeStampCode(t, firstErr, 1025)
				stamp, stampErr := awaitStamp(t, futures[1])
				if stampErr != nil || len(stamp) != 10 {
					t.Fatalf("successful attempt: %x, %v", stamp, stampErr)
				}
				stored, readErr := owner.TransactCtx(ctx, func(tr fdb.WritableTransaction) (any, error) { return tr.Get(key).Get() })
				if readErr != nil || !bytes.Equal(stamp, stored.([]byte)) {
					t.Fatalf("managed stamp=%x stored=%x err=%v", stamp, stored, readErr)
				}
			case "terminal":
				requireFacadeStampCode(t, err, 2015)
				_, stampErr := awaitStamp(t, futures[0])
				requireFacadeStampCode(t, stampErr, 1025)
			case "caller-exit":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("managed caller exit: %v", err)
				}
				_, stampErr := awaitStamp(t, futures[0])
				if !errors.Is(stampErr, context.Canceled) {
					t.Fatalf("captured caller interruption: %v", stampErr)
				}
			case "no-write":
				if err != nil {
					t.Fatal(err)
				}
				_, stampErr := awaitStamp(t, futures[0])
				requireFacadeStampCode(t, stampErr, 2021)
			}
		})
	}
}

func TestManagedVersionstampDatabaseAttempts(t *testing.T) {
	t.Parallel()
	checkManagedVersionstampAttempts(t, openTestDB(t))
}

func TestManagedVersionstampTenantAttempts(t *testing.T) {
	t.Parallel()
	db, _ := openTestDBWithTenants(t)
	name := fdb.Key(t.Name())
	if err := db.CreateTenant(name); err != nil {
		t.Fatal(err)
	}
	tenant, err := db.OpenTenant(name)
	if err != nil {
		t.Fatal(err)
	}
	checkManagedVersionstampAttempts(t, tenant)
}

func TestVersionstampFacadeResetAndFailedReuse(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cancel()
	oldPending := tr.GetVersionstamp()
	tr.Reset()
	_, err = awaitStamp(t, oldPending)
	requireFacadeStampCode(t, err, 1025)
	ready := tr.GetVersionstamp()
	key := fdb.Key(t.Name())
	tr.SetVersionstampedValue(key, make([]byte, 14))
	if err := tr.Commit().Get(); err != nil {
		t.Fatal(err)
	}
	stamp, err := awaitStamp(t, ready)
	if err != nil || len(stamp) != 10 {
		t.Fatalf("first stamp: %x, %v", stamp, err)
	}
	retained, err := awaitStamp(t, tr.GetVersionstamp())
	if err != nil || !bytes.Equal(stamp, retained) {
		t.Fatalf("auto-reuse getter: %x, %v", retained, err)
	}
	if err := tr.Options().SetSizeLimit(32); err != nil {
		t.Fatal(err)
	}
	tr.Set(key, make([]byte, 64))
	second := tr.Commit() // producer is captured before starting its goroutine
	failedStamp := tr.GetVersionstamp()
	requireFacadeStampCode(t, second.Get(), 2101)
	_, err = awaitStamp(t, failedStamp)
	requireFacadeStampCode(t, err, 2020)
	tr.Reset()
	retained, err = awaitStamp(t, ready)
	if err != nil || !bytes.Equal(stamp, retained) {
		t.Fatalf("Reset changed old ready stamp: %x, %v", retained, err)
	}
	fresh := tr.GetVersionstamp()
	if fresh.IsReady() {
		t.Fatal("replacement future inherited old completion")
	}
	tr.Cancel()
	_, err = awaitStamp(t, fresh)
	requireFacadeStampCode(t, err, 1025)
}

func TestVersionstampReadTransactUnsupported(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := db.ReadTransact(func(rt fdb.ReadTransaction) (any, error) {
		tr, ok := rt.(fdb.Transaction)
		if !ok {
			t.Fatalf("unexpected wrapper %T", rt)
		}
		_, err := awaitStamp(t, tr.GetVersionstamp())
		requireFacadeStampCode(t, err, 2015)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
