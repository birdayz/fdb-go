package fdb_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

type nonpersistentDefaultsState struct {
	sizeLimit        int64
	readSystemKeys   bool
	snapshotDisable  int64
	bypassUnreadable bool
	causalReadRisky  bool
}

// Inspect only at synchronous callback boundaries, with no outstanding reads or option setters.
func nonpersistentState(t *testing.T, tx fdb.ReadTransaction) nonpersistentDefaultsState {
	t.Helper()
	native := nativeTransactionValue(t, tx)
	integer := func(name string, kind reflect.Kind) int64 {
		v := native.FieldByName(name)
		if !v.IsValid() || v.Kind() != kind {
			t.Fatalf("nonpersistent inspection: native %q is not %s", name, kind)
		}
		return v.Int()
	}
	boolean := func(owner reflect.Value, name string) bool {
		v := owner.FieldByName(name)
		if !v.IsValid() || v.Kind() != reflect.Bool {
			t.Fatalf("nonpersistent inspection: %q is not bool", name)
		}
		return v.Bool()
	}
	ryw := native.FieldByName("ryw")
	if !ryw.IsValid() || ryw.Kind() != reflect.Struct {
		t.Fatal("nonpersistent inspection: native ryw is not a struct")
	}
	writes := ryw.FieldByName("w")
	if !writes.IsValid() || writes.Kind() != reflect.Pointer {
		t.Fatal("nonpersistent inspection: live write map is not a pointer")
	}
	if writes.IsNil() {
		writes = ryw.FieldByName("base")
	} else {
		writes = writes.Elem()
	}
	if !writes.IsValid() || writes.Kind() != reflect.Struct {
		t.Fatal("nonpersistent inspection: write map version is not a struct")
	}
	return nonpersistentDefaultsState{
		sizeLimit:        integer("sizeLimit", reflect.Int64),
		readSystemKeys:   boolean(native, "readSystemKeys"),
		snapshotDisable:  integer("snapshotRYWDisableCount", reflect.Int),
		bypassUnreadable: boolean(writes, "bypassUnreadable"),
		causalReadRisky:  boolean(native, "causalReadRisky"),
	}
}

func assertNonpersistentState(t *testing.T, tx fdb.ReadTransaction, callback int, phase string, want nonpersistentDefaultsState) {
	t.Helper()
	got := nonpersistentState(t, tx)
	if got.sizeLimit != want.sizeLimit {
		t.Errorf("callback %d %s: sizeLimit=%d, want %d", callback, phase, got.sizeLimit, want.sizeLimit)
	}
	if got.readSystemKeys != want.readSystemKeys {
		t.Errorf("callback %d %s: readSystemKeys=%t, want %t", callback, phase, got.readSystemKeys, want.readSystemKeys)
	}
	if got.snapshotDisable != want.snapshotDisable {
		t.Errorf("callback %d %s: snapshotRYWDisableCount=%d, want %d", callback, phase, got.snapshotDisable, want.snapshotDisable)
	}
	if got.bypassUnreadable != want.bypassUnreadable {
		t.Errorf("callback %d %s: bypassUnreadable=%t, want %t", callback, phase, got.bypassUnreadable, want.bypassUnreadable)
	}
	if got.causalReadRisky != want.causalReadRisky {
		t.Errorf("callback %d %s: causalReadRisky=%t, want %t", callback, phase, got.causalReadRisky, want.causalReadRisky)
	}
}

