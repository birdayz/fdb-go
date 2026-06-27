package client

// Tests for FDB hard limits: key size, value size, transaction size.
// These verify our client correctly handles boundary conditions that
// FDB enforces server-side.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// TestLargeValue_NearLimit writes a value near the 100KB limit.
// FDB allows up to 100,000 bytes. Values exactly at the limit should succeed.
func TestLargeValue_NearLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("limit_test_large_value")
	value := bytes.Repeat([]byte("x"), 99_999) // just under 100KB

	// Write near-limit value.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, value)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write 99999 bytes: %v", err)
	}

	// Read back and verify.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(result.([]byte), value) {
		t.Fatalf("value mismatch: got %d bytes, want %d", len(result.([]byte)), len(value))
	}

	// Clean up.
	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return nil, nil
	})
}

// TestLargeKey_NearLimit writes a key near the ~10KB limit.
// FDB allows keys up to 10,000 bytes.
func TestLargeKey_NearLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte(strings.Repeat("k", 9_999)) // just under 10KB
	value := []byte("v")

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, value)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write with 9999-byte key: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(result.([]byte), value) {
		t.Fatalf("value mismatch: got %q, want %q", result, value)
	}

	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return nil, nil
	})
}

// TestEmptyKeyValue tests writing and reading empty key/value combinations.
func TestEmptyKeyValue_Limits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Empty value is valid.
	key := []byte("limit_test_empty_value")
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte{})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write empty value: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read empty value: %v", err)
	}
	val := result.([]byte)
	if val == nil || len(val) != 0 {
		t.Fatalf("expected empty value, got %v", val)
	}

	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return nil, nil
	})
}

// TestTransactionSizeLimit verifies that exceeding a client-side transaction
// size limit produces error 2101 (transaction_too_large) on commit.
// Uses SetSizeLimit to configure the limit (FDB's default is 10MB but our
// client only checks when sizeLimit > 0).
func TestTransactionSizeLimit_Commit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Set a low size limit, write data exceeding it, verify commit fails.
	db.SetTransactionSizeLimit(5000)    // 5KB limit
	defer db.SetTransactionSizeLimit(0) // restore

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Write ~10KB of data to exceed the 5KB limit.
		for i := 0; i < 10; i++ {
			key := []byte(strings.Repeat("x", 20) + string(rune(i)))
			val := bytes.Repeat([]byte("d"), 1000)
			tx.Set(key, val)
		}
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected transaction_too_large error, got nil")
	}

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != 2101 {
		t.Errorf("error code: got %d, want 2101 (transaction_too_large)", fdbErr.Code)
	}
}

// TestGetNonExistentKey verifies that Get on a non-existent key returns nil.
func TestGetNonExistentKey_Limits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("definitely_does_not_exist_12345"))
	})
	if err != nil {
		t.Fatalf("get non-existent: %v", err)
	}
	if result.([]byte) != nil {
		t.Fatalf("expected nil for non-existent key, got %v", result)
	}
}

// TestGetRangeEmpty verifies that GetRange on an empty range returns empty.
func TestGetRangeEmpty_Limits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, []byte("empty_range_test_start"), []byte("empty_range_test_end"), 0)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("getrange: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 0 {
		t.Fatalf("expected empty range, got %d keys", len(kvs))
	}
}
