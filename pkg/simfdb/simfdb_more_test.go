package simfdb

import (
	"bytes"
	"encoding/binary"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

func seed(db *SimDB, kv ...string) {
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		for i := 0; i+1 < len(kv); i += 2 {
			tx.Set(k(kv[i]), []byte(kv[i+1]))
		}
		return nil, nil
	})
}

func TestGetKeySelectorResolution(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "b", "1", "d", "2", "f", "3") // keys: b, d, f

	get := func(sel fdb.Selectable) string {
		v, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
			return string(tx.GetKey(sel).MustGet()), nil
		})
		return v.(string)
	}
	cases := []struct {
		name string
		sel  fdb.KeySelector
		want string
	}{
		{"FGE(c)->d", fdb.FirstGreaterOrEqual(k("c")), "d"},
		{"FGE(d)->d", fdb.FirstGreaterOrEqual(k("d")), "d"},
		{"FGT(d)->f", fdb.FirstGreaterThan(k("d")), "f"},
		{"LLE(d)->d", fdb.LastLessOrEqual(k("d")), "d"},
		{"LLT(d)->b", fdb.LastLessThan(k("d")), "b"},
		{"FGE(a)->b", fdb.FirstGreaterOrEqual(k("a")), "b"},
	}
	for _, c := range cases {
		if got := get(c.sel); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSnapshotReadAddsNoConflict(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "s", "0")
	// txA snapshot-reads s (no read conflict), then writes a different key and commits after
	// txB writes s. A snapshot read must NOT cause a conflict.
	txA := db.newTxn(false)
	txA.Snapshot().Get(k("s")).MustGet() // snapshot read: no conflict
	db.Transact(func(tx fdb.WritableTransaction) (any, error) { tx.Set(k("s"), []byte("B")); return nil, nil })
	txA.Set(k("other"), []byte("A"))
	if err := txA.Commit().Get(); err != nil {
		t.Fatalf("snapshot read should not conflict, got %v", err)
	}
}

func TestTransactionTooOldWindow(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// Simulate a far-advanced database so an ancient read version falls outside the MVCC window.
	db.lastVersion = mvccWindow + 100
	tx := db.newTxn(false)
	tx.SetReadVersion(0) // ancient
	tx.Get(k("x")).MustGet()
	tx.Set(k("x"), []byte("v"))
	if code := errCode(t, tx.Commit().Get()); code != 1007 {
		t.Fatalf("ancient read version: got %d, want transaction_too_old(1007)", code)
	}

	// A write-only transaction (no read conflict ranges) at an ancient read version does NOT
	// get too_old — 1007 gates on having read conflict ranges.
	wtx := db.newTxn(false)
	wtx.SetReadVersion(0)
	wtx.Set(k("y"), []byte("v"))
	if err := wtx.Commit().Get(); err != nil {
		t.Fatalf("write-only ancient txn should commit, got %v", err)
	}
}

func TestVersionstampedValueStamped(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// Value: 3-byte prefix + 10-byte placeholder + 4-byte LE offset (=3).
	val := append([]byte{1, 2, 3}, bytes.Repeat([]byte{0xFF}, 10)...)
	off := make([]byte, 4)
	binary.LittleEndian.PutUint32(off, 3)
	val = append(val, off...)
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.SetVersionstampedValue(k("vk"), val)
		return nil, nil
	})
	got, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return tx.Get(k("vk")).MustGet(), nil
	})
	stamped := got.([]byte)
	if len(stamped) != 13 { // 3 prefix + 10 stamp, offset stripped
		t.Fatalf("stamped value len = %d, want 13", len(stamped))
	}
	if bytes.Equal(stamped[3:13], bytes.Repeat([]byte{0xFF}, 10)) {
		t.Fatal("value placeholder not overwritten")
	}
}

func TestClearRangeRemovesOnlyRange(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "1", "b", "2", "c", "3", "d", "4")
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.ClearRange(fdb.KeyRange{Begin: k("b"), End: k("d")}) // clears b, c (not a, not d)
		return nil, nil
	})
	got, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return keysOf(tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{}).GetSliceOrPanic()), nil
	})
	if !eqStrs(got.([]string), []string{"a", "d"}) {
		t.Fatalf("after ClearRange [b,d): keys = %v, want [a d]", got)
	}
}

func TestAddReadConflictRangeExplicit(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "k", "0")
	// An explicit read conflict range conflicts with a later write, even without a Get.
	txA := db.newTxn(false)
	txA.ensureReadVersion()
	if err := txA.AddReadConflictRange(fdb.KeyRange{Begin: k("k"), End: k("l")}); err != nil {
		t.Fatal(err)
	}
	db.Transact(func(tx fdb.WritableTransaction) (any, error) { tx.Set(k("k"), []byte("B")); return nil, nil })
	txA.Set(k("other"), []byte("A"))
	if code := errCode(t, txA.Commit().Get()); code != 1020 {
		t.Fatalf("explicit read conflict range: got %d, want 1020", code)
	}
}
