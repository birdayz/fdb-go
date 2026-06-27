package client

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestGetKey_AllKeysEndSelector verifies the allKeysEnd short-circuit in getKey.
// When the selector key is \xFF\xFF (allKeysEnd) and offset > 0, the result
// is allKeysEnd immediately without contacting the server. This matches
// C++ NativeAPI.actor.cpp's short-circuit.
func TestGetKey_AllKeysEndSelector(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.SetReadSystemKeys()
		// firstGreaterOrEqual(\xFF\xFF) = offset=1 with allKeysEnd key.
		// Should return \xFF\xFF immediately.
		key, err := tx.GetKey(ctx, []byte{0xFF, 0xFF}, false, 1)
		if err != nil {
			return nil, err
		}
		if len(key) != 2 || key[0] != 0xFF || key[1] != 0xFF {
			t.Errorf("expected allKeysEnd, got %x", key)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestGetKey_EmptyKeyOffset verifies the empty-key short-circuit in getKey.
// When the selector key is empty and offset <= 0, returns empty key.
func TestGetKey_EmptyKeyOffset(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// lastLessThan("") = key="" orEqual=false offset=0.
		// C++ short-circuit: len==0 && offset<=0 → return "".
		key, err := tx.GetKey(ctx, []byte{}, false, 0)
		if err != nil {
			return nil, err
		}
		if len(key) != 0 {
			t.Errorf("expected empty key, got %x (len %d)", key, len(key))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestGetRange_EmptyResult verifies that getRange returns empty slice
// and more=false for a range with no keys.
func TestGetRange_EmptyResult(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Use a prefix that no test writes to.
	prefix := t.Name() + "_empty_"

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)
		kvs, more, err := tx.GetRange(ctx, begin, end, 100)
		if err != nil {
			return nil, err
		}
		if len(kvs) != 0 {
			t.Errorf("expected 0 results, got %d", len(kvs))
		}
		if more {
			t.Error("expected more=false for empty range")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// TestGetRange_LargeLimit verifies that the wire limit is clamped to MaxInt32
// and doesn't overflow. We pass a very large limit and verify the scan works.
func TestGetRange_LargeLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed 3 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"A"), []byte("a"))
		tx.Set([]byte(prefix+"B"), []byte("b"))
		tx.Set([]byte(prefix+"C"), []byte("c"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Scan with unlimited (0 = no limit in our API).
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(kvs))
	}
}

// TestGetRange_Reverse verifies reverse range scanning at the transaction level.
func TestGetRange_Reverse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"X"), []byte("x"))
		tx.Set([]byte(prefix+"Y"), []byte("y"))
		tx.Set([]byte(prefix+"Z"), []byte("z"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)
		kvs, _, err := tx.GetRangeReverse(ctx, begin, end, 100)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRangeReverse: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(kvs))
	}
	// Reverse order: Z, Y, X.
	if string(kvs[0].Value) != "z" || string(kvs[1].Value) != "y" || string(kvs[2].Value) != "x" {
		t.Errorf("expected reverse order [z, y, x], got [%s, %s, %s]",
			kvs[0].Value, kvs[1].Value, kvs[2].Value)
	}
}

// TestGetRange_WithLimit verifies that the limit parameter is respected
// and more=true is returned when there are remaining keys.
func TestGetRange_WithLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"

	// Seed 5 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 5; i++ {
			tx.Set([]byte(prefix+string(rune('A'+i))), []byte{byte(i)})
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Scan with limit 2.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)
		kvs, more, err := tx.GetRange(ctx, begin, end, 2)
		if err != nil {
			return nil, err
		}
		return []any{kvs, more}, nil
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	parts := result.([]any)
	kvs := parts[0].([]KeyValue)
	more := parts[1].(bool)
	if len(kvs) != 2 {
		t.Fatalf("expected 2 keys with limit 2, got %d", len(kvs))
	}
	if !more {
		t.Error("expected more=true with limit 2 and 5 keys")
	}
}

