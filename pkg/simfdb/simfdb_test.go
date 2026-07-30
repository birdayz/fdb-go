package simfdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

func k(s string) fdb.Key { return fdb.Key(s) }

func errCode(t *testing.T, err error) int {
	t.Helper()
	var e fdb.Error
	if !errors.As(err, &e) {
		t.Fatalf("not an fdb.Error: %v", err)
	}
	return e.Code
}

func TestBasicSetGetCommit(t *testing.T) {
	t.Parallel()
	db := New(nil)
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("a"), []byte("1"))
		tx.Set(k("b"), []byte("2"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return string(tx.Get(k("a")).MustGet()), nil
	})
	if err != nil || got != "1" {
		t.Fatalf("Get(a) = %q, %v; want \"1\"", got, err)
	}
	// Absent key returns nil.
	nilv, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return tx.Get(k("missing")).MustGet(), nil
	})
	if nilv.([]byte) != nil {
		t.Fatalf("Get(missing) = %v, want nil", nilv)
	}
}

func TestReadYourWrites(t *testing.T) {
	t.Parallel()
	db := New(nil)
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("x"), []byte("old"))
		return nil, nil
	})
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("x"), []byte("new"))
		if got := string(tx.Get(k("x")).MustGet()); got != "new" {
			t.Fatalf("RYW Get(x) = %q, want \"new\"", got)
		}
		tx.Clear(k("x"))
		if got := tx.Get(k("x")).MustGet(); got != nil {
			t.Fatalf("RYW after clear = %v, want nil", got)
		}
		// present-empty is distinct from absent
		tx.Set(k("x"), []byte{})
		if got := tx.Get(k("x")).MustGet(); got == nil {
			t.Fatal("RYW present-empty read as nil (absent)")
		}
		return nil, nil
	})
}

func TestMVCCSnapshotIsolation(t *testing.T) {
	t.Parallel()
	db := New(nil)
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("v"), []byte("0"))
		return nil, nil
	})
	// txA pins its read version, then txB commits a new value; txA must still see the old one.
	txA := db.newTxn(false)
	if got := string(txA.Get(k("v")).MustGet()); got != "0" {
		t.Fatalf("txA initial = %q", got)
	}
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("v"), []byte("1"))
		return nil, nil
	})
	if got := string(txA.Get(k("v")).MustGet()); got != "0" {
		t.Fatalf("txA after external commit = %q, want \"0\" (snapshot isolation)", got)
	}
}

func TestSSIReadWriteConflict(t *testing.T) {
	t.Parallel()
	db := New(nil)
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("c"), []byte("0"))
		return nil, nil
	})
	txA := db.newTxn(false)
	txA.Get(k("c")).MustGet() // read → adds read conflict on c

	// txB writes c and commits first.
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("c"), []byte("B"))
		return nil, nil
	})

	// txA writes c and commits: it read c at a version before txB's commit → not_committed(1020).
	txA.Set(k("c"), []byte("A"))
	if code := errCode(t, txA.Commit().Get()); code != 1020 {
		t.Fatalf("expected 1020 (not_committed), got %d", code)
	}
}

func TestDisjointKeysNoConflict(t *testing.T) {
	t.Parallel()
	db := New(nil)
	txA := db.newTxn(false)
	txB := db.newTxn(false)
	txA.Get(k("a")).MustGet()
	txB.Get(k("b")).MustGet()
	txA.Set(k("a"), []byte("A"))
	txB.Set(k("b"), []byte("B"))
	if err := txB.Commit().Get(); err != nil {
		t.Fatalf("txB commit: %v", err)
	}
	if err := txA.Commit().Get(); err != nil {
		t.Fatalf("txA commit (disjoint) should not conflict: %v", err)
	}
}

func TestBlindWritesDoNotConflict(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// Two write-only transactions to the same key: no reads, so no read conflict ranges → both
	// commit (last writer wins). Write-only transactions never conflict.
	txA := db.newTxn(false)
	txB := db.newTxn(false)
	txA.Set(k("w"), []byte("A"))
	txB.Set(k("w"), []byte("B"))
	if err := txB.Commit().Get(); err != nil {
		t.Fatal(err)
	}
	if err := txA.Commit().Get(); err != nil {
		t.Fatalf("blind write should not conflict: %v", err)
	}
}

func TestRangeReadSortedWithRYWReverseLimit(t *testing.T) {
	t.Parallel()
	db := New(nil)
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		for _, s := range []string{"k1", "k2", "k4"} {
			tx.Set(k(s), []byte(s))
		}
		return nil, nil
	})
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("k3"), []byte("k3")) // pending write merges into the range
		tx.Clear(k("k2"))             // pending clear removes from the range
		rr := tx.GetRange(fdb.KeyRange{Begin: k("k0"), End: k("k9")}, fdb.RangeOptions{})
		kvs := rr.GetSliceOrPanic()
		got := keysOf(kvs)
		want := []string{"k1", "k3", "k4"}
		if !eqStrs(got, want) {
			t.Fatalf("range = %v, want %v (RYW merge)", got, want)
		}
		// reverse
		rev := tx.GetRange(fdb.KeyRange{Begin: k("k0"), End: k("k9")}, fdb.RangeOptions{Reverse: true}).GetSliceOrPanic()
		if !eqStrs(keysOf(rev), []string{"k4", "k3", "k1"}) {
			t.Fatalf("reverse range = %v", keysOf(rev))
		}
		// limit
		lim := tx.GetRange(fdb.KeyRange{Begin: k("k0"), End: k("k9")}, fdb.RangeOptions{Limit: 2}).GetSliceOrPanic()
		if !eqStrs(keysOf(lim), []string{"k1", "k3"}) {
			t.Fatalf("limited range = %v", keysOf(lim))
		}
		return nil, nil
	})
}

