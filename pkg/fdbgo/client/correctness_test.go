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

func TestGetKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write keys: gk_a, gk_b, gk_c, gk_d, gk_e
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		for _, s := range []string{"a", "b", "c", "d", "e"} {
			tx.Set([]byte("gk_"+s), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// firstGreaterOrEqual("gk_c") → should return "gk_c" (exact match)
	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.GetKey(ctx, []byte("gk_c"), false, 1) // orEqual=false, offset=1 = firstGreaterOrEqual
	})
	if err != nil {
		t.Fatalf("firstGreaterOrEqual: %v", err)
	}
	if string(result.([]byte)) != "gk_c" {
		t.Errorf("firstGreaterOrEqual(gk_c): got %q, want %q", result, "gk_c")
	}

	// firstGreaterThan("gk_c") → should return "gk_d"
	result, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.GetKey(ctx, []byte("gk_c"), true, 1) // orEqual=true, offset=1 = firstGreaterThan
	})
	if err != nil {
		t.Fatalf("firstGreaterThan: %v", err)
	}
	if string(result.([]byte)) != "gk_d" {
		t.Errorf("firstGreaterThan(gk_c): got %q, want %q", result, "gk_d")
	}

	// lastLessOrEqual("gk_c") → should return "gk_c"
	result, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.GetKey(ctx, []byte("gk_c"), true, 0) // orEqual=true, offset=0 = lastLessOrEqual
	})
	if err != nil {
		t.Fatalf("lastLessOrEqual: %v", err)
	}
	if string(result.([]byte)) != "gk_c" {
		t.Errorf("lastLessOrEqual(gk_c): got %q, want %q", result, "gk_c")
	}

	// lastLessThan("gk_c") → should return "gk_b"
	result, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.GetKey(ctx, []byte("gk_c"), false, 0) // orEqual=false, offset=0 = lastLessThan
	})
	if err != nil {
		t.Fatalf("lastLessThan: %v", err)
	}
	if string(result.([]byte)) != "gk_b" {
		t.Errorf("lastLessThan(gk_c): got %q, want %q", result, "gk_b")
	}
}

func TestSnapshotRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Seed a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("snap_key"), []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1: snapshot read + write (should NOT conflict)
	// tx2: regular write to same key, committed between tx1's read and commit
	//
	// With regular read: tx1 would conflict (read conflict range includes snap_key).
	// With snapshot read: tx1 should succeed (no read conflict range).

	tx1 := db.CreateTransaction()
	rv, _ := db.grvBatcher.GetReadVersion(ctx)
	tx1.SetReadVersion(rv)

	// Snapshot read — no conflict range added.
	val, err := tx1.Snapshot().Get(ctx, []byte("snap_key"))
	if err != nil {
		t.Fatalf("snapshot Get: %v", err)
	}
	if string(val) != "v0" {
		t.Fatalf("snapshot Get: got %q, want %q", val, "v0")
	}

	// tx2 writes the same key and commits.
	_, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("snap_key"), []byte("v1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx2 commit: %v", err)
	}

	// tx1 writes and commits — should succeed because snapshot read
	// didn't add a read conflict range.
	tx1.Set([]byte("snap_key"), []byte("v_from_tx1"))
	err = tx1.Commit(ctx)
	if err != nil {
		t.Fatalf("tx1 should NOT conflict after snapshot read, got: %v", err)
	}

	// Verify: now do a regular read that WOULD conflict.
	tx3 := db.CreateTransaction()
	rv3, _ := db.grvBatcher.GetReadVersion(ctx)
	tx3.SetReadVersion(rv3)

	// Regular read — adds conflict range.
	_, _ = tx3.Get(ctx, []byte("snap_key"))

	// Another transaction writes the same key.
	_, _ = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("snap_key"), []byte("v2"))
		return nil, nil
	})

	// tx3 should conflict.
	tx3.Set([]byte("snap_key"), []byte("v_from_tx3"))
	err = tx3.Commit(ctx)
	if err == nil {
		t.Fatal("tx3 SHOULD conflict after regular read")
	}
	t.Logf("tx3 conflict (expected): %v", err)
}

func TestExplicitConflictRanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("ecr_key"), []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// AddReadConflictKey: tx1 adds explicit read conflict (no actual read),
	// tx2 writes the same key. tx1 should conflict on commit.
	tx1 := db.CreateTransaction()
	rv, _ := db.grvBatcher.GetReadVersion(ctx)
	tx1.SetReadVersion(rv)
	tx1.AddReadConflictKey([]byte("ecr_key"))
	tx1.Set([]byte("ecr_other"), []byte("unrelated"))

	// tx2 writes the conflicting key.
	_, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("ecr_key"), []byte("v1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx2: %v", err)
	}

	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("tx1 should conflict due to AddReadConflictKey")
	}
	t.Logf("tx1 conflict (expected): %v", err)

	// AddWriteConflictKey: tx3 adds explicit write conflict on a key
	// that tx4 also writes. tx4 reads it first, so tx4 should conflict.
	tx3 := db.CreateTransaction()
	rv3, _ := db.grvBatcher.GetReadVersion(ctx)
	tx3.SetReadVersion(rv3)

	tx4 := db.CreateTransaction()
	tx4.SetReadVersion(rv3)
	_, _ = tx4.Get(ctx, []byte("ecr_wc")) // adds read conflict
	tx4.Set([]byte("ecr_wc"), []byte("from_tx4"))

	// tx3 only has a write conflict (no mutation on ecr_wc, but conflict range covers it).
	tx3.AddWriteConflictKey([]byte("ecr_wc"))
	tx3.Set([]byte("ecr_dummy"), []byte("x")) // need a mutation to commit
	err = tx3.Commit(ctx)
	if err != nil {
		t.Fatalf("tx3 should succeed: %v", err)
	}

	// tx4 should now conflict — tx3's write conflict overlaps tx4's read conflict.
	err = tx4.Commit(ctx)
	if err == nil {
		t.Fatal("tx4 should conflict due to tx3's AddWriteConflictKey")
	}
	t.Logf("tx4 conflict (expected): %v", err)
}

func TestCancel(t *testing.T) {
	t.Parallel()

	tx := &Transaction{state: txStateActive}
	tx.Set([]byte("key"), []byte("val"))

	tx.Cancel()

	// Get should fail.
	_, err := tx.Get(context.Background(), []byte("key"))
	if err == nil {
		t.Error("Get after Cancel should fail")
	}

	// Commit should fail.
	err = tx.Commit(context.Background())
	if err == nil {
		t.Error("Commit after Cancel should fail")
	}

	// Set should still work (buffered locally, no state check).
	// But the transaction is useless — Commit will fail.
	tx.Set([]byte("key2"), []byte("val2"))
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
