package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// Correctness tests run against a real FDB 7.3.75 testcontainer.
// Each test uses the shared openTestDB helper which starts a container,
// configures the cluster, and connects.

func TestClear(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_clear_me")

	// Write a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify it exists.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Get before clear: %v", err)
	}
	if string(result.([]byte)) != "exists" {
		t.Fatalf("before clear: got %q, want %q", result, "exists")
	}

	// Clear it.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}

	// Verify it's gone.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	// Write 5 keys: pfx+a through pfx+e
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, suffix := range []string{"a", "b", "c", "d", "e"} {
			tx.Set([]byte(pfx+suffix), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear range [pfx+b, pfx+d) — should delete b, c.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte(pfx+"b"), []byte(pfx+"d"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ClearRange: %v", err)
	}

	// Verify: a and d and e survive.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, []byte(pfx), []byte(pfx+"~"), 100)
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
	want := []string{pfx + "a", pfx + "d", pfx + "e"}
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_counter")

	// Initialize counter to 10.
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], 10)
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, buf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Atomic ADD +5.
	binary.LittleEndian.PutUint64(buf[:], 5)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutAddValue, key, buf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ADD: %v", err)
	}

	// Read back — should be 15.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	// Write 10 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set([]byte(pfx+string(rune('a'+i))), []byte{byte(i)})
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
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte(pfx), []byte(pfx+"~"), 3)
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
		if string(kvs[0].Key) != pfx+"a" || string(kvs[2].Key) != pfx+"c" {
			t.Errorf("keys: first=%q last=%q", kvs[0].Key, kvs[2].Key)
		}
	}
}

func TestMultiKeyTransaction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"
	keyX := []byte(pfx + "x")
	keyY := []byte(pfx + "y")

	// Write two keys in one transaction, read both back in another.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(keyX, []byte("100"))
		tx.Set(keyY, []byte("200"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		x, err := tx.Get(ctx, keyX)
		if err != nil {
			return nil, err
		}
		y, err := tx.Get(ctx, keyY)
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_does_not_exist_ever")
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	// Write keys: pfx+a through pfx+e
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, s := range []string{"a", "b", "c", "d", "e"} {
			tx.Set([]byte(pfx+s), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// firstGreaterOrEqual(pfx+"c") → should return pfx+"c" (exact match)
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(pfx+"c"), false, 1) // orEqual=false, offset=1 = firstGreaterOrEqual
	})
	if err != nil {
		t.Fatalf("firstGreaterOrEqual: %v", err)
	}
	if string(result.([]byte)) != pfx+"c" {
		t.Errorf("firstGreaterOrEqual(%sc): got %q, want %q", pfx, result, pfx+"c")
	}

	// firstGreaterThan(pfx+"c") → should return pfx+"d"
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(pfx+"c"), true, 1) // orEqual=true, offset=1 = firstGreaterThan
	})
	if err != nil {
		t.Fatalf("firstGreaterThan: %v", err)
	}
	if string(result.([]byte)) != pfx+"d" {
		t.Errorf("firstGreaterThan(%sc): got %q, want %q", pfx, result, pfx+"d")
	}

	// lastLessOrEqual(pfx+"c") → should return pfx+"c"
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(pfx+"c"), true, 0) // orEqual=true, offset=0 = lastLessOrEqual
	})
	if err != nil {
		t.Fatalf("lastLessOrEqual: %v", err)
	}
	if string(result.([]byte)) != pfx+"c" {
		t.Errorf("lastLessOrEqual(%sc): got %q, want %q", pfx, result, pfx+"c")
	}

	// lastLessThan(pfx+"c") → should return pfx+"b"
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(pfx+"c"), false, 0) // orEqual=false, offset=0 = lastLessThan
	})
	if err != nil {
		t.Fatalf("lastLessThan: %v", err)
	}
	if string(result.([]byte)) != pfx+"b" {
		t.Errorf("lastLessThan(%sc): got %q, want %q", pfx, result, pfx+"b")
	}
}

func TestSnapshotRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_snap_key")

	// Seed a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1: snapshot read + write (should NOT conflict)
	// tx2: regular write to same key, committed between tx1's read and commit
	//
	// With regular read: tx1 would conflict (read conflict range includes key).
	// With snapshot read: tx1 should succeed (no read conflict range).

	tx1 := db.CreateTransaction()
	rv, _, _ := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	tx1.SetReadVersion(rv)

	// Snapshot read — no conflict range added.
	val, err := tx1.Snapshot().Get(ctx, key)
	if err != nil {
		t.Fatalf("snapshot Get: %v", err)
	}
	if string(val) != "v0" {
		t.Fatalf("snapshot Get: got %q, want %q", val, "v0")
	}

	// tx2 writes the same key and commits.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v1"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx2 commit: %v", err)
	}

	// tx1 writes and commits — should succeed because snapshot read
	// didn't add a read conflict range.
	tx1.Set(key, []byte("v_from_tx1"))
	err = tx1.Commit(ctx)
	if err != nil {
		t.Fatalf("tx1 should NOT conflict after snapshot read, got: %v", err)
	}

	// Verify: now do a regular read that WOULD conflict.
	tx3 := db.CreateTransaction()
	rv3, _, _ := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	tx3.SetReadVersion(rv3)

	// Regular read — adds conflict range.
	_, _ = tx3.Get(ctx, key)

	// Another transaction writes the same key.
	_, _ = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v2"))
		return nil, nil
	})

	// tx3 should conflict.
	tx3.Set(key, []byte("v_from_tx3"))
	err = tx3.Commit(ctx)
	if err == nil {
		t.Fatal("tx3 SHOULD conflict after regular read")
	}
	t.Logf("tx3 conflict (expected): %v", err)
}

func TestExplicitConflictRanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"
	keyMain := []byte(pfx + "key")
	keyOther := []byte(pfx + "other")
	keyWC := []byte(pfx + "wc")
	keyDummy := []byte(pfx + "dummy")

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(keyMain, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// AddReadConflictKey: tx1 adds explicit read conflict (no actual read),
	// tx2 writes the same key. tx1 should conflict on commit.
	tx1 := db.CreateTransaction()
	rv, _, _ := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	tx1.SetReadVersion(rv)
	tx1.AddReadConflictKey(keyMain)
	tx1.Set(keyOther, []byte("unrelated"))

	// tx2 writes the conflicting key.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(keyMain, []byte("v1"))
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
	rv3, _, _ := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	tx3.SetReadVersion(rv3)

	tx4 := db.CreateTransaction()
	tx4.SetReadVersion(rv3)
	_, _ = tx4.Get(ctx, keyWC) // adds read conflict
	tx4.Set(keyWC, []byte("from_tx4"))

	// tx3 only has a write conflict (no mutation on keyWC, but conflict range covers it).
	tx3.AddWriteConflictKey(keyWC)
	tx3.Set(keyDummy, []byte("x")) // need a mutation to commit
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_cancel_key")

	// Write a key so there's something to read.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Create a real transaction, do a successful read, then cancel.
	tx := db.CreateTransaction()
	val, err := tx.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get before Cancel: %v", err)
	}
	if string(val) != "exists" {
		t.Fatalf("Get before Cancel: got %q, want %q", val, "exists")
	}

	tx.Cancel()

	// Get after Cancel should fail.
	_, err = tx.Get(ctx, key)
	if err == nil {
		t.Error("Get after Cancel should fail")
	}

	// Commit after Cancel should fail.
	tx.Set(key, []byte("modified"))
	err = tx.Commit(ctx)
	if err == nil {
		t.Error("Commit after Cancel should fail")
	}

	// Verify the key was NOT modified (cancel prevented commit).
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if string(result.([]byte)) != "exists" {
		t.Fatalf("key should be unchanged after cancelled tx, got %q", result)
	}
}

func TestReadOnlyCommitIntegration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"
	keyNonexistent := []byte(pfx + "nonexistent")
	keyReadonly := []byte(pfx + "key")

	// Transact with only reads, no writes — commit should be a no-op.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Read a non-existent key. No mutations.
		_, err := tx.Get(ctx, keyNonexistent)
		return nil, err
	})
	if err != nil {
		t.Fatalf("read-only Transact should succeed: %v", err)
	}

	// Seed a key, then read it in a read-only transaction.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(keyReadonly, []byte("val"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, keyReadonly)
	})
	if err != nil {
		t.Fatalf("read-only Get: %v", err)
	}
	if string(result.([]byte)) != "val" {
		t.Fatalf("got %q, want %q", result, "val")
	}
}

func TestAddReadConflictRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"a"), []byte("1"))
		tx.Set([]byte(pfx+"b"), []byte("2"))
		tx.Set([]byte(pfx+"c"), []byte("3"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1: add read conflict range [pfx+a, pfx+d) — covers a, b, c.
	tx1 := db.CreateTransaction()
	rv, _, _ := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	tx1.SetReadVersion(rv)
	tx1.AddReadConflictRange([]byte(pfx+"a"), []byte(pfx+"d"))
	tx1.Set([]byte(pfx+"unrelated"), []byte("x"))

	// tx2: write pfx+b (inside the conflict range).
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"b"), []byte("modified"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx2: %v", err)
	}

	// tx1 should conflict.
	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("tx1 should conflict — rcr_b was written inside its read conflict range")
	}
	t.Logf("tx1 conflict (expected): %v", err)
}

func TestAddWriteConflictRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	// tx1 reads a key in the range [pfx+a, pfx+d).
	tx1 := db.CreateTransaction()
	rv, _, _ := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	tx1.SetReadVersion(rv)
	_, _ = tx1.Get(ctx, []byte(pfx+"b")) // adds read conflict for pfx+b
	tx1.Set([]byte(pfx+"b"), []byte("from_tx1"))

	// tx2 adds a write conflict range covering [pfx+a, pfx+d).
	tx2 := db.CreateTransaction()
	tx2.SetReadVersion(rv)
	tx2.AddWriteConflictRange([]byte(pfx+"a"), []byte(pfx+"d"))
	tx2.Set([]byte(pfx+"other"), []byte("x")) // need a mutation to commit
	err := tx2.Commit(ctx)
	if err != nil {
		t.Fatalf("tx2 should succeed: %v", err)
	}

	// tx1 should conflict — tx2's write conflict range overlaps tx1's read conflict.
	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("tx1 should conflict — tx2's write conflict range overlaps tx1's read")
	}
	t.Logf("tx1 conflict (expected): %v", err)
}

func TestReadTransact(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"
	keyA := []byte(pfx + "a")
	keyB := []byte(pfx + "b")
	keyPhantom := []byte(pfx + "phantom")

	// Seed keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(keyA, []byte("1"))
		tx.Set(keyB, []byte("2"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// ReadTransact: read two keys, return their values.
	result, err := db.ReadTransact(ctx, func(tx *Transaction) (any, error) {
		a, err := tx.Get(ctx, keyA)
		if err != nil {
			return nil, err
		}
		b, err := tx.Get(ctx, keyB)
		if err != nil {
			return nil, err
		}
		return []string{string(a), string(b)}, nil
	})
	if err != nil {
		t.Fatalf("ReadTransact: %v", err)
	}
	vals := result.([]string)
	if vals[0] != "1" || vals[1] != "2" {
		t.Fatalf("got %v, want [1 2]", vals)
	}

	// ReadTransact does NOT commit — verify by writing inside it
	// and checking the write didn't persist.
	_, err = db.ReadTransact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(keyPhantom, []byte("should_not_persist"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ReadTransact with write: %v", err)
	}

	// Verify the write didn't persist.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, keyPhantom)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.([]byte) != nil {
		t.Fatalf("rt_phantom should not exist, got %q", result)
	}
}

func TestGetVersionstamp(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	// Commit a transaction and get the versionstamp.
	tx1 := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)
	tx1.Set([]byte(pfx+"key1"), []byte("val1"))
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 commit: %v", err)
	}

	vs1, err := tx1.GetVersionstamp()
	if err != nil {
		t.Fatalf("GetVersionstamp: %v", err)
	}
	if len(vs1) != 10 {
		t.Fatalf("versionstamp length: got %d, want 10", len(vs1))
	}

	// The version component (first 8 bytes BE) should match GetCommittedVersion.
	cv1, _ := tx1.GetCommittedVersion()
	vsVersion := int64(binary.BigEndian.Uint64(vs1[0:8]))
	if vsVersion != cv1 {
		t.Errorf("version mismatch: versionstamp=%d, committedVersion=%d", vsVersion, cv1)
	}
	t.Logf("tx1: version=%d, txnBatchId=%d, versionstamp=%x", cv1, binary.BigEndian.Uint16(vs1[8:10]), vs1)

	// Commit a second transaction — its versionstamp should be greater.
	tx2 := db.CreateTransaction()
	rv2, _, _ := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	tx2.SetReadVersion(rv2)
	tx2.Set([]byte(pfx+"key2"), []byte("val2"))
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("tx2 commit: %v", err)
	}

	vs2, _ := tx2.GetVersionstamp()
	t.Logf("tx2: versionstamp=%x", vs2)

	// vs2 should be strictly greater than vs1 (byte comparison, big-endian = ordered).
	if bytes.Compare(vs2, vs1) <= 0 {
		t.Errorf("versionstamp should be strictly increasing: vs1=%x vs2=%x", vs1, vs2)
	}

	// GetVersionstamp before commit should fail.
	tx3 := db.CreateTransaction()
	_, err = tx3.GetVersionstamp()
	if err == nil {
		t.Error("GetVersionstamp before commit should fail")
	}
}

func TestWatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_watch_test_key")

	// Set initial value.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("initial"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("initial set: %v", err)
	}

	// Start watching in a goroutine.
	watchDone := make(chan error, 1)
	go func() {
		_, werr := db.Transact(ctx, func(tx *Transaction) (any, error) {
			return nil, tx.Watch(ctx, key)
		})
		watchDone <- werr
	}()

	// Give the watch time to register, then change the key.
	time.Sleep(500 * time.Millisecond)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("changed"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("change set: %v", err)
	}

	// Watch should resolve.
	select {
	case err := <-watchDone:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("watch did not resolve within 30 seconds")
	}
}

func TestGetEstimatedRangeSizeBytes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	// Seed some data so the range is non-empty.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 100; i++ {
			k := []byte(fmt.Sprintf("%s%04d", pfx, i))
			tx.Set(k, bytes.Repeat([]byte("x"), 1000))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// GetEstimatedRangeSizeBytes should not error.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetEstimatedRangeSizeBytes(ctx, []byte(pfx), []byte(pfx+"~"))
	})
	if err != nil {
		t.Fatalf("GetEstimatedRangeSizeBytes: %v", err)
	}
	size := result.(int64)
	t.Logf("estimated range size: %d bytes", size)
	// The exact value is non-deterministic, but it should be non-negative.
	if size < 0 {
		t.Fatalf("expected non-negative size, got %d", size)
	}
}

func TestGetRangeSplitPoints(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	// Seed some data.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 100; i++ {
			k := []byte(fmt.Sprintf("%s%04d", pfx, i))
			tx.Set(k, bytes.Repeat([]byte("y"), 1000))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// ~100KB seeded at 50KB chunks ⇒ the server returns split points inside
	// the range. The byte sample is keyed on deterministic key hashes (the
	// sampled SET is fixed for fixed key names), but its propagation into the
	// storage server's metrics is ASYNC after commit — an immediate call can
	// legitimately see an empty sample. Poll until points appear. Do NOT
	// weaken this back to "empty is fine": that tolerance masked a parser
	// that decoded ZERO split points from EVERY reply, forever (splitPoints
	// is a FlatBuffers offset-vector, not an inline blob — see
	// parseSplitRangeReply); a broken parser never converges here.
	var points [][]byte
	deadline := time.Now().Add(60 * time.Second)
	for {
		result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			return tx.GetRangeSplitPoints(ctx, []byte(pfx), []byte(pfx+"~"), 50000)
		})
		if err != nil {
			t.Fatalf("GetRangeSplitPoints: %v", err)
		}
		points = result.([][]byte)
		if len(points) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("split points: %d", len(points))
	for i, p := range points {
		t.Logf("  split[%d]: %q", i, p)
	}
	if len(points) == 0 {
		t.Fatal("expected at least one split point for ~100KB at 50KB chunks (after byte-sample propagation), got none")
	}
	for i, p := range points {
		if !bytes.HasPrefix(p, []byte(pfx)) {
			t.Errorf("split[%d] = %q outside the seeded range", i, p)
		}
	}
}

func TestEmptyRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte(pfx), []byte(pfx+"~"), 100)
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

// TestGetAddressesForKey verifies that GetAddressesForKey returns at least
// one storage server address for an existing key.
func TestGetAddressesForKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_locality_test_key")

	// Write a key so the shard is populated.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Get addresses for the key.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetAddressesForKey(ctx, key)
	})
	if err != nil {
		t.Fatalf("GetAddressesForKey: %v", err)
	}
	addrs := result.([]string)
	if len(addrs) == 0 {
		t.Fatal("expected at least one address")
	}
	// Verify address format (host:port).
	for _, addr := range addrs {
		if !strings.Contains(addr, ":") {
			t.Errorf("address %q doesn't look like host:port", addr)
		}
	}
	t.Logf("addresses for key: %v", addrs)
}