func TestAtomicAddAppliesAtCommit(t *testing.T) {
	t.Parallel()
	db := New(nil)
	le := func(n uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, n); return b }
	db.Transact(func(tx fdb.WritableTransaction) (any, error) { tx.Add(k("cnt"), le(5)); return nil, nil })
	db.Transact(func(tx fdb.WritableTransaction) (any, error) { tx.Add(k("cnt"), le(3)); return nil, nil })
	got, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return binary.LittleEndian.Uint64(tx.Get(k("cnt")).MustGet()), nil
	})
	if got.(uint64) != 8 {
		t.Fatalf("Add accumulation = %d, want 8", got)
	}
}

func TestVersionstampedKeyStampedAndReadable(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// Key: 2-byte prefix + 10-byte 0xFF placeholder + 4-byte LE offset (=2, placeholder start).
	buildKey := func() []byte {
		key := append([]byte{0xAA, 0xBB}, bytes.Repeat([]byte{0xFF}, 10)...)
		off := make([]byte, 4)
		binary.LittleEndian.PutUint32(off, 2)
		return append(key, off...)
	}
	var stamp fdb.Key
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Options().SetNextWriteNoWriteConflictRange()
		tx.SetVersionstampedKey(fdb.Key(buildKey()), []byte("payload"))
		return nil, nil
	})
	// GetVersionstamp is only valid on the committing txn; check the stamped key landed by
	// scanning the prefix.
	got, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return tx.GetRange(fdb.KeyRange{Begin: k("\xaa"), End: k("\xac")}, fdb.RangeOptions{}).GetSliceOrPanic(), nil
	})
	kvs := got.([]fdb.KeyValue)
	if len(kvs) != 1 {
		t.Fatalf("expected 1 stamped key, got %d", len(kvs))
	}
	stampedKey := kvs[0].Key
	// The 10 bytes at offset 2 must be the commit version (non-0xFF), and the 4-byte offset is stripped.
	if len(stampedKey) != 12 {
		t.Fatalf("stamped key len = %d, want 12 (offset stripped)", len(stampedKey))
	}
	if bytes.Equal(stampedKey[2:12], bytes.Repeat([]byte{0xFF}, 10)) {
		t.Fatal("placeholder was not overwritten")
	}
	if string(kvs[0].Value) != "payload" {
		t.Fatalf("value = %q", kvs[0].Value)
	}
	_ = stamp
}

func TestSizeLimits(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// key_too_large (2102)
	if code := errCode(t, db.run(func(tx fdb.WritableTransaction) {
		tx.Set(fdb.Key(bytes.Repeat([]byte("k"), keySizeLimit+1)), []byte("v"))
	})); code != 2102 {
		t.Fatalf("oversized key: got %d, want 2102", code)
	}
	// value_too_large (2103)
	if code := errCode(t, db.run(func(tx fdb.WritableTransaction) {
		tx.Set(k("k"), bytes.Repeat([]byte("v"), valueSizeLimit+1))
	})); code != 2103 {
		t.Fatalf("oversized value: got %d, want 2103", code)
	}
}

func TestDeterministicVersions(t *testing.T) {
	t.Parallel()
	run := func() int64 {
		db := New(nil)
		for i := 0; i < 10; i++ {
			db.Transact(func(tx fdb.WritableTransaction) (any, error) { tx.Set(k("z"), []byte{byte(i)}); return nil, nil })
		}
		return db.lastVersion
	}
	if a, b := run(), run(); a != b || a != 10 {
		t.Fatalf("versions not deterministic: %d vs %d (want 10)", a, b)
	}
}

func TestOnErrorRetryableResets(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn(false)
	tx.Set(k("k"), []byte("v"))
	// A retryable error resets the txn (buffer dropped) and resolves success.
	if err := tx.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
		t.Fatalf("OnError(1020) should resolve nil, got %v", err)
	}
	if len(tx.buffer) != 0 {
		t.Fatal("OnError(retryable) did not reset the buffer")
	}
	// A non-retryable error resolves the error itself.
	if code := errCode(t, tx.OnError(fdb.Error{Code: 2101}).Get()); code != 2101 {
		t.Fatalf("OnError(2101) = %d, want 2101 passthrough", code)
	}
}

// run is a test helper: a single-shot transaction that returns the commit error.
func (db *SimDB) run(fn func(fdb.WritableTransaction)) error {
	tx := db.newTxn(false)
	fn(tx)
	return tx.Commit().Get()
}

func keysOf(kvs []fdb.KeyValue) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = string(kv.Key)
	}
	return out
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
