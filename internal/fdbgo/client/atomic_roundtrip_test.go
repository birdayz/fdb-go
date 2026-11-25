package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// TestAtomicAdd_RoundTrip verifies that our client-side doAdd matches the
// server-side ATOMIC_ADD by writing via atomic, reading back, and comparing
// with our local computation. This catches any divergence between our
// Atomic.h port and FDB's server-side implementation.
func TestAtomicAdd_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed with initial value = 100.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		var v [8]byte
		binary.LittleEndian.PutUint64(v[:], 100)
		tx.Set(key, v[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Atomically ADD 42.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		var param [8]byte
		binary.LittleEndian.PutUint64(param[:], 42)
		tx.Atomic(MutAddValue, key, param[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("atomic ADD: %v", err)
	}

	// Read back and verify.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	val := result.([]byte)
	got := binary.LittleEndian.Uint64(val)

	// Local computation should match.
	base := le64(100)
	param := le64(42)
	localResult, _ := applyAtomic(MutAddValue, base, param)
	localVal := binary.LittleEndian.Uint64(localResult)

	if got != 142 {
		t.Fatalf("server: expected 142, got %d", got)
	}
	if localVal != 142 {
		t.Fatalf("local: expected 142, got %d", localVal)
	}
	if got != localVal {
		t.Fatalf("divergence! server=%d, local=%d", got, localVal)
	}
}

// TestAtomicByteMax_RoundTrip verifies BYTE_MAX server behavior matches local.
func TestAtomicByteMax_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed with "apple".
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("apple"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Atomic BYTE_MAX with "banana" — "banana" > "apple" lexicographically.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutByteMax, key, []byte("banana"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("atomic BYTE_MAX: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	serverVal := string(result.([]byte))

	localResult, _ := applyAtomic(MutByteMax, []byte("apple"), []byte("banana"))
	localVal := string(localResult)

	if serverVal != "banana" {
		t.Fatalf("server: expected 'banana', got %q", serverVal)
	}
	if localVal != "banana" {
		t.Fatalf("local: expected 'banana', got %q", localVal)
	}
	if serverVal != localVal {
		t.Fatalf("divergence! server=%q, local=%q", serverVal, localVal)
	}
}

// TestAtomicCompareAndClear_RoundTrip verifies COMPARE_AND_CLEAR.
func TestAtomicCompareAndClear_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed with "hello".
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("hello"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// COMPARE_AND_CLEAR with "hello" — should clear the key.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutCompareAndClear, key, []byte("hello"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("atomic CAS: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	val := result.([]byte)
	if len(val) != 0 {
		t.Fatalf("server: expected cleared key (empty), got %q", val)
	}

	// Verify local agrees.
	_, cleared := applyAtomic(MutCompareAndClear, []byte("hello"), []byte("hello"))
	if !cleared {
		t.Fatal("local: expected cleared=true")
	}
}

// TestAtomicAdd_Overflow_RoundTrip verifies uint64 overflow behavior matches.
func TestAtomicAdd_Overflow_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed with max uint64 - 1.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, le64(^uint64(1))) // 0xFFFFFFFFFFFFFFFE
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// ADD 3 — should wrap around to 1.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutAddValue, key, le64(3))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("atomic ADD overflow: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	serverVal := binary.LittleEndian.Uint64(result.([]byte))

	localResult, _ := applyAtomic(MutAddValue, le64(^uint64(1)), le64(3))
	localVal := binary.LittleEndian.Uint64(localResult)

	expected := uint64(1) // 0xFFFFFFFFFFFFFFFE + 3 = 0x10000000000000001, truncated to 1
	if serverVal != expected {
		t.Fatalf("server: expected %d, got %d", expected, serverVal)
	}
	if localVal != expected {
		t.Fatalf("local: expected %d, got %d", expected, localVal)
	}

	// Verify local matches server (the critical invariant).
	if !bytes.Equal(result.([]byte), localResult) {
		t.Fatalf("divergence! server=%x, local=%x", result.([]byte), localResult)
	}
}
