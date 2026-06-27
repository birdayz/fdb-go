package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// TestFDB_DatabaseLocked_ReadPathEnforcement pins the locked-database read
// check: a locked database refuses reads from non-lock-aware transactions,
// exactly as C++ does client-side (NativeAPI.actor.cpp:7425-7426). Post-RFC-104
// the GRV cache is opt-in (default off), so every DEFAULT transaction here
// fresh-fetches its read version and reaches the locked check — the enforcement
// no longer rides the cache (the RFC-096 lastLocked mechanism is removed). Uses
// a DEDICATED container — a database lock is global, not key-prefix-scoped, and
// would break every parallel test on the shared TestMain container.
func TestFDB_DatabaseLocked_ReadPathEnforcement(t *testing.T) {
	t.Parallel()

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer setupCancel()
	container, connectCF, err := startFDBContainer(setupCtx)
	if err != nil {
		t.Fatalf("dedicated FDB container: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		container.Terminate(cleanupCtx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db, err := OpenDatabaseFromConfig(ctx, connectCF, WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabaseFromConfig: %v", err)
	}
	defer db.Close()

	key := []byte(t.Name() + "_key")
	databaseLockedKey := []byte("\xff/dbLocked") // SystemData.cpp:1383

	// Seed a key pre-lock.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Lock the database exactly as C++ ManagementAPI lockDatabase does
	// (ManagementAPI.actor.cpp:2471-2489): ACCESS_SYSTEM_KEYS + LOCK_AWARE,
	// SetVersionstampedValue of "0123456789"(10B, replaced by the commit
	// versionstamp at offset 0) + 16-byte lock UID, versionstamp offset
	// suffix 0, plus a write-conflict range over the normal keyspace.
	lockTx := db.CreateTransaction()
	lockTx.SetAccessSystemKeys()
	lockTx.SetLockAware(true)
	lockUID := []byte{
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00,
	}
	lockValue := append([]byte("0123456789"), lockUID...)
	lockValue = append(lockValue, 0x00, 0x00, 0x00, 0x00) // versionstamp offset 0 (LE)
	lockTx.Atomic(MutSetVersionstampedValue, databaseLockedKey, lockValue)
	lockTx.addWriteConflict([]byte(""), []byte("\xff"))
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatalf("lock database: %v", err)
	}

	// All enforcement arms run on a FRESH client handle: the first handle's
	// GRV cache may legitimately hold a fresh pre-lock version (C++ accepts
	// the same staleness for its cache), which would make the arms
	// timing-dependent.
	db2, err := OpenDatabaseFromConfig(ctx, connectCF, WithAPIVersion(730))
	if err != nil {
		t.Fatalf("OpenDatabaseFromConfig (fresh handle): %v", err)
	}
	defer db2.Close()

	// Arm 1: plain RAW transaction (no Run retry loop — 1038 is retryable
	// and a run loop would spin its budget; assert the FIRST error). The
	// fresh handle's empty cache forces a real GRV fetch.
	plain := db2.CreateTransaction()
	if _, err := plain.Get(ctx, key); err == nil {
		t.Fatal("plain read on a LOCKED database succeeded, want database_locked (1038)")
	} else {
		assertFDBErrorCode(t, err, ErrDatabaseLocked)
	}

	// Arm 2: a second raw (default) transaction immediately after. Default
	// transactions do NOT consult the GRV cache (USE_GRV_CACHE is off by
	// default — RFC-104), so this fresh-fetches its own GRV and gets the locked
	// flag → 1038. A warm cache cannot let a default transaction fail-open; only
	// an opt-in (SetUseGrvCache) transaction reads the cache, and that path
	// fail-opens by C++ contract (not exercised here).
	plain2 := db2.CreateTransaction()
	if _, err := plain2.Get(ctx, key); err == nil {
		t.Fatal("second plain read on a LOCKED database succeeded, want database_locked (1038)")
	} else {
		assertFDBErrorCode(t, err, ErrDatabaseLocked)
	}

	// Arm 2b: the SAME handle that committed the lock transaction must also
	// surface 1038 for plain reads. Its commit warmed the shared cache (version
	// + freshness, RFC-104), but a default read never consults the cache, so it
	// fresh-fetches and hits the locked flag. (Bounded poll kept for robustness;
	// convergence is immediate now that default reads always fresh-fetch.)
	armDeadline := time.Now().Add(10 * time.Second)
	for {
		sameHandle := db.CreateTransaction()
		if _, err := sameHandle.Get(ctx, key); err != nil {
			assertFDBErrorCode(t, err, ErrDatabaseLocked)
			break
		}
		if time.Now().After(armDeadline) {
			t.Fatal("plain reads on the lock-committing handle still succeeding 10s after lock")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Arm 3: LOCK_AWARE reads succeed (C++ options.lockAware exemption).
	lockAware := db2.CreateTransaction()
	lockAware.SetLockAware(true)
	if v, err := lockAware.Get(ctx, key); err != nil {
		t.Fatalf("LOCK_AWARE read on locked database: %v", err)
	} else if string(v) != "v" {
		t.Fatalf("LOCK_AWARE read = %q, want %q", v, "v")
	}

	// Arm 4: READ_LOCK_AWARE reads succeed (also sets C++ options.lockAware,
	// NativeAPI.actor.cpp:7083-7091).
	readLockAware := db2.CreateTransaction()
	readLockAware.SetReadLockAware(true)
	if v, err := readLockAware.Get(ctx, key); err != nil {
		t.Fatalf("READ_LOCK_AWARE read on locked database: %v", err)
	} else if string(v) != "v" {
		t.Fatalf("READ_LOCK_AWARE read = %q, want %q", v, "v")
	}

	// Unlock (lock-aware clear, as C++ unlockDatabase does).
	unlockTx := db2.CreateTransaction()
	unlockTx.SetAccessSystemKeys()
	unlockTx.SetLockAware(true)
	unlockTx.Clear(databaseLockedKey)
	if err := unlockTx.Commit(ctx); err != nil {
		t.Fatalf("unlock database: %v", err)
	}

	// Arm 5: plain reads succeed again. POLL: the unlock commit must become
	// visible to a fresh GRV before reads stop seeing the locked flag. Default
	// reads fresh-fetch (no cache — RFC-104), so this converges as soon as the
	// unlock is visible, not on any cache-refresh cadence.
	deadline := time.Now().Add(30 * time.Second)
	for {
		postUnlock := db2.CreateTransaction()
		v, err := postUnlock.Get(ctx, key)
		if err == nil {
			if string(v) != "v" {
				t.Fatalf("post-unlock read = %q, want %q", v, "v")
			}
			break
		}
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) || fdbErr.Code != ErrDatabaseLocked {
			t.Fatalf("post-unlock read: unexpected error %v (want eventual success or transient 1038)", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("plain reads still failing with database_locked 30s after unlock")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