func TestFDB_NonpersistentDefaultsReappliedAfterAutomaticRetry(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"database", "tenant"} {
		for _, route := range []string{"Transact", "TransactCtx", "ReadTransact", "ReadTransactCtx"} {
			for _, defaultsOn := range []bool{false, true} {
				name := "defaults_off_once_on"
				if defaultsOn {
					name = "defaults_on_restored"
				}
				t.Run(owner+"/"+route+"/"+name, func(t *testing.T) {
					t.Parallel()
					db, run := persistentDefaultsRoute(t, owner, route)
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if err := db.Options().SetTransactionRetryLimit(2); err != nil {
						t.Fatal(err)
					}
					if defaultsOn {
						// Tenant management cleanup must not inherit the deliberately small test limit.
						t.Cleanup(func() {
							if err := db.Options().SetTransactionSizeLimit(10000000); err != nil {
								t.Errorf("restore size limit for cleanup: %v", err)
							}
						})
						for _, err := range []error{
							db.Options().SetTransactionSizeLimit(1000),
							db.Options().SetReadSystemKeys(),
							db.Options().SetSnapshotRywDisable(),
							db.Options().SetTransactionBypassUnreadable(),
							db.Options().SetTransactionCausalReadRisky(),
						} {
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					want := nonpersistentDefaultsState{sizeLimit: 10000000}
					if defaultsOn {
						want = nonpersistentDefaultsState{sizeLimit: 1000, readSystemKeys: true, snapshotDisable: 1, bypassUnreadable: true, causalReadRisky: true}
					}
					attemptKey := fdb.Key(t.Name() + "/attempt")
					snapshotKey := fdb.Key(t.Name() + "/snapshot")
					stampKey := fdb.Key(t.Name() + "/stamp")
					finalKey := fdb.Key(t.Name() + "/final")
					payload := bytes.Repeat([]byte("v"), 1500)
					write := route == "Transact" || route == "TransactCtx"
					calls := 0
					_, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
						calls++
						assertNonpersistentState(t, tx, calls, "entry", want)
						tr := tx.(fdb.Transaction)
						if calls == 1 {
							if err := tx.Options().SetSizeLimit(32); err != nil {
								return nil, err
							}
							override := nonpersistentDefaultsState{sizeLimit: 32, readSystemKeys: true, snapshotDisable: 1, bypassUnreadable: true, causalReadRisky: true}
							if defaultsOn {
								if err := tx.Options().SetSnapshotRywEnable(); err != nil {
									return nil, err
								}
								override.snapshotDisable = 0
							} else {
								for _, err := range []error{tx.Options().SetSnapshotRywDisable(), tx.Options().SetBypassUnreadable(), tx.Options().SetCausalReadRisky()} {
									if err != nil {
										return nil, err
									}
								}
								err := tx.Options().SetReadSystemKeys()
								if owner == "tenant" {
									var rejected *fdb.TenantOptionError
									if !errors.As(err, &rejected) || rejected.FDBCode() != 2007 {
										t.Errorf("tenant read_system_keys override=%v, want TenantOptionError 2007", err)
									}
									override.readSystemKeys = false
								} else if err != nil {
									return nil, err
								}
							}
							assertNonpersistentState(t, tx, calls, "once-only override", override)
							tr.Set(attemptKey, []byte("discarded"))
							return nil, fdb.Error{Code: 1020}
						}
						if calls == 2 {
							if got, err := tx.Get(attemptKey).Get(); err != nil || got != nil {
								t.Errorf("retry retained first-attempt mutation: value=%q err=%v", got, err)
							}
							tr.Set(snapshotKey, []byte("overlay"))
							got, err := tx.Snapshot().Get(snapshotKey).Get()
							var snapshotWant []byte
							if !defaultsOn {
								snapshotWant = []byte("overlay")
							}
							if err != nil || !bytes.Equal(got, snapshotWant) {
								t.Errorf("retried snapshot overlay: value=%q err=%v, want %q", got, err, snapshotWant)
							}
							operand := make([]byte, 14)
							tr.SetVersionstampedValue(stampKey, operand)
							got, err = tx.Get(stampKey).Get()
							if defaultsOn {
								if err != nil || !bytes.Equal(got, operand) {
									t.Errorf("retried bypass read: value=%x err=%v, want operand %x", got, err, operand)
								}
							} else if !isFDBCode(err, 1036) {
								t.Errorf("retried unreadable read=%v, want 1036 after once-only bypass drops", err)
							}
							return nil, fdb.Error{Code: 1020}
						}
						tr.Set(finalKey, payload)
						return nil, nil
					})
					wantCode := 0
					if write && defaultsOn {
						wantCode = 2101
					}
					if calls != 3 || wantCode == 0 && err != nil || wantCode != 0 && !isFDBCode(err, wantCode) {
						t.Errorf("calls=%d err=%v, want 3 callbacks and code %d", calls, err, wantCode)
					}
					_, err = run(ctx, func(tx fdb.ReadTransaction) (any, error) {
						for _, key := range []fdb.Key{attemptKey, snapshotKey, stampKey, finalKey} {
							got, err := tx.Get(key).Get()
							var persistedWant []byte
							if bytes.Equal(key, finalKey) && write && !defaultsOn {
								persistedWant = payload
							}
							if err != nil || !bytes.Equal(got, persistedWant) {
								t.Errorf("fresh persisted read %q: value=%x err=%v, want %x", key, got, err, persistedWant)
							}
						}
						return nil, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					if write {
						if _, err := run(ctx, func(tx fdb.ReadTransaction) (any, error) {
							tx.(fdb.WritableTransaction).Clear(finalKey)
							return nil, nil
						}); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}
	}
}
