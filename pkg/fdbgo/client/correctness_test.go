package client

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// Correctness tests run against a real FDB 7.3.75 testcontainer.
// Each test uses the shared openTestDB helper which starts a container,
// configures the cluster, and connects.

func TestClear(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("clear_me"), []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify it exists.
	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.Get(ctx, []byte("clear_me"))
	})
	if err != nil {
		t.Fatalf("Get before clear: %v", err)
	}
	if string(result.([]byte)) != "exists" {
		t.Fatalf("before clear: got %q, want %q", result, "exists")
	}

	// Clear it.
	_, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Clear([]byte("clear_me"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}

	// Verify it's gone.
	result, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.Get(ctx, []byte("clear_me"))
	})
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if result.([]byte) != nil {
		t.Fatalf("after clear: got %q, want nil", result)
	}
}

func TestClearRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write 5 keys: cr_a, cr_b, cr_c, cr_d, cr_e
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		for _, suffix := range []string{"a", "b", "c", "d", "e"} {
			tx.Set([]byte("cr_"+suffix), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear range [cr_b, cr_d) — should delete cr_b, cr_c.
	_, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.ClearRange([]byte("cr_b"), []byte("cr_d"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ClearRange: %v", err)
	}

	// Verify: cr_a and cr_d and cr_e survive.
	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		kvs, _, err := tx.GetRange(ctx, []byte("cr_"), []byte("cr_~"), 100)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]KeyValue)
	got := make([]string, len(kvs))
	for i, kv := range kvs {
		got[i] = string(kv.Key)
	}
	want := []string{"cr_a", "cr_d", "cr_e"}
	if len(got) != len(want) {
		t.Fatalf("keys: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAtomicAdd(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte("counter")

	// Initialize counter to 10.
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], 10)
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set(key, buf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Atomic ADD +5.
	binary.LittleEndian.PutUint64(buf[:], 5)
	_, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Atomic(MutAddValue, key, buf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ADD: %v", err)
	}

	// Read back — should be 15.
	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	val := binary.LittleEndian.Uint64(result.([]byte))
	if val != 15 {
		t.Fatalf("counter: got %d, want 15", val)
	}
}

func TestGetRangeWithLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write 10 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		for i := 0; i < 10; i++ {
			tx.Set([]byte("lim_"+string(rune('a'+i))), []byte{byte(i)})
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Read with limit 3 — should get 3 keys, more=true.
	type rangeResult struct {
		kvs  []KeyValue
		more bool
	}
	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		kvs, more, err := tx.GetRange(ctx, []byte("lim_"), []byte("lim_~"), 3)
		return rangeResult{kvs, more}, err
	})
	if err != nil {
		t.Fatalf("GetRange(3): %v", err)
	}
	rr := result.(rangeResult)
	kvs := rr.kvs
	more := rr.more
	if len(kvs) != 3 {
		t.Errorf("count: got %d, want 3", len(kvs))
	}
	if !more {
		t.Error("more: got false, want true")
	}
	if len(kvs) >= 3 {
		if string(kvs[0].Key) != "lim_a" || string(kvs[2].Key) != "lim_c" {
			t.Errorf("keys: first=%q last=%q", kvs[0].Key, kvs[2].Key)
		}
	}
}

func TestMultiKeyTransaction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write two keys in one transaction, read both back in another.
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("mk_x"), []byte("100"))
		tx.Set([]byte("mk_y"), []byte("200"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		x, err := tx.Get(ctx, []byte("mk_x"))
		if err != nil {
			return nil, err
		}
		y, err := tx.Get(ctx, []byte("mk_y"))
		if err != nil {
			return nil, err
		}
		return []string{string(x), string(y)}, nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	vals := result.([]string)
	if vals[0] != "100" || vals[1] != "200" {
		t.Fatalf("values: got %v, want [100 200]", vals)
	}
}

func TestGetNonExistentKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.Get(ctx, []byte("does_not_exist_ever"))
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if result.([]byte) != nil {
		t.Fatalf("expected nil for non-existent key, got %q", result)
	}
}

func TestEmptyRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		kvs, more, err := tx.GetRange(ctx, []byte("empty_range_prefix_"), []byte("empty_range_prefix_~"), 100)
		if err != nil {
			return nil, err
		}
		if len(kvs) != 0 {
			t.Errorf("expected 0 keys, got %d", len(kvs))
		}
		if more {
			t.Error("expected more=false for empty range")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
}