func TestConcurrentTransactions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"
	const goroutines = 10
	const txPerGoroutine = 50
	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	start := time.Now()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < txPerGoroutine; i++ {
				key := []byte(fmt.Sprintf("%s%d_%d", pfx, id, i))
				val := []byte(fmt.Sprintf("value_%d_%d", id, i))
				_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
					tx.Set(key, val)
					return nil, nil
				})
				if err != nil {
					errorCount.Add(1)
					continue
				}
				// Read back in separate transaction
				result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
					return tx.Get(ctx, key)
				})
				if err != nil {
					errorCount.Add(1)
					continue
				}
				if !bytes.Equal(result.([]byte), val) {
					t.Errorf("goroutine %d tx %d: got %q, want %q", id, i, result, val)
					errorCount.Add(1)
					continue
				}
				successCount.Add(1)
			}
		}(g)
	}
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("concurrent: %d success, %d errors, %d total, %.0f tx/s",
		successCount.Load(), errorCount.Load(),
		goroutines*txPerGoroutine,
		float64(successCount.Load())/elapsed.Seconds())

	if errorCount.Load() > 0 {
		t.Errorf("%d errors occurred", errorCount.Load())
	}
}

func TestConcurrentGetRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	const writers = 5
	const readers = 5
	const opsPerWorker = 50
	prefix := t.Name() + "_"

	// Seed initial data so readers have something to scan from the start.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 20; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), []byte(fmt.Sprintf("seed_%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	var writeErrors atomic.Int64
	var readErrors atomic.Int64
	var readSuccess atomic.Int64
	var writeSuccess atomic.Int64

	start := time.Now()

	// Writer goroutines: continuously insert new keys.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				key := []byte(fmt.Sprintf("%sw%d_%04d", prefix, id, i))
				val := []byte(fmt.Sprintf("wval_%d_%d", id, i))
				_, werr := db.Transact(ctx, func(tx *Transaction) (any, error) {
					tx.Set(key, val)
					return nil, nil
				})
				if werr != nil {
					writeErrors.Add(1)
				} else {
					writeSuccess.Add(1)
				}
			}
		}(w)
	}

	// Reader goroutines: continuously scan the prefix range.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				result, rerr := db.Transact(ctx, func(tx *Transaction) (any, error) {
					kvs, _, err := tx.GetRange(ctx, []byte(prefix), []byte(prefix+"~"), 100)
					return kvs, err
				})
				if rerr != nil {
					readErrors.Add(1)
					continue
				}
				kvs := result.([]KeyValue)
				// Every returned key must start with prefix.
				for _, kv := range kvs {
					if !bytes.HasPrefix(kv.Key, []byte(prefix)) {
						t.Errorf("reader %d scan %d: key %q missing prefix %q", id, i, kv.Key, prefix)
						readErrors.Add(1)
						continue
					}
				}
				// Keys must be in sorted order.
				for j := 1; j < len(kvs); j++ {
					if bytes.Compare(kvs[j-1].Key, kvs[j].Key) >= 0 {
						t.Errorf("reader %d scan %d: keys not sorted at %d: %q >= %q",
							id, i, j, kvs[j-1].Key, kvs[j].Key)
						readErrors.Add(1)
						break
					}
				}
				readSuccess.Add(1)
			}
		}(r)
	}

	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("concurrent range: writes=%d(err=%d) reads=%d(err=%d) elapsed=%s",
		writeSuccess.Load(), writeErrors.Load(),
		readSuccess.Load(), readErrors.Load(),
		elapsed)
	t.Logf("throughput: %.0f writes/s, %.0f reads/s",
		float64(writeSuccess.Load())/elapsed.Seconds(),
		float64(readSuccess.Load())/elapsed.Seconds())

	if writeErrors.Load() > 0 {
		t.Errorf("%d write errors", writeErrors.Load())
	}
	if readErrors.Load() > 0 {
		t.Errorf("%d read errors", readErrors.Load())
	}
}

func TestWriteWriteConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1 and tx2 both start at the same read version.
	tx1 := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)

	tx2 := db.CreateTransaction()
	tx2.SetReadVersion(rv)

	// Both read the key (establishes read conflicts).
	_, err = tx1.Get(ctx, key)
	if err != nil {
		t.Fatalf("tx1 Get: %v", err)
	}
	_, err = tx2.Get(ctx, key)
	if err != nil {
		t.Fatalf("tx2 Get: %v", err)
	}

	// Both write different values.
	tx1.Set(key, []byte("from_tx1"))
	tx2.Set(key, []byte("from_tx2"))

	// tx1 commits first — should succeed.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 commit should succeed: %v", err)
	}

	// tx2 commits — should conflict (not_committed 1020).
	err = tx2.Commit(ctx)
	if err == nil {
		t.Fatal("tx2 should conflict after tx1 committed")
	}
	t.Logf("tx2 conflict (expected): %v", err)

	// Use OnError to reset tx2 for retry.
	if retryErr := tx2.OnError(ctx, err); retryErr != nil {
		t.Fatalf("OnError should allow retry: %v", retryErr)
	}

	// Retry tx2: read, write, commit.
	_, err = tx2.Get(ctx, key)
	if err != nil {
		t.Fatalf("tx2 retry Get: %v", err)
	}
	tx2.Set(key, []byte("from_tx2"))
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("tx2 retry commit: %v", err)
	}

	// Final value should be from_tx2 (last writer wins after retry).
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if string(result.([]byte)) != "from_tx2" {
		t.Fatalf("final value: got %q, want %q", result, "from_tx2")
	}
}

func TestReadWriteConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"

	// Seed keys in the range.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"a"), []byte("1"))
		tx.Set([]byte(pfx+"b"), []byte("2"))
		tx.Set([]byte(pfx+"c"), []byte("3"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// txA reads a range — establishes read conflict on [pfx+a, pfx+d).
	txA := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	txA.SetReadVersion(rv)
	_, _, err = txA.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"d"), 100)
	if err != nil {
		t.Fatalf("txA GetRange: %v", err)
	}

	// txB writes into that range and commits.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"b"), []byte("modified"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("txB: %v", err)
	}

	// txA tries to commit — should conflict because txB wrote into its read range.
	// txA must write something to force a real commit; without writes,
	// FDB may short-circuit commit and skip conflict detection.
	txA.Set([]byte(pfx+"unrelated"), []byte("x"))
	err = txA.Commit(ctx)
	if err == nil {
		t.Fatal("txA should conflict — txB wrote into its read range")
	}
	t.Logf("txA conflict (expected): %v", err)
}

func TestRetryCountIncrement(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")

	// Seed a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("initial"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Use Transact with a function that intentionally conflicts on the first
	// attempt: it writes outside Transact before returning, forcing a conflict
	// on the second call onwards. We track call count with an atomic counter.
	var callCount atomic.Int64
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		n := callCount.Add(1)
		_, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		tx.Set(key, []byte(fmt.Sprintf("attempt_%d", n)))

		if n == 1 {
			// Cause a conflict: write the same key from another transaction
			// before this one commits.
			_, err := db.Transact(ctx, func(tx2 *Transaction) (any, error) {
				tx2.Set(key, []byte("spoiler"))
				return nil, nil
			})
			if err != nil {
				return nil, fmt.Errorf("spoiler: %w", err)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}

	calls := callCount.Load()
	if calls < 2 {
		t.Fatalf("expected at least 2 calls (1 conflict + 1 retry), got %d", calls)
	}
	t.Logf("Transact completed after %d attempts", calls)

	// Verify the key was written.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	val := string(result.([]byte))
	if !strings.HasPrefix(val, "attempt_") {
		t.Fatalf("final value: got %q, want attempt_N", val)
	}
	t.Logf("final value: %q", val)
}

func TestConcurrentWritesSameKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")
	const goroutines = 10

	var wg sync.WaitGroup
	var errCount atomic.Int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			val := []byte(fmt.Sprintf("writer_%d", id))
			_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				tx.Set(key, val)
				return nil, nil
			})
			if err != nil {
				errCount.Add(1)
				t.Errorf("goroutine %d: %v", id, err)
			}
		}(g)
	}
	wg.Wait()

	if errCount.Load() > 0 {
		t.Fatalf("%d goroutines failed", errCount.Load())
	}

	// Read the final value — should be one of the 10 writer values.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	val := string(result.([]byte))
	if !strings.HasPrefix(val, "writer_") {
		t.Fatalf("final value: got %q, want writer_N", val)
	}
	t.Logf("final value (last writer wins): %q", val)
}

func TestAtomicAddConcurrent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_counter")
	const goroutines = 10

	// Initialize counter to 0.
	var zeroBuf [8]byte
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, zeroBuf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// 10 goroutines each atomically ADD 1.
	var wg sync.WaitGroup
	var addBuf [8]byte
	binary.LittleEndian.PutUint64(addBuf[:], 1)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				tx.Atomic(MutAddValue, key, addBuf[:])
				return nil, nil
			})
			if err != nil {
				t.Errorf("atomic ADD: %v", err)
			}
		}()
	}
	wg.Wait()

	// Read counter — should be exactly 10.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	counter := binary.LittleEndian.Uint64(result.([]byte))
	if counter != goroutines {
		t.Fatalf("counter: got %d, want %d", counter, goroutines)
	}
	t.Logf("counter after %d concurrent adds: %d", goroutines, counter)
}

