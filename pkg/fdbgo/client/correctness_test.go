package client

import (
	"bytes"
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("clear_me"), []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify it exists.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("clear_me"))
	})
	if err != nil {
		t.Fatalf("Get before clear: %v", err)
	}
	if string(result.([]byte)) != "exists" {
		t.Fatalf("before clear: got %q, want %q", result, "exists")
	}

	// Clear it.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear([]byte("clear_me"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}

	// Verify it's gone.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write 5 keys: cr_a, cr_b, cr_c, cr_d, cr_e
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, suffix := range []string{"a", "b", "c", "d", "e"} {
			tx.Set([]byte("cr_"+suffix), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear range [cr_b, cr_d) — should delete cr_b, cr_c.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte("cr_b"), []byte("cr_d"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ClearRange: %v", err)
	}

	// Verify: cr_a and cr_d and cr_e survive.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte("counter")

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

	// Write 10 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write two keys in one transaction, read both back in another.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("mk_x"), []byte("100"))
		tx.Set([]byte("mk_y"), []byte("200"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write keys: gk_a, gk_b, gk_c, gk_d, gk_e
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, s := range []string{"a", "b", "c", "d", "e"} {
			tx.Set([]byte("gk_"+s), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// firstGreaterOrEqual("gk_c") → should return "gk_c" (exact match)
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte("gk_c"), false, 1) // orEqual=false, offset=1 = firstGreaterOrEqual
	})
	if err != nil {
		t.Fatalf("firstGreaterOrEqual: %v", err)
	}
	if string(result.([]byte)) != "gk_c" {
		t.Errorf("firstGreaterOrEqual(gk_c): got %q, want %q", result, "gk_c")
	}

	// firstGreaterThan("gk_c") → should return "gk_d"
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte("gk_c"), true, 1) // orEqual=true, offset=1 = firstGreaterThan
	})
	if err != nil {
		t.Fatalf("firstGreaterThan: %v", err)
	}
	if string(result.([]byte)) != "gk_d" {
		t.Errorf("firstGreaterThan(gk_c): got %q, want %q", result, "gk_d")
	}

	// lastLessOrEqual("gk_c") → should return "gk_c"
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte("gk_c"), true, 0) // orEqual=true, offset=0 = lastLessOrEqual
	})
	if err != nil {
		t.Fatalf("lastLessOrEqual: %v", err)
	}
	if string(result.([]byte)) != "gk_c" {
		t.Errorf("lastLessOrEqual(gk_c): got %q, want %q", result, "gk_c")
	}

	// lastLessThan("gk_c") → should return "gk_b"
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Seed a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	rv, _ := db.db.grvBatcher.getReadVersion(db.db, ctx, 8<<24)
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
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	rv3, _ := db.db.grvBatcher.getReadVersion(db.db, ctx, 8<<24)
	tx3.SetReadVersion(rv3)

	// Regular read — adds conflict range.
	_, _ = tx3.Get(ctx, []byte("snap_key"))

	// Another transaction writes the same key.
	_, _ = db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("ecr_key"), []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// AddReadConflictKey: tx1 adds explicit read conflict (no actual read),
	// tx2 writes the same key. tx1 should conflict on commit.
	tx1 := db.CreateTransaction()
	rv, _ := db.db.grvBatcher.getReadVersion(db.db, ctx, 8<<24)
	tx1.SetReadVersion(rv)
	tx1.AddReadConflictKey([]byte("ecr_key"))
	tx1.Set([]byte("ecr_other"), []byte("unrelated"))

	// tx2 writes the conflicting key.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
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
	rv3, _ := db.db.grvBatcher.getReadVersion(db.db, ctx, 8<<24)
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Write a key so there's something to read.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("cancel_key"), []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Create a real transaction, do a successful read, then cancel.
	tx := db.CreateTransaction()
	val, err := tx.Get(ctx, []byte("cancel_key"))
	if err != nil {
		t.Fatalf("Get before Cancel: %v", err)
	}
	if string(val) != "exists" {
		t.Fatalf("Get before Cancel: got %q, want %q", val, "exists")
	}

	tx.Cancel()

	// Get after Cancel should fail.
	_, err = tx.Get(ctx, []byte("cancel_key"))
	if err == nil {
		t.Error("Get after Cancel should fail")
	}

	// Commit after Cancel should fail.
	tx.Set([]byte("cancel_key"), []byte("modified"))
	err = tx.Commit(ctx)
	if err == nil {
		t.Error("Commit after Cancel should fail")
	}

	// Verify the key was NOT modified (cancel prevented commit).
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("cancel_key"))
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

	// Transact with only reads, no writes — commit should be a no-op.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Read a non-existent key. No mutations.
		_, err := tx.Get(ctx, []byte("readonly_nonexistent"))
		return nil, err
	})
	if err != nil {
		t.Fatalf("read-only Transact should succeed: %v", err)
	}

	// Seed a key, then read it in a read-only transaction.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("readonly_key"), []byte("val"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("readonly_key"))
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

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("rcr_a"), []byte("1"))
		tx.Set([]byte("rcr_b"), []byte("2"))
		tx.Set([]byte("rcr_c"), []byte("3"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// tx1: add read conflict range [rcr_a, rcr_d) — covers a, b, c.
	tx1 := db.CreateTransaction()
	rv, _ := db.db.grvBatcher.getReadVersion(db.db, ctx, 8<<24)
	tx1.SetReadVersion(rv)
	tx1.AddReadConflictRange([]byte("rcr_a"), []byte("rcr_d"))
	tx1.Set([]byte("rcr_unrelated"), []byte("x"))

	// tx2: write rcr_b (inside the conflict range).
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("rcr_b"), []byte("modified"))
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

	// tx1 reads a key in the range [wcr_a, wcr_d).
	tx1 := db.CreateTransaction()
	rv, _ := db.db.grvBatcher.getReadVersion(db.db, ctx, 8<<24)
	tx1.SetReadVersion(rv)
	_, _ = tx1.Get(ctx, []byte("wcr_b")) // adds read conflict for wcr_b
	tx1.Set([]byte("wcr_b"), []byte("from_tx1"))

	// tx2 adds a write conflict range covering [wcr_a, wcr_d).
	tx2 := db.CreateTransaction()
	tx2.SetReadVersion(rv)
	tx2.AddWriteConflictRange([]byte("wcr_a"), []byte("wcr_d"))
	tx2.Set([]byte("wcr_other"), []byte("x")) // need a mutation to commit
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

	// Seed keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("rt_a"), []byte("1"))
		tx.Set([]byte("rt_b"), []byte("2"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// ReadTransact: read two keys, return their values.
	result, err := db.ReadTransact(ctx, func(tx *Transaction) (any, error) {
		a, err := tx.Get(ctx, []byte("rt_a"))
		if err != nil {
			return nil, err
		}
		b, err := tx.Get(ctx, []byte("rt_b"))
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
		tx.Set([]byte("rt_phantom"), []byte("should_not_persist"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ReadTransact with write: %v", err)
	}

	// Verify the write didn't persist.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("rt_phantom"))
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

	// Commit a transaction and get the versionstamp.
	tx1 := db.CreateTransaction()
	rv, err := db.db.grvBatcher.getReadVersion(db.db, ctx, 8<<24)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)
	tx1.Set([]byte("vs_key1"), []byte("val1"))
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
	rv2, _ := db.db.grvBatcher.getReadVersion(db.db, ctx, 8<<24)
	tx2.SetReadVersion(rv2)
	tx2.Set([]byte("vs_key2"), []byte("val2"))
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

func TestEmptyRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
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