// TestGetValue_NonExistentKey verifies that Get returns nil or empty for a key
// that doesn't exist (not an error).
func TestGetValue_NonExistentKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(t.Name()+"_nonexistent"))
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// FDB returns nil or empty bytes for nonexistent keys — both are acceptable.
	if result != nil {
		val := result.([]byte)
		if len(val) != 0 {
			t.Fatalf("expected nil or empty for nonexistent key, got %v", val)
		}
	}
}

// TestWatch_ValueCapturedSyncFiresAfterModify deterministically pins the watch
// value-capture fix (the CI flake root cause). A watch fires when the storage
// server sees a value different from the one it was registered against. The fix
// splits Watch into a SYNCHRONOUS WatchSetup (pin the value at the read version)
// and an async WatchPoll. This test exercises the split directly: capture the
// value BEFORE the modify, then poll — the watch must fire. (Before the fix,
// tr.Watch read the value in a detached goroutine that could run AFTER the
// modify, capturing the already-current value so the watch never fired — a
// silent 10s timeout. By controlling the order here, the proof is deterministic,
// not race-dependent.)
func TestWatch_ValueCapturedSyncFiresAfterModify(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	key := []byte(t.Name() + "_key")

	// Seed "initial".
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("initial"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Capture the watched value AND read version SYNCHRONOUSLY (what tr.Watch now
	// does before returning the future) — captures "initial" at this version.
	var captured []byte
	var capturedRV int64
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		v, rv, _, err := tx.WatchSetup(ctx, key)
		captured, capturedRV = v, rv
		return nil, err
	}); err != nil {
		t.Fatalf("WatchSetup: %v", err)
	}

	// Modify strictly AFTER the value was captured.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatalf("modify: %v", err)
	}

	// Poll: the current value ("changed") differs from the captured value
	// ("initial"), so the watch must fire promptly.
	done := make(chan error, 1)
	go func() {
		_, e := db.Transact(ctx, func(tx *Transaction) (any, error) {
			return nil, tx.WatchPoll(ctx, key, captured, capturedRV, types.SpanContext{})
		})
		done <- e
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WatchPoll(stale captured value) returned %v, want it to fire", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WatchPoll did not fire for a stale captured value within 10s — value-capture regression")
	}
}

// TestWatch_ReadVersionSurvivesReset exercises the read-version half of the watch
// fix (codex's P1): WatchPoll must register the watch at the read version captured
// by WatchSetup, NOT re-read tx.readVersion at poll time — which the common
// `w := tr.Watch(k)` inside Database.Transact pattern clears to 0 (postCommitReset)
// before the async poll runs. This drives exactly that state (reset between setup
// and poll) and asserts the watch still fires.
//
// NOTE — not a fail-without-fix sentinel: the storage server benignly tolerates a
// stale (0) version for the value-diff fire in this scenario, so the watch fires
// even with the old code. The test guards the reset-path (that WatchPoll works on
// a post-commit-reset transaction); the fix's value is correctness — a detached
// goroutine must not read mutable transaction state (readVersion) that commit may
// have cleared — which is the same rationale as the synchronous value capture.
func TestWatch_ReadVersionSurvivesReset(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	key := []byte(t.Name() + "_key")

	// Seed "initial".
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("initial"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Watch setup on a manual transaction; capture value + read version.
	tx := db.CreateTransaction()
	value, rv, _, err := tx.WatchSetup(ctx, key)
	if err != nil {
		t.Fatalf("WatchSetup: %v", err)
	}
	if rv == 0 {
		t.Fatal("WatchSetup returned read version 0")
	}

	// Force the post-commit state of the tr.Watch-in-Transact pattern: the tx is
	// reset and tx.readVersion is cleared to 0. The fix must not depend on it.
	tx.postCommitReset()

	// Modify the key after setup.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatalf("modify: %v", err)
	}

	// Poll with the captured read version — must fire even though tx.readVersion
	// is now 0 (reset). Before the fix, sendWatch read tx.readVersion = 0.
	done := make(chan error, 1)
	go func() { done <- tx.WatchPoll(ctx, key, value, rv, types.SpanContext{}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WatchPoll after reset returned %v, want it to fire (read version must be the captured one)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("WatchPoll did not fire after reset — read version not preserved (codex P1 regression)")
	}
}