func TestGetRangeReverseAllModes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"
	const count = 20

	// Write 20 keys: pfx+0000 to pfx+0019.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < count; i++ {
			k := []byte(fmt.Sprintf("%s%04d", pfx, i))
			tx.Set(k, []byte(fmt.Sprintf("val_%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Our API uses limit (not streaming mode), but we can exercise different
	// limit values to test the reverse scan path thoroughly.
	limits := []struct {
		name  string
		limit int
	}{
		{"unlimited", 0},
		{"exact_20", 20},
		{"small_5", 5},
		{"medium_10", 10},
		{"large_50", 50},
		{"one", 1},
		{"all_100", 100},
	}

	for _, tc := range limits {
		t.Run(tc.name, func(t *testing.T) {
			result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				kvs, _, err := tx.GetRangeReverse(ctx,
					[]byte(pfx),
					[]byte(pfx+"~"),
					tc.limit)
				return kvs, err
			})
			if err != nil {
				t.Fatalf("GetRangeReverse(%s): %v", tc.name, err)
			}
			kvs := result.([]KeyValue)

			// Determine expected count.
			expected := count
			if tc.limit > 0 && tc.limit < count {
				expected = tc.limit
			}
			if len(kvs) != expected {
				t.Fatalf("count: got %d, want %d", len(kvs), expected)
			}

			// Verify reverse order: keys must be strictly descending.
			for i := 1; i < len(kvs); i++ {
				if bytes.Compare(kvs[i-1].Key, kvs[i].Key) <= 0 {
					t.Fatalf("not reverse at %d: %q <= %q", i, kvs[i-1].Key, kvs[i].Key)
				}
			}

			// If we got all 20, verify first and last.
			if len(kvs) == count {
				wantFirst := fmt.Sprintf("%s0019", pfx)
				wantLast := fmt.Sprintf("%s0000", pfx)
				if string(kvs[0].Key) != wantFirst {
					t.Errorf("first key: got %q, want %q", kvs[0].Key, wantFirst)
				}
				if string(kvs[count-1].Key) != wantLast {
					t.Errorf("last key: got %q, want %q", kvs[count-1].Key, wantLast)
				}
			}
		})
	}
}

func TestWatchClearRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	pfx := t.Name() + "_"
	key := []byte(pfx + "key")

	// Set initial value.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Start watch in a goroutine.
	watchCtx, watchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer watchCancel()

	watchErr := make(chan error, 1)
	go func() {
		_, err := db.Transact(watchCtx, func(tx *Transaction) (any, error) {
			_, err := tx.Get(watchCtx, key)
			if err != nil {
				return nil, err
			}
			return nil, tx.Watch(watchCtx, key)
		})
		watchErr <- err
	}()

	// Let the watch register, then ClearRange covering the key.
	time.Sleep(500 * time.Millisecond)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return nil, tx.ClearRange([]byte(pfx), []byte(pfx+"~"))
	})
	if err != nil {
		t.Fatalf("ClearRange: %v", err)
	}

	// Watch should fire.
	select {
	case err := <-watchErr:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case <-watchCtx.Done():
		t.Fatal("watch did not resolve within 10 seconds after ClearRange")
	}

	// Verify key is gone.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.([]byte) != nil {
		t.Fatalf("key should be cleared, got %q", result)
	}
}

func TestWatchNonExistentKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_watch_new_key")

	// Key does NOT exist yet. Start a watch on it.
	watchCtx, watchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer watchCancel()

	watchErr := make(chan error, 1)
	go func() {
		_, err := db.Transact(watchCtx, func(tx *Transaction) (any, error) {
			// Read the key (nil) to establish watch version.
			_, err := tx.Get(watchCtx, key)
			if err != nil {
				return nil, err
			}
			return nil, tx.Watch(watchCtx, key)
		})
		watchErr <- err
	}()

	// Let the watch register, then set the key from another transaction.
	time.Sleep(500 * time.Millisecond)
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("now_exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	// Watch should fire.
	select {
	case err := <-watchErr:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case <-watchCtx.Done():
		t.Fatal("watch did not resolve within 10 seconds after setting non-existent key")
	}

	// Verify the key has the new value.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if string(result.([]byte)) != "now_exists" {
		t.Fatalf("value: got %q, want %q", result, "now_exists")
	}
}

// TestWatchTimeoutViaContext verifies that a Watch on an unchanging key
// respects the context deadline and returns context.DeadlineExceeded.
func TestWatchTimeoutViaContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_watch_timeout_key")

	// Seed a key with a value.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("stable"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Watch with a short context deadline. Don't change the key.
	watchCtx, watchCancel := context.WithTimeout(ctx, 2*time.Second)
	defer watchCancel()

	start := time.Now()
	_, err = db.Transact(watchCtx, func(tx *Transaction) (any, error) {
		// Read key to establish watch version.
		_, err := tx.Get(watchCtx, key)
		if err != nil {
			return nil, err
		}
		return nil, tx.Watch(watchCtx, key)
	})
	elapsed := time.Since(start)

	// Watch should have returned with a context deadline error.
	if err == nil {
		t.Fatal("Watch should have timed out, but returned nil")
	}

	// Check that the error is deadline-related.
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected context deadline/canceled error, got: %v", err)
	}

	// Should have waited roughly 2 seconds, not the full 120s.
	if elapsed > 10*time.Second {
		t.Fatalf("Watch took too long (%v) — context deadline should have cut it short", elapsed)
	}
	t.Logf("Watch timed out after %v (expected ~2s)", elapsed)
}

// TestWatchFiresOnAtomicMutation verifies that a Watch fires when the watched
// key is mutated via an atomic operation (not just Set).
func TestWatchFiresOnAtomicMutation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_watch_atomic_key")

	// Seed key with counter=0.
	var zeroBuf [8]byte
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, zeroBuf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Start watch in a goroutine.
	watchCtx, watchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer watchCancel()

	watchErr := make(chan error, 1)
	go func() {
		_, err := db.Transact(watchCtx, func(tx *Transaction) (any, error) {
			_, err := tx.Get(watchCtx, key)
			if err != nil {
				return nil, err
			}
			return nil, tx.Watch(watchCtx, key)
		})
		watchErr <- err
	}()

	// Let the watch register, then do an atomic ADD on the key.
	time.Sleep(500 * time.Millisecond)
	var addBuf [8]byte
	binary.LittleEndian.PutUint64(addBuf[:], 42)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutAddValue, key, addBuf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("atomic mutation: %v", err)
	}

	// Watch should fire.
	select {
	case err := <-watchErr:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case <-watchCtx.Done():
		t.Fatal("watch did not fire within 10 seconds after atomic mutation")
	}

	// Verify the value is 42.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	got := binary.LittleEndian.Uint64(result.([]byte))
	if got != 42 {
		t.Fatalf("counter: got %d, want 42", got)
	}
}

// TestWatchCancellation verifies that cancelling the context returns
// context.Canceled from the Watch.
func TestWatchCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_watch_cancel_key")

	// Seed a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("stable"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Create a cancellable context for the watch.
	watchCtx, watchCancel := context.WithCancel(ctx)

	watchErr := make(chan error, 1)
	go func() {
		_, err := db.Transact(watchCtx, func(tx *Transaction) (any, error) {
			_, err := tx.Get(watchCtx, key)
			if err != nil {
				return nil, err
			}
			return nil, tx.Watch(watchCtx, key)
		})
		watchErr <- err
	}()

	// Let the watch register, then cancel the context.
	time.Sleep(500 * time.Millisecond)
	watchCancel()

	// Watch should return with a cancellation error.
	select {
	case err := <-watchErr:
		if err == nil {
			t.Fatal("Watch should return error on context cancellation")
		}
		// The error should be context-related (Canceled or DeadlineExceeded).
		if !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "context") {
			t.Fatalf("expected context cancellation error, got: %v", err)
		}
		t.Logf("Watch cancellation (expected): %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return within 10 seconds after context cancellation")
	}
}

// TestHedgeKnob verifies SetHedgeEnabled controls speculative requests.
func TestHedgeKnob(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Default: hedge enabled.
	if !db.HedgeEnabled() {
		t.Fatal("hedge should be enabled by default")
	}

	key := []byte(t.Name() + "_key")

	// Write a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("hedged"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	// Read with hedge enabled — should work.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("get with hedge: %v", err)
	}
	if string(result.([]byte)) != "hedged" {
		t.Fatalf("got %q, want %q", result, "hedged")
	}

	// Disable hedge.
	db.SetHedgeEnabled(false)
	if db.HedgeEnabled() {
		t.Fatal("hedge should be disabled after SetHedgeEnabled(false)")
	}

	// Read with hedge disabled — should still work (just no hedging).
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("get without hedge: %v", err)
	}
	if string(result.([]byte)) != "hedged" {
		t.Fatalf("got %q, want %q", result, "hedged")
	}

	// Re-enable.
	db.SetHedgeEnabled(true)
	if !db.HedgeEnabled() {
		t.Fatal("hedge should be re-enabled")
	}
}
