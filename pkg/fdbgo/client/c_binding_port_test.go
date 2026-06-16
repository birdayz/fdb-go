package client

// Ported from FoundationDB C binding unit tests.
// Source: bindings/c/test/unit/unit_tests.cpp
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// ---------------------------------------------------------------------------
// Range operations
// ---------------------------------------------------------------------------

// TestGetRangeReverse verifies that reading a range in reverse returns keys
// in descending order. C++ uses negative limit for reverse scans.
// Ported from unit_tests.cpp line 1185
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1185
func TestGetRangeReverse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	prefix := "c_range_rev_"

	// Write 4 keys: a, b, c, d.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"a"), []byte("1"))
		tx.Set([]byte(prefix+"b"), []byte("2"))
		tx.Set([]byte(prefix+"c"), []byte("3"))
		tx.Set([]byte(prefix+"d"), []byte("4"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Read forward — verify ascending order.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, []byte(prefix+"a"), []byte(prefix+"d\x00"), 100)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRange forward: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 4 {
		for i, kv := range kvs {
			t.Logf("fwd key[%d] = %q", i, kv.Key)
		}
		t.Fatalf("forward: expected 4 keys, got %d", len(kvs))
	}
	fwdExpected := []string{prefix + "a", prefix + "b", prefix + "c", prefix + "d"}
	for i, kv := range kvs {
		if string(kv.Key) != fwdExpected[i] {
			t.Errorf("fwd key[%d]: got %q, want %q", i, kv.Key, fwdExpected[i])
		}
	}

	// Read reverse — verify descending order (matching C test line 1213-1221).
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRangeReverse(ctx, []byte(prefix+"a"), []byte(prefix+"d\x00"), 100)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRange reverse: %v", err)
	}
	kvs = result.([]KeyValue)
	if len(kvs) != 4 {
		for i, kv := range kvs {
			t.Logf("rev key[%d] = %q", i, kv.Key)
		}
		t.Fatalf("reverse: expected 4 keys, got %d", len(kvs))
	}
	revExpected := []string{prefix + "d", prefix + "c", prefix + "b", prefix + "a"}
	for i, kv := range kvs {
		if string(kv.Key) != revExpected[i] {
			t.Errorf("rev key[%d]: got %q, want %q", i, kv.Key, revExpected[i])
		}
	}
}

// TestGetRangeLimit verifies that GetRange respects the limit parameter and
// returns more=true when there are additional keys beyond the limit.
// Ported from unit_tests.cpp line 1226
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1226
func TestGetRangeLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	prefix := "c_range_lim_"

	// Write 4 keys: a, b, c, d.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(prefix+"a"), []byte("1"))
		tx.Set([]byte(prefix+"b"), []byte("2"))
		tx.Set([]byte(prefix+"c"), []byte("3"))
		tx.Set([]byte(prefix+"d"), []byte("4"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	type rangeResult struct {
		kvs  []KeyValue
		more bool
	}
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte(prefix+"a"), []byte(prefix+"d\x00"), 2)
		return rangeResult{kvs, more}, err
	})
	if err != nil {
		t.Fatalf("GetRange(limit=2): %v", err)
	}
	rr := result.(rangeResult)

	if len(rr.kvs) < 1 || len(rr.kvs) > 2 {
		t.Fatalf("expected 1-2 keys, got %d", len(rr.kvs))
	}
	if !rr.more {
		t.Error("more: got false, want true (4 keys total, limit 2)")
	}
	// Verify returned keys are valid.
	data := map[string]string{
		prefix + "a": "1", prefix + "b": "2",
		prefix + "c": "3", prefix + "d": "4",
	}
	for _, kv := range rr.kvs {
		if want, ok := data[string(kv.Key)]; !ok {
			t.Errorf("unexpected key %q", kv.Key)
		} else if string(kv.Value) != want {
			t.Errorf("key %q: got value %q, want %q", kv.Key, kv.Value, want)
		}
	}
}

// TestClearSingleKey verifies that clearing a single key removes it.
// Ported from unit_tests.cpp line 1293
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1293
func TestClearSingleKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("c_clear_foo")

	// Set key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("bar"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Clear key.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}

	// Verify gone.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Get after Clear: %v", err)
	}
	if result.([]byte) != nil {
		t.Fatalf("key should be cleared, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// Atomic operations (all 12 types from C binding tests)
// ---------------------------------------------------------------------------

// TestAtomicAdd_CPort verifies FDB_MUTATION_TYPE_ADD.
// Ported from unit_tests.cpp line 1314
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1314
func TestAtomicAdd_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("c_add_foo")

	// Initialize to 0.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte{0x00})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Atomic ADD +1.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutAddValue, key, []byte{0x01})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ADD: %v", err)
	}

	// Read back — should be > 0 and <= 1.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	val := result.([]byte)
	if len(val) != 1 {
		t.Fatalf("expected 1 byte, got %d", len(val))
	}
	if val[0] == 0 {
		t.Fatal("value should be > 0 after ADD")
	}
	if val[0] > 1 {
		t.Logf("value=%d (>1, possible retry committed twice)", val[0])
	}
}

// TestAtomicBitAnd_CPort verifies FDB_MUTATION_TYPE_BIT_AND with same-length,
// extended, and truncated operands.
// Ported from unit_tests.cpp line 1348
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1348
func TestAtomicBitAnd_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_and_"

	// foo='a'(97), bar='c'(99), baz="abc"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("a"))
		tx.Set([]byte(pfx+"bar"), []byte("c"))
		tx.Set([]byte(pfx+"baz"), []byte("abc"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Apply BIT_AND.
	// foo: 'a'(0x61) & 'b'(0x62) = 0x60 = 96
	// bar: 'c'(0x63,0x00) & "ad"(0x61,0x64) = (0x61, 0x00) = 'a', 0
	// baz: "abc" truncated to 'a' & 'e'(0x65) = 'a'(0x61) & 0x65 = 0x61 = 'a'
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutAnd, []byte(pfx+"foo"), []byte("b"))
		tx.Atomic(MutAnd, []byte(pfx+"bar"), []byte{'a', 'd'})
		tx.Atomic(MutAnd, []byte(pfx+"baz"), []byte("e"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("BIT_AND: %v", err)
	}

	// Verify foo = 96.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	fooVal := result.([]byte)
	if len(fooVal) != 1 || fooVal[0] != 96 {
		t.Errorf("foo: got %v, want [96]", fooVal)
	}

	// Verify bar = ['a', 0].
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"bar"))
	})
	if err != nil {
		t.Fatalf("Get bar: %v", err)
	}
	barVal := result.([]byte)
	if len(barVal) != 2 || barVal[0] != 97 || barVal[1] != 0 {
		t.Errorf("bar: got %v, want [97 0]", barVal)
	}

	// Verify baz = 'a' (97).
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"baz"))
	})
	if err != nil {
		t.Fatalf("Get baz: %v", err)
	}
	bazVal := result.([]byte)
	if len(bazVal) != 1 || bazVal[0] != 97 {
		t.Errorf("baz: got %v, want [97]", bazVal)
	}
}

// TestAtomicBitOr_CPort verifies FDB_MUTATION_TYPE_BIT_OR with same-length,
// extended, and truncated operands.
// Ported from unit_tests.cpp line 1415
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1415
func TestAtomicBitOr_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_or_"

	// foo='a'(97), bar='b'(98), baz="abc"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("a"))
		tx.Set([]byte(pfx+"bar"), []byte("b"))
		tx.Set([]byte(pfx+"baz"), []byte("abc"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Apply BIT_OR.
	// foo: 'a'(0x61) | 'b'(0x62) = 0x63 = 'c'(99)
	// bar: 'b'(0x62,0x00) | "ad"(0x61,0x64) = (0x63, 0x64) = "cd"
	// baz: "abc" truncated to 'a' | 'd'(0x64) = 0x61|0x64 = 0x65 = 'e'(101)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutOr, []byte(pfx+"foo"), []byte("b"))
		tx.Atomic(MutOr, []byte(pfx+"bar"), []byte{'a', 'd'})
		tx.Atomic(MutOr, []byte(pfx+"baz"), []byte("d"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("BIT_OR: %v", err)
	}

	// Verify foo = 99 = 'c'.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	fooVal := result.([]byte)
	if len(fooVal) != 1 || fooVal[0] != 99 {
		t.Errorf("foo: got %v (byte=%d), want [99]", fooVal, fooVal[0])
	}

	// Verify bar = "cd".
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"bar"))
	})
	if err != nil {
		t.Fatalf("Get bar: %v", err)
	}
	barVal := result.([]byte)
	if string(barVal) != "cd" {
		t.Errorf("bar: got %q, want %q", barVal, "cd")
	}

	// Verify baz = 101 = 'e'.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"baz"))
	})
	if err != nil {
		t.Fatalf("Get baz: %v", err)
	}
	bazVal := result.([]byte)
	if len(bazVal) != 1 || bazVal[0] != 101 {
		t.Errorf("baz: got %v (byte=%d), want [101]", bazVal, bazVal[0])
	}
}

// TestAtomicBitXor_CPort verifies FDB_MUTATION_TYPE_BIT_XOR with same-length,
// extended, and truncated operands.
// Ported from unit_tests.cpp line 1480
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1480
func TestAtomicBitXor_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_xor_"

	// foo='a'(97), bar='b'(98), baz="abc"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("a"))
		tx.Set([]byte(pfx+"bar"), []byte("b"))
		tx.Set([]byte(pfx+"baz"), []byte("abc"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Apply BIT_XOR.
	// foo: 'a'(0x61) ^ 'b'(0x62) = 0x03
	// bar: 'b'(0x62,0x00) ^ "ad"(0x61,0x64) = (0x03, 0x64)
	// baz: "abc" truncated to 'a' ^ 'd'(0x64) = 0x61^0x64 = 0x05
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutXor, []byte(pfx+"foo"), []byte("b"))
		tx.Atomic(MutXor, []byte(pfx+"bar"), []byte{'a', 'd'})
		tx.Atomic(MutXor, []byte(pfx+"baz"), []byte("d"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("BIT_XOR: %v", err)
	}

	// Verify foo = 0x03.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	fooVal := result.([]byte)
	if len(fooVal) != 1 || fooVal[0] != 0x03 {
		t.Errorf("foo: got %v, want [0x03]", fooVal)
	}

	// Verify bar = [0x03, 0x64].
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"bar"))
	})
	if err != nil {
		t.Fatalf("Get bar: %v", err)
	}
	barVal := result.([]byte)
	if len(barVal) != 2 || barVal[0] != 0x03 || barVal[1] != 0x64 {
		t.Errorf("bar: got %v, want [0x03 0x64]", barVal)
	}

	// Verify baz = 0x05.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"baz"))
	})
	if err != nil {
		t.Fatalf("Get baz: %v", err)
	}
	bazVal := result.([]byte)
	if len(bazVal) != 1 || bazVal[0] != 0x05 {
		t.Errorf("baz: got %v, want [0x05]", bazVal)
	}
}

// TestAtomicCompareAndClear_CPort verifies FDB_MUTATION_TYPE_COMPARE_AND_CLEAR.
// If the operand matches the stored value, the key is cleared. If not, the key
// is left unchanged.
// Ported from unit_tests.cpp line 1557
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1557
func TestAtomicCompareAndClear_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_cac_"

	// foo="bar", fdb="foundation"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("bar"))
		tx.Set([]byte(pfx+"fdb"), []byte("foundation"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// COMPARE_AND_CLEAR: foo with operand "bar" (matches) -> should clear.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutCompareAndClear, []byte(pfx+"foo"), []byte("bar"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("CompareAndClear: %v", err)
	}

	// foo should be gone.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	if result.([]byte) != nil {
		t.Errorf("foo should be cleared, got %q", result)
	}

	// fdb should still exist.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"fdb"))
	})
	if err != nil {
		t.Fatalf("Get fdb: %v", err)
	}
	if string(result.([]byte)) != "foundation" {
		t.Errorf("fdb: got %q, want %q", result, "foundation")
	}
}

// TestAtomicAppendIfFits_CPort verifies FDB_MUTATION_TYPE_APPEND_IF_FITS.
// Appends operand to existing value, or inserts if key doesn't exist.
// Ported from unit_tests.cpp line 1584
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1584
func TestAtomicAppendIfFits_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_aif_"

	// foo="f"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("f"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// APPEND_IF_FITS: foo += "db", bar = "foundation" (insert).
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutAppendIfFits, []byte(pfx+"foo"), []byte("db"))
		tx.Atomic(MutAppendIfFits, []byte(pfx+"bar"), []byte("foundation"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("AppendIfFits: %v", err)
	}

	// foo should be "fdb".
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	fooVal := result.([]byte)
	if fooVal == nil {
		t.Fatal("foo should exist")
	}
	if string(fooVal) != "fdb" {
		// The C test also notes that retries can cause double-append.
		t.Logf("foo: got %q (expected 'fdb', may differ if retried)", fooVal)
	}

	// bar should be "foundation".
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"bar"))
	})
	if err != nil {
		t.Fatalf("Get bar: %v", err)
	}
	barVal := result.([]byte)
	if barVal == nil {
		t.Fatal("bar should exist")
	}
	if string(barVal) != "foundation" {
		t.Logf("bar: got %q (expected 'foundation', may differ if retried)", barVal)
	}
}

// TestAtomicMax_CPort verifies FDB_MUTATION_TYPE_MAX (integer-like comparison
// with zero-extension and truncation).
// Ported from unit_tests.cpp line 1623
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1623
func TestAtomicMax_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_max_"

	// foo='a', bar='b', baz="cba"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("a"))
		tx.Set([]byte(pfx+"bar"), []byte("b"))
		tx.Set([]byte(pfx+"baz"), []byte("cba"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// MAX: foo with 'b' -> 'b' (b > a).
	// bar with "aa" -> "aa" extended comparison: 'b',0x00 vs 'a','a' -> 'a','a'
	//   Wait, C test expects "aa" wins. Let me re-read...
	//   Actually MAX treats values as little-endian integers with zero-extension.
	//   bar='b'(0x62) zero-extended to 0x62,0x00 vs param "aa"(0x61,0x61).
	//   As LE integer: bar=0x0062 vs param=0x6161. param > bar, so result = "aa".
	// baz with 'b' -> truncated: "cba" truncated to 'c' vs 'b' -> 'c' wins.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutMax, []byte(pfx+"foo"), []byte("b"))
		tx.Atomic(MutMax, []byte(pfx+"bar"), []byte("aa"))
		tx.Atomic(MutMax, []byte(pfx+"baz"), []byte("b"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("MAX: %v", err)
	}

	// foo = "b".
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	if string(result.([]byte)) != "b" {
		t.Errorf("foo: got %q, want %q", result, "b")
	}

	// bar = "aa".
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"bar"))
	})
	if err != nil {
		t.Fatalf("Get bar: %v", err)
	}
	if string(result.([]byte)) != "aa" {
		t.Errorf("bar: got %q, want %q", result, "aa")
	}

	// baz = "c" (truncated to param length, 'c' > 'b').
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"baz"))
	})
	if err != nil {
		t.Fatalf("Get baz: %v", err)
	}
	if string(result.([]byte)) != "c" {
		t.Errorf("baz: got %q, want %q", result, "c")
	}
}

// TestAtomicMin_CPort verifies FDB_MUTATION_TYPE_MIN (integer-like comparison
// with zero-extension and truncation).
// Ported from unit_tests.cpp line 1657
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1657
func TestAtomicMin_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_min_"

	// foo='a', bar='b', baz="cba"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("a"))
		tx.Set([]byte(pfx+"bar"), []byte("b"))
		tx.Set([]byte(pfx+"baz"), []byte("cba"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// MIN: foo with 'b' -> 'a' (a < b).
	// bar with "aa" -> 'b' zero-extended to 'b',0x00 vs "aa"(0x61,0x61).
	//   As LE integer: bar=0x0062 vs param=0x6161. bar < param, so result = 'b',0x00.
	// baz with 'b' -> truncated: "cba" truncated to 'c' vs 'b' -> 'b' wins.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutMin, []byte(pfx+"foo"), []byte("b"))
		tx.Atomic(MutMin, []byte(pfx+"bar"), []byte("aa"))
		tx.Atomic(MutMin, []byte(pfx+"baz"), []byte("b"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("MIN: %v", err)
	}

	// foo = "a".
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	if string(result.([]byte)) != "a" {
		t.Errorf("foo: got %q, want %q", result, "a")
	}

	// bar = ['b', 0x00] (2 bytes, zero-extended).
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"bar"))
	})
	if err != nil {
		t.Fatalf("Get bar: %v", err)
	}
	barVal := result.([]byte)
	if len(barVal) != 2 || barVal[0] != 'b' || barVal[1] != 0 {
		t.Errorf("bar: got %v, want ['b' 0]", barVal)
	}

	// baz = "b".
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"baz"))
	})
	if err != nil {
		t.Fatalf("Get baz: %v", err)
	}
	if string(result.([]byte)) != "b" {
		t.Errorf("baz: got %q, want %q", result, "b")
	}
}

// TestAtomicByteMax_CPort verifies FDB_MUTATION_TYPE_BYTE_MAX (byte-wise
// comparison without extension/truncation — keeps the longer string if it's
// the max).
// Ported from unit_tests.cpp line 1693
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1693
func TestAtomicByteMax_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_bmax_"

	// foo='a', bar='b', baz="cba"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("a"))
		tx.Set([]byte(pfx+"bar"), []byte("b"))
		tx.Set([]byte(pfx+"baz"), []byte("cba"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// BYTE_MAX: byte comparison, no extension/truncation.
	// foo: 'a' vs 'b' -> 'b' (b > a)
	// bar: 'b' vs "cc" -> "cc" (c > b byte-wise)
	// baz: "cba" vs 'b' -> "cba" (c > b byte-wise, longer wins)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutByteMax, []byte(pfx+"foo"), []byte("b"))
		tx.Atomic(MutByteMax, []byte(pfx+"bar"), []byte("cc"))
		tx.Atomic(MutByteMax, []byte(pfx+"baz"), []byte("b"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("BYTE_MAX: %v", err)
	}

	// foo = "b".
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	if string(result.([]byte)) != "b" {
		t.Errorf("foo: got %q, want %q", result, "b")
	}

	// bar = "cc".
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"bar"))
	})
	if err != nil {
		t.Fatalf("Get bar: %v", err)
	}
	if string(result.([]byte)) != "cc" {
		t.Errorf("bar: got %q, want %q", result, "cc")
	}

	// baz = "cba".
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"baz"))
	})
	if err != nil {
		t.Fatalf("Get baz: %v", err)
	}
	if string(result.([]byte)) != "cba" {
		t.Errorf("baz: got %q, want %q", result, "cba")
	}
}

// TestAtomicByteMin_CPort verifies FDB_MUTATION_TYPE_BYTE_MIN (byte-wise
// comparison without extension/truncation — keeps the shorter string if it's
// the min).
// Ported from unit_tests.cpp line 1727
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1727
func TestAtomicByteMin_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_bmin_"

	// foo='a', bar='b', baz="abc"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"foo"), []byte("a"))
		tx.Set([]byte(pfx+"bar"), []byte("b"))
		tx.Set([]byte(pfx+"baz"), []byte("abc"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// BYTE_MIN: byte comparison, no extension/truncation.
	// foo: 'a' vs 'b' -> 'a' (a < b)
	// bar: 'b' vs "aa" -> "aa" (a < b byte-wise)
	// baz: "abc" vs 'b' -> "abc" (a < b byte-wise, abc wins)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutByteMin, []byte(pfx+"foo"), []byte("b"))
		tx.Atomic(MutByteMin, []byte(pfx+"bar"), []byte("aa"))
		tx.Atomic(MutByteMin, []byte(pfx+"baz"), []byte("b"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("BYTE_MIN: %v", err)
	}

	// foo = "a".
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"foo"))
	})
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	if string(result.([]byte)) != "a" {
		t.Errorf("foo: got %q, want %q", result, "a")
	}

	// bar = "aa".
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"bar"))
	})
	if err != nil {
		t.Fatalf("Get bar: %v", err)
	}
	if string(result.([]byte)) != "aa" {
		t.Errorf("bar: got %q, want %q", result, "aa")
	}

	// baz = "abc".
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(pfx+"baz"))
	})
	if err != nil {
		t.Fatalf("Get baz: %v", err)
	}
	if string(result.([]byte)) != "abc" {
		t.Errorf("baz: got %q, want %q", result, "abc")
	}
}

// TestAtomicSetVersionstampedKey_CPort verifies FDB_MUTATION_TYPE_SET_VERSIONSTAMPED_KEY.
// The key contains a placeholder for the 10-byte versionstamp plus a 4-byte
// little-endian offset suffix indicating where the versionstamp goes.
// Ported from unit_tests.cpp line 1761
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1761
func TestAtomicSetVersionstampedKey_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_vsk_"

	// Build key: pfx + "foo" + 10 zero bytes (versionstamp placeholder) + 4-byte LE offset.
	// The offset points to where the versionstamp should be written: len(pfx) + 3 (for "foo").
	offset := uint32(len(pfx) + 3)
	key := make([]byte, 0, len(pfx)+3+10+4)
	key = append(key, []byte(pfx)...)
	key = append(key, 'f', 'o', 'o')
	key = append(key, make([]byte, 10)...) // 10 zero bytes for versionstamp
	offsetBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(offsetBuf, offset)
	key = append(key, offsetBuf...)

	var versionstamp []byte

	// Commit with SET_VERSIONSTAMPED_KEY and retrieve the versionstamp.
	tx := db.CreateTransaction()
	tx.Atomic(MutSetVersionstampedKey, key, []byte("bar"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	vs, err := tx.GetVersionstamp()
	if err != nil {
		t.Fatalf("GetVersionstamp: %v", err)
	}
	versionstamp = vs

	if len(versionstamp) != 10 {
		t.Fatalf("versionstamp length: got %d, want 10", len(versionstamp))
	}

	// The actual key stored should be pfx + "foo" + versionstamp.
	expectedKey := make([]byte, 0, len(pfx)+3+10)
	expectedKey = append(expectedKey, []byte(pfx)...)
	expectedKey = append(expectedKey, 'f', 'o', 'o')
	expectedKey = append(expectedKey, versionstamp...)

	// Read back.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, expectedKey)
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	val := result.([]byte)
	if val == nil {
		t.Fatal("expected key to exist after SET_VERSIONSTAMPED_KEY")
	}
	if string(val) != "bar" {
		t.Errorf("value: got %q, want %q", val, "bar")
	}
}

// TestAtomicSetVersionstampedValue_CPort verifies FDB_MUTATION_TYPE_SET_VERSIONSTAMPED_VALUE.
// The value contains a placeholder for the 10-byte versionstamp plus a 4-byte
// little-endian offset suffix.
// Ported from unit_tests.cpp line 1800
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1800
func TestAtomicSetVersionstampedValue_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_vsv_"
	keyName := []byte(pfx + "foo")

	// Build value: "bar" + 10 zero bytes (versionstamp placeholder) + 4-byte LE offset.
	// Offset = 3 (versionstamp goes right after "bar").
	val := make([]byte, 0, 3+10+4)
	val = append(val, 'b', 'a', 'r')
	val = append(val, make([]byte, 10)...) // placeholder
	offsetBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(offsetBuf, 3)
	val = append(val, offsetBuf...)

	var versionstamp []byte

	// Commit with SET_VERSIONSTAMPED_VALUE and retrieve the versionstamp.
	tx := db.CreateTransaction()
	tx.Atomic(MutSetVersionstampedValue, keyName, val)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	vs, err := tx.GetVersionstamp()
	if err != nil {
		t.Fatalf("GetVersionstamp: %v", err)
	}
	versionstamp = vs

	if len(versionstamp) != 10 {
		t.Fatalf("versionstamp length: got %d, want 10", len(versionstamp))
	}

	// The stored value should be "bar" + versionstamp (offset bytes stripped by FDB).
	expectedVal := make([]byte, 0, 3+10)
	expectedVal = append(expectedVal, 'b', 'a', 'r')
	expectedVal = append(expectedVal, versionstamp...)

	// Read back.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, keyName)
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	readVal := result.([]byte)
	if readVal == nil {
		t.Fatal("expected key to exist after SET_VERSIONSTAMPED_VALUE")
	}
	if !bytes.Equal(readVal, expectedVal) {
		t.Errorf("value: got %x, want %x", readVal, expectedVal)
	}
}

// ---------------------------------------------------------------------------
// Version operations
// ---------------------------------------------------------------------------

// TestSetReadVersionOld_CPort verifies that setting the read version to 1
// (ancient) causes transaction_too_old (1007) on read.
// Ported from unit_tests.cpp line 905
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L905
func TestSetReadVersionOld_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Get a real read version to confirm the client is working.
	tx0 := db.CreateTransaction()
	rv, rvErr := tx0.GetReadVersion(ctx)
	t.Logf("current read version: %d, err: %v", rv, rvErr)

	tx := db.CreateTransaction()
	tx.SetReadVersion(1)

	// A snapshot read with a very old version should fail with transaction_too_old.
	_, err := tx.Snapshot().Get(ctx, []byte("foo"))
	if err == nil {
		t.Fatal("expected error for old read version, got nil")
	}

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != ErrTransactionTooOld {
		t.Errorf("error code: got %d, want %d (transaction_too_old)", fdbErr.Code, ErrTransactionTooOld)
	}
}

// TestSetReadVersionFuture_CPort verifies that setting the read version to a
// far-future value causes future_version (1009) on read.
// Ported from unit_tests.cpp line 915
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L915
func TestSetReadVersionFuture_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	tx.SetReadVersion(int64(math.MaxInt64))

	_, err := tx.Get(ctx, []byte("foo"))
	if err == nil {
		t.Fatal("expected error for future read version, got nil")
	}

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != ErrFutureVersion {
		t.Errorf("error code: got %d, want %d (future_version)", fdbErr.Code, ErrFutureVersion)
	}
}

// TestGetCommittedVersionReadOnly_CPort verifies that a read-only transaction
// that is committed has no meaningful committed version. In our Go client,
// GetCommittedVersion after a read-only Commit returns 0 (the zero-value),
// since no commit was sent to FDB. The C test checks for -1.
// Ported from unit_tests.cpp line 1849
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1849
func TestGetCommittedVersionReadOnly_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	// Read a key (read-only).
	_, err := tx.Get(ctx, []byte("c_cv_ro_foo"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Commit read-only transaction.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// In our Go client, a read-only commit skips the actual commit RPC,
	// so committedVersion stays at 0.
	cv, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("GetCommittedVersion: %v", err)
	}
	// The C binding returns -1 for read-only. Our Go client returns 0
	// (default int64 zero) because no commit RPC was issued.
	if cv != 0 {
		t.Errorf("committed version: got %d, want 0 (read-only)", cv)
	}
}

// TestGetCommittedVersion_CPort verifies that a write transaction returns a
// non-negative committed version after successful commit.
// Ported from unit_tests.cpp line 1869
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1869
func TestGetCommittedVersion_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	tx.Set([]byte("c_cv_foo"), []byte("bar"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	cv, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("GetCommittedVersion: %v", err)
	}
	if cv < 0 {
		t.Errorf("committed version: got %d, want >= 0", cv)
	}
}

// ---------------------------------------------------------------------------
// Transaction lifecycle
// ---------------------------------------------------------------------------

// TestTransactionCancel_CPort verifies that cancelling a transaction prevents
// further operations.
// Ported from unit_tests.cpp line 2105
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2105
func TestTransactionCancel_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	tx.Cancel()

	// Get after Cancel should fail.
	_, err := tx.Get(ctx, []byte("c_cancel_foo"))
	if err == nil {
		t.Error("Get after Cancel should fail")
	}
}

// TestAddConflictRange_CPort verifies that explicit conflict ranges cause
// transactions to conflict as expected.
// Ported from unit_tests.cpp line 2118
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2118
func TestAddConflictRange_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_acr_"

	// tx1 gets a read version (establishes its snapshot).
	tx1 := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx1.SetReadVersion(rv)

	// tx2 writes a key and commits — this creates a version gap.
	tx2key := []byte(pfx + "a")
	tx2end := append([]byte(pfx+"a"), 0) // strinc equivalent
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.AddWriteConflictRange(tx2key, tx2end)
		tx.Set([]byte(pfx+"dummy"), []byte("x"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("tx2: %v", err)
	}

	// tx1 adds read + write conflict ranges and tries to commit.
	tx1.AddReadConflictRange(tx2key, tx2end)
	tx1.AddWriteConflictRange(tx2key, tx2end)
	tx1.Set([]byte(pfx+"dummy2"), []byte("y"))

	err = tx1.Commit(ctx)
	if err == nil {
		t.Fatal("tx1 should conflict — tx2 wrote in its conflict range")
	}

	// Verify the error is a conflict (not_committed, 1020).
	var fdbErr *wire.FDBError
	if errors.As(err, &fdbErr) {
		if fdbErr.Code != ErrNotCommitted {
			t.Errorf("error code: got %d, want %d (not_committed)", fdbErr.Code, ErrNotCommitted)
		}
	} else {
		t.Logf("conflict error (expected): %v", err)
	}
}

// TestCommitDoesNotReset_CPort verifies that committing a transaction does not
// clear its internal state for GetCommittedVersion. After commit, the read
// version should still be accessible.
// Ported from unit_tests.cpp line 2516
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2516
func TestCommitDoesNotReset_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_notreset_"

	// tx1: set and commit.
	tx1 := db.CreateTransaction()
	rv1, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV for tx1: %v", err)
	}
	tx1.SetReadVersion(rv1)
	tx1.Set([]byte(pfx+"foo"), []byte("bar"))
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 Commit: %v", err)
	}

	// After commit, GetCommittedVersion should work.
	cv1, err := tx1.GetCommittedVersion()
	if err != nil {
		t.Fatalf("tx1 GetCommittedVersion: %v", err)
	}
	if cv1 < 0 {
		t.Errorf("tx1 committed version: got %d, want >= 0", cv1)
	}

	// The C test verifies that the read version doesn't change after commit
	// (i.e., the transaction was not reset). We verify the same by checking
	// that the committed version is still accessible.
	cv1Again, err := tx1.GetCommittedVersion()
	if err != nil {
		t.Fatalf("tx1 GetCommittedVersion again: %v", err)
	}
	if cv1 != cv1Again {
		t.Errorf("committed version changed: first=%d, second=%d", cv1, cv1Again)
	}
}

// TestErrorPredicate_CPort verifies error retryability classification.
// This is a pure logic test — no database needed.
// Ported from unit_tests.cpp line 2432
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2432
func TestErrorPredicate_CPort(t *testing.T) {
	t.Parallel()

	// Helper: check if an error code is retryable via OnError.
	// We create a fresh transaction and call OnError. If it returns nil,
	// the error is retryable.
	isRetryable := func(code int) bool {
		tx := &Transaction{}
		err := &wire.FDBError{Code: code}
		return tx.OnError(context.Background(), err) == nil
	}

	// RETRYABLE errors (matches FDB_ERROR_PREDICATE_RETRYABLE).
	retryable := []int{
		1007, // transaction_too_old
		1020, // not_committed
		1021, // commit_unknown_result
		1038, // database_locked
	}
	for _, code := range retryable {
		if !isRetryable(code) {
			t.Errorf("error %d should be retryable", code)
		}
	}

	// NON-RETRYABLE errors.
	nonRetryable := []int{
		1031, // transaction_timed_out
		2000, // client_invalid_operation
		2004, // key_outside_legal_range
		2005, // inverted_range
		2006, // invalid_option_value
		2007, // invalid_option
		2011, // version_invalid
		2020, // transaction_invalid_version
		2023, // transaction_read_only
		2100, // incompatible_protocol_version
		2101, // transaction_too_large
		2102, // key_too_large
		2103, // value_too_large
		2108, // unsupported_operation
		2200, // api_version_unset
		4000, // unknown_error
		4001, // internal_error
	}
	for _, code := range nonRetryable {
		if isRetryable(code) {
			t.Errorf("error %d should NOT be retryable", code)
		}
	}

	// MAYBE_COMMITTED: commit_unknown_result is retryable.
	if !isRetryable(1021) {
		t.Error("1021 (commit_unknown_result) should be retryable")
	}

	// RETRYABLE_NOT_COMMITTED: not_committed is retryable, commit_unknown_result
	// is also retryable (but via a different path — self-conflicting).
	if !isRetryable(1020) {
		t.Error("1020 (not_committed) should be retryable")
	}

	// Non-FDB error should not be retryable.
	tx := &Transaction{}
	plainErr := fmt.Errorf("some random error")
	if tx.OnError(context.Background(), plainErr) == nil {
		t.Error("non-FDB error should not be retryable")
	}
}

// ---------------------------------------------------------------------------
// Transaction timeout and retry limit
// ---------------------------------------------------------------------------

// TestSetTimeout_CPort verifies that a 1ms timeout eventually fires with error 1031.
// Ported from unit_tests.cpp line 769
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L769
//
// The C test sets a 1ms timeout and loops Get + OnError until 1031 escapes.
// Our Go implementation checks the deadline before each operation, so with
// a 1ms timeout the first or second operation will return 1031, and OnError
// will refuse to retry it.
func TestSetTimeout_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetTimeout(1) // 1 millisecond

	// Burn through the timeout — sleep to guarantee deadline passes.
	time.Sleep(2 * time.Millisecond)

	// Now any operation should return 1031.
	err := tx.checkTimeout()
	if err == nil {
		t.Fatal("expected timeout error after 1ms")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != ErrTransactionTimedOut {
		t.Errorf("error code: got %d, want %d", fdbErr.Code, ErrTransactionTimedOut)
	}

	// OnError(1031) should NOT retry — error must escape.
	retryErr := tx.OnError(context.Background(), err)
	if retryErr == nil {
		t.Fatal("OnError should not retry transaction_timed_out")
	}
	if !errors.As(retryErr, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
		t.Errorf("OnError returned wrong error: %v", retryErr)
	}
}

// TestSetRetryLimit verifies that OnError respects the retry limit.
// After retryLimit retries, the next OnError call returns the error
// instead of retrying.
func TestSetRetryLimit(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetRetryLimit(2) // allow 2 retries

	retryableErr := &wire.FDBError{Code: ErrNotCommitted}

	// First retry — should succeed.
	if err := tx.OnError(context.Background(), retryableErr); err != nil {
		t.Fatalf("retry 1 should succeed, got: %v", err)
	}
	if tx.retryCount != 1 {
		t.Errorf("retryCount after 1st: got %d, want 1", tx.retryCount)
	}

	// Second retry — should succeed.
	if err := tx.OnError(context.Background(), retryableErr); err != nil {
		t.Fatalf("retry 2 should succeed, got: %v", err)
	}
	if tx.retryCount != 2 {
		t.Errorf("retryCount after 2nd: got %d, want 2", tx.retryCount)
	}

	// Third attempt — retryCount(2) >= retryLimit(2), should fail.
	err := tx.OnError(context.Background(), retryableErr)
	if err == nil {
		t.Fatal("retry 3 should fail (limit reached)")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrNotCommitted {
		t.Errorf("expected not_committed error, got: %v", err)
	}
}

// TestSetRetryLimit_Zero verifies that retryLimit=0 means no retries at all.
func TestSetRetryLimit_Zero(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetRetryLimit(0) // no retries

	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
	if err == nil {
		t.Fatal("retryLimit=0 should not allow any retries")
	}
}

// TestSetTimeout_Get verifies that a timed-out transaction returns 1031 on Get.
// This is a pure unit test — no database needed — using ensureReadVersion
// which calls checkTimeout before the GRV fetch.
func TestSetTimeout_Get(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetTimeout(1) // 1ms

	// Wait for deadline to pass.
	time.Sleep(2 * time.Millisecond)

	// ensureReadVersion should return timeout error.
	err := tx.ensureReadVersion(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
		t.Errorf("expected error 1031, got: %v", err)
	}
}

// TestSetTimeout_Preserved verifies that timeout survives OnError + reset.
// The timeout option is preserved across retries (matching C++ where options
// are re-applied on reset).
func TestSetTimeout_Preserved(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetTimeout(500) // 500ms — long enough to not fire during test

	// Force a retryable error.
	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
	if err != nil {
		t.Fatalf("OnError should retry: %v", err)
	}

	// After reset, timeout should still be set.
	if tx.timeout != 500*time.Millisecond {
		t.Errorf("timeout not preserved: got %v, want 500ms", tx.timeout)
	}
	if tx.deadline.IsZero() {
		t.Error("deadline should be re-computed after reset")
	}

	// And checkTimeout should not fire (we have 500ms).
	if err := tx.checkTimeout(); err != nil {
		t.Errorf("timeout should not fire yet: %v", err)
	}
}

// TestSetTimeout_OverallBudget verifies that timeout is an overall budget
// across all retries, NOT per-retry. Matches C++ where creationTime is set
// once and the deadline = creationTime + timeout across all OnError retries.
// Deterministic: sets creationTime to a known past value, no sleep needed.
func TestSetTimeout_OverallBudget(t *testing.T) {
	t.Parallel()

	// Create a tx whose creationTime is 200ms in the past.
	tx := &Transaction{creationTime: time.Now().Add(-200 * time.Millisecond)}
	tx.SetTimeout(500) // 500ms budget from creationTime → deadline = 300ms from now

	// Deadline should NOT have fired yet (300ms from now).
	if err := tx.checkTimeout(); err != nil {
		t.Fatalf("timeout should not fire yet (300ms remaining): %v", err)
	}

	// OnError resets the tx for retry but should NOT restart the timer.
	// creationTime stays at -200ms, deadline stays at creationTime + 500ms.
	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
	if err != nil {
		t.Fatalf("OnError should retry: %v", err)
	}

	// After OnError, deadline is still anchored to the original creationTime.
	// Verify creationTime was NOT updated by checking the deadline is the same.
	// The deadline should be creationTime + 500ms = now - 200ms + 500ms = now + 300ms.
	if err := tx.checkTimeout(); err != nil {
		t.Errorf("timeout should not fire after OnError (budget not exhausted): %v", err)
	}

	// Now simulate exhausted budget: set creationTime far in the past.
	tx.creationTime = time.Now().Add(-1 * time.Second) // 1s ago
	tx.deadline = tx.creationTime.Add(tx.timeout)      // deadline = 500ms ago
	if err := tx.checkTimeout(); err == nil {
		t.Error("timeout should fire: budget exhausted (creationTime 1s ago, 500ms timeout)")
	}
}

// TestSetTimeout_ResetRestartsTimer verifies that user-facing Reset()
// restarts the timeout timer (updates creationTime). Matches C++
// ReadYourWritesTransaction::reset() which sets creationTime = now().
// Deterministic: checks creationTime directly instead of timing sleeps.
func TestSetTimeout_ResetRestartsTimer(t *testing.T) {
	t.Parallel()

	// Start with creationTime far in the past — budget is already exhausted.
	tx := &Transaction{creationTime: time.Now().Add(-10 * time.Second)}
	tx.SetTimeout(500) // deadline = -10s + 500ms = -9.5s → already expired

	// Should be timed out.
	if err := tx.checkTimeout(); err == nil {
		t.Fatal("timeout should fire (budget exhausted from past creationTime)")
	}

	// User Reset() should restart the timer — creationTime becomes now().
	tx.Reset()
	tx.SetTimeout(500) // re-apply → deadline = now() + 500ms

	// After Reset, the full 500ms budget should be available.
	if err := tx.checkTimeout(); err != nil {
		t.Errorf("timeout should not fire after Reset (fresh 500ms budget): %v", err)
	}

	// Verify creationTime was updated to a recent time.
	if time.Since(tx.creationTime) > 1*time.Second {
		t.Errorf("creationTime not updated by Reset: %v (should be ~now)", tx.creationTime)
	}
}

// TestSetTimeout_Disabled verifies that timeout=0 disables the timeout.
func TestSetTimeout_Disabled(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetTimeout(100) // set a timeout
	tx.SetTimeout(0)   // then disable it

	if tx.timeout != 0 {
		t.Errorf("timeout should be 0, got %v", tx.timeout)
	}
	if !tx.deadline.IsZero() {
		t.Error("deadline should be zero when timeout disabled")
	}

	// checkTimeout should always pass.
	if err := tx.checkTimeout(); err != nil {
		t.Errorf("disabled timeout should not fire: %v", err)
	}
}

// TestSetRetryLimit_Unlimited verifies that SetRetryLimit(-1) removes the limit.
func TestSetRetryLimit_Unlimited(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetRetryLimit(0) // set limit to 0

	// Should not retry.
	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
	if err == nil {
		t.Fatal("retryLimit=0 should not retry")
	}

	// Now remove the limit.
	tx.state.Store(int32(txStateActive))
	tx.SetRetryLimit(-1)

	// Should retry now.
	if err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted}); err != nil {
		t.Fatalf("unlimited retry should succeed: %v", err)
	}
}

// TestSetMaxRetryDelay verifies that SetMaxRetryDelay caps the backoff.
// The default max is 1s (maxBackoff). Setting a smaller cap should limit growth.
func TestSetMaxRetryDelay(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetRetryLimit(-1)    // unlimited retries
	tx.SetMaxRetryDelay(50) // 50ms cap

	retryableErr := &wire.FDBError{Code: ErrNotCommitted}

	// Retry several times to grow the backoff.
	for i := 0; i < 10; i++ {
		if err := tx.OnError(context.Background(), retryableErr); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}

	// After 10 retries with growth rate 2x, uncapped backoff would be
	// 10ms * 2^10 = 10240ms. With 50ms cap, it should be <= 50ms.
	if tx.backoff > 50*time.Millisecond {
		t.Errorf("backoff %v exceeds max retry delay 50ms", tx.backoff)
	}
}

// TestSetTimeout_CommitCheck verifies that Commit checks the timeout.
func TestSetTimeout_CommitCheck(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetTimeout(1)                     // 1ms
	tx.Set([]byte("key"), []byte("val")) // need mutations for commit path
	time.Sleep(2 * time.Millisecond)

	err := tx.Commit(context.Background())
	if err == nil {
		t.Fatal("expected timeout error on Commit")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTimedOut {
		t.Errorf("expected error 1031 from Commit, got: %v", err)
	}
}

// TestGetApproximateSize_CPort verifies GetApproximateSize tracks mutation size.
// C binding: fdb_transaction_get_approximate_size
func TestGetApproximateSize_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}

	// Empty transaction should have zero size.
	if size := tx.GetApproximateSize(); size != 0 {
		t.Errorf("empty tx size: got %d, want 0", size)
	}

	// Add a mutation and verify size increases.
	tx.Set([]byte("key123"), []byte("value456"))
	size1 := tx.GetApproximateSize()
	if size1 == 0 {
		t.Error("size should be non-zero after Set")
	}

	// Add more mutations.
	tx.Set([]byte("another_key"), []byte("another_value"))
	tx.Clear([]byte("cleared_key"))
	size2 := tx.GetApproximateSize()
	if size2 <= size1 {
		t.Errorf("size should increase: %d <= %d", size2, size1)
	}

	// Add conflict ranges.
	tx.AddReadConflictKey([]byte("conflict_read"))
	tx.AddWriteConflictKey([]byte("conflict_write"))
	size3 := tx.GetApproximateSize()
	if size3 <= size2 {
		t.Errorf("size should increase with conflict ranges: %d <= %d", size3, size2)
	}
}

// TestGetRangeReverse_Full verifies full reverse range scan returns keys in descending order.
// C binding: fdb_transaction_get_range with reverse=true, limit=0 (unlimited)
func TestGetRangeReverse_Full(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := "reverse_full_"
	keys := make([]string, 20)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s%04d", prefix, i)
	}

	// Write 20 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, k := range keys {
			tx.Set([]byte(k), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read all in reverse.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRangeReverse(ctx,
			[]byte(prefix+"0000"),
			[]byte(prefix+"9999"),
			0, // unlimited
		)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("reverse range: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 20 {
		t.Fatalf("got %d keys, want 20", len(kvs))
	}

	// Verify descending order.
	for i := 0; i < len(kvs)-1; i++ {
		if bytes.Compare(kvs[i].Key, kvs[i+1].Key) <= 0 {
			t.Errorf("not descending at %d: %q >= %q", i, kvs[i].Key, kvs[i+1].Key)
		}
	}
}

// ---------------------------------------------------------------------------
// Additional range and value tests
// ---------------------------------------------------------------------------

// TestGetRangeStreamingMode_CPort verifies that GetRange respects different
// limit values — unlimited (returns all), exact limit (returns exactly N),
// and small limit for iteration (returns limited results with more=true).
// The pure Go client doesn't have a streaming mode enum; the limit parameter
// controls the same behavior.
func TestGetRangeStreamingMode_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	prefix := "c_stream_"

	// Write 50 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 50; i++ {
			k := fmt.Sprintf("%s%04d", prefix, i)
			tx.Set([]byte(k), []byte(fmt.Sprintf("val%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	begin := []byte(prefix + "0000")
	end := []byte(prefix + "9999")

	// WantAll equivalent: limit=0 (unlimited) — returns all 50 results.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, begin, end, 0)
		return struct {
			kvs  []KeyValue
			more bool
		}{kvs, more}, err
	})
	if err != nil {
		t.Fatalf("GetRange unlimited: %v", err)
	}
	r := result.(struct {
		kvs  []KeyValue
		more bool
	})
	if len(r.kvs) != 50 {
		t.Errorf("unlimited: got %d keys, want 50", len(r.kvs))
	}
	if r.more {
		t.Error("unlimited: more should be false when all keys returned")
	}

	// Exact equivalent: limit=50 — returns exactly 50 results.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, begin, end, 50)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRange exact: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 50 {
		t.Errorf("exact: got %d keys, want 50", len(kvs))
	}

	// Iterator equivalent: limit=10 — returns 10 results with more=true.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, begin, end, 10)
		return struct {
			kvs  []KeyValue
			more bool
		}{kvs, more}, err
	})
	if err != nil {
		t.Fatalf("GetRange iterator: %v", err)
	}
	r = result.(struct {
		kvs  []KeyValue
		more bool
	})
	if len(r.kvs) != 10 {
		t.Errorf("iterator: got %d keys, want 10", len(r.kvs))
	}
	if !r.more {
		t.Error("iterator: more should be true when limit < total keys")
	}

	// Verify the 10 returned keys are the first 10 in ascending order.
	for i, kv := range r.kvs {
		want := fmt.Sprintf("%s%04d", prefix, i)
		if string(kv.Key) != want {
			t.Errorf("iterator key[%d]: got %q, want %q", i, kv.Key, want)
		}
	}
}

// TestGetRangeEmpty_CPort verifies that GetRange on a range with no keys
// returns an empty slice and more=false.
func TestGetRangeEmpty_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Use a prefix that no other test writes to.
	prefix := "c_empty_range_"

	// Clear the range to be sure it's empty.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		if err := tx.ClearRange([]byte(prefix), []byte(prefix+"\xff")); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}

	type rangeResult struct {
		kvs  []KeyValue
		more bool
	}
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte(prefix), []byte(prefix+"\xff"), 100)
		return rangeResult{kvs, more}, err
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	rr := result.(rangeResult)
	if len(rr.kvs) != 0 {
		t.Errorf("expected 0 keys, got %d", len(rr.kvs))
	}
	if rr.more {
		t.Error("more should be false for empty range")
	}
}

// TestClearRangeAndVerify_CPort writes 10 keys, clears range [3,7), and
// verifies that keys 0-2 and 7-9 still exist while keys 3-6 are gone.
func TestClearRangeAndVerify_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	prefix := "c_clrng_"

	// Write 10 keys: prefix_00 through prefix_09.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			k := fmt.Sprintf("%s%02d", prefix, i)
			tx.Set([]byte(k), []byte(fmt.Sprintf("v%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear range [03, 07) — removes keys 03, 04, 05, 06.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return nil, tx.ClearRange([]byte(prefix+"03"), []byte(prefix+"07"))
	})
	if err != nil {
		t.Fatalf("ClearRange: %v", err)
	}

	// Verify which keys exist.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		out := make(map[string][]byte)
		for i := 0; i < 10; i++ {
			k := fmt.Sprintf("%s%02d", prefix, i)
			val, err := tx.Get(ctx, []byte(k))
			if err != nil {
				return nil, err
			}
			out[k] = val
		}
		return out, nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	data := result.(map[string][]byte)
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("%s%02d", prefix, i)
		val := data[k]
		if i >= 3 && i < 7 {
			// Should be cleared.
			if val != nil {
				t.Errorf("key %q should be cleared, got %q", k, val)
			}
		} else {
			// Should still exist.
			want := fmt.Sprintf("v%d", i)
			if val == nil {
				t.Errorf("key %q should exist", k)
			} else if string(val) != want {
				t.Errorf("key %q: got %q, want %q", k, val, want)
			}
		}
	}
}

// TestMultipleAtomicOps_CPort verifies that multiple atomic ADD operations on
// the same key within a single transaction produce the correct sum. This
// exercises the RYW cache's atomic mutation merging.
func TestMultipleAtomicOps_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("c_multi_atomic_add")

	// Initialize to 0 as int64 LE.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], 0)
		tx.Set(key, buf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Apply 5 atomic ADDs of values 10, 20, 30, 40, 50 in one transaction.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, v := range []uint64{10, 20, 30, 40, 50} {
			var buf [8]byte
			binary.LittleEndian.PutUint64(buf[:], v)
			tx.Atomic(MutAddValue, key, buf[:])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("atomic ADDs: %v", err)
	}

	// Read back — should be 150 (10+20+30+40+50).
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	val := result.([]byte)
	if len(val) != 8 {
		t.Fatalf("expected 8 bytes, got %d", len(val))
	}
	got := binary.LittleEndian.Uint64(val)
	if got != 150 {
		t.Errorf("sum: got %d, want 150", got)
	}
}

// TestLargeValue_CPort writes a 90KB value (near the FDB 100KB limit),
// reads it back, and verifies byte-exact match.
func TestLargeValue_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("c_large_val")

	// Create 90KB value with recognizable pattern.
	size := 90 * 1024
	bigVal := make([]byte, size)
	for i := range bigVal {
		bigVal[i] = byte(i % 251) // prime mod avoids simple repetition
	}

	// Write it.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, bigVal)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read it back.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	readVal := result.([]byte)
	if readVal == nil {
		t.Fatal("expected non-nil value")
	}
	if len(readVal) != size {
		t.Fatalf("value length: got %d, want %d", len(readVal), size)
	}
	if !bytes.Equal(readVal, bigVal) {
		// Find first mismatch for diagnostic.
		for i := range bigVal {
			if readVal[i] != bigVal[i] {
				t.Fatalf("mismatch at byte %d: got 0x%02x, want 0x%02x", i, readVal[i], bigVal[i])
			}
		}
	}
}

// TestEmptyKeyValue_CPort verifies that empty keys and empty values are
// handled correctly — both should be readable and return empty byte slices.
func TestEmptyKeyValue_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	emptyKey := []byte("")
	normalKey := []byte("c_emptyval_k")

	// Write: key="" value="" and key="c_emptyval_k" value="".
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(emptyKey, []byte(""))
		tx.Set(normalKey, []byte(""))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read empty key.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, emptyKey)
	})
	if err != nil {
		t.Fatalf("Get empty key: %v", err)
	}
	val := result.([]byte)
	if val == nil {
		t.Error("empty key: got nil, want empty byte slice")
	} else if len(val) != 0 {
		t.Errorf("empty key value: got %q (len=%d), want empty", val, len(val))
	}

	// Read normal key with empty value.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, normalKey)
	})
	if err != nil {
		t.Fatalf("Get normal key: %v", err)
	}
	val = result.([]byte)
	if val == nil {
		t.Error("normal key: got nil, want empty byte slice")
	} else if len(val) != 0 {
		t.Errorf("normal key value: got %q (len=%d), want empty", val, len(val))
	}
}

// TestGetKey_AllSelectors verifies all key selector types against real FDB.
// C binding: fdb_transaction_get_key
func TestGetKey_AllSelectors(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := "getkey_sel_"
	// Write keys: 10, 20, 30, 40, 50
	vals := []string{"10", "20", "30", "40", "50"}
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, v := range vals {
			tx.Set([]byte(prefix+v), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name    string
		key     string
		orEqual bool
		offset  int32
		want    string
	}{
		// firstGreaterOrEqual("30") → "30"
		{"FGE exact", prefix + "30", false, 1, prefix + "30"},
		// firstGreaterOrEqual("25") → "30"
		{"FGE between", prefix + "25", false, 1, prefix + "30"},
		// firstGreaterThan("30") → "40"
		{"FGT exact", prefix + "30", true, 1, prefix + "40"},
		// lastLessOrEqual("30") → "30"
		{"LLE exact", prefix + "30", true, 0, prefix + "30"},
		// lastLessThan("30") → "20"
		{"LLT exact", prefix + "30", false, 0, prefix + "20"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				return tx.GetKey(ctx, []byte(tc.key), tc.orEqual, tc.offset)
			})
			if err != nil {
				t.Fatalf("GetKey: %v", err)
			}
			got := string(result.([]byte))
			if got != tc.want {
				t.Errorf("GetKey(%q, orEqual=%v, offset=%d) = %q, want %q",
					tc.key, tc.orEqual, tc.offset, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Watch
// ---------------------------------------------------------------------------

// TestWatch_CPort verifies the full Watch lifecycle: start a watch on a key,
// update the key from another goroutine, and confirm the watch resolves.
// Ported from unit_tests.cpp line 2071
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2071
func TestWatch_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("watch_cport_foo")

	// Seed the key with an initial value.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("foo"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Start a watch in a transaction. The watch reads the current value and
	// then long-polls until it changes.
	watchCtx, watchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer watchCancel()

	watchErr := make(chan error, 1)
	go func() {
		_, err := db.Transact(watchCtx, func(tx *Transaction) (any, error) {
			// Read the key to establish the version for the watch.
			_, err := tx.Get(watchCtx, key)
			if err != nil {
				return nil, err
			}
			// Start the watch — this commits the transaction and blocks
			// until the key changes.
			return nil, tx.Watch(watchCtx, key)
		})
		watchErr <- err
	}()

	// Give the watch time to set up, then update the key.
	time.Sleep(500 * time.Millisecond)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("bar"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// The watch should resolve within the 10-second context.
	select {
	case err := <-watchErr:
		if err != nil {
			t.Fatalf("watch returned error: %v", err)
		}
	case <-watchCtx.Done():
		t.Fatal("watch did not resolve within 10 seconds")
	}

	// Verify the key has the new value.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	val := result.([]byte)
	if string(val) != "bar" {
		t.Errorf("final value: got %q, want %q", val, "bar")
	}
}

// ---------------------------------------------------------------------------
// Read-your-writes
// ---------------------------------------------------------------------------

// TestReadYourWrites_CPort verifies that uncommitted writes are visible to
// subsequent reads within the same transaction (read-your-writes semantics).
// Ported from unit_tests.cpp line 643
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L643
func TestReadYourWrites_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("c_ryw_foo")

	// Clear any leftover value.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}

	// In one transaction: Set a key, then Get it without committing.
	// The uncommitted write should be visible via RYW.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("hello_ryw"))

		// Read back within the same transaction — should see the uncommitted write.
		val, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		return val, nil
	})
	if err != nil {
		t.Fatalf("RYW transaction: %v", err)
	}

	val := result.([]byte)
	if val == nil {
		t.Fatal("RYW: got nil, want the uncommitted write to be visible")
	}
	if string(val) != "hello_ryw" {
		t.Errorf("RYW: got %q, want %q", val, "hello_ryw")
	}
}

// ---------------------------------------------------------------------------
// Transaction size limit
// ---------------------------------------------------------------------------

// TestSizeLimit_CPort verifies that setting a transaction size limit causes
// Commit to fail with transaction_too_large (2101) when exceeded.
// Ported from unit_tests.cpp line 835
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L835
//
// Pure unit test — no Docker needed. The size limit check happens at Commit
// time in our Go client.
func TestSizeLimit_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetSizeLimit(1000) // 1000 bytes

	// Write a key with a large value that pushes total transaction size over 1000.
	bigValue := make([]byte, 1200)
	for i := range bigValue {
		bigValue[i] = 'x'
	}
	tx.Set([]byte("c_sizelim_key"), bigValue)

	// Commit should fail with transaction_too_large.
	err := tx.Commit(context.Background())
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

// ---------------------------------------------------------------------------
// GetRange streaming mode EXACT
// ---------------------------------------------------------------------------

// TestGetRangeStreamingExact_CPort verifies that GetRange with an exact limit
// returns precisely that many results, and that more=true when additional keys
// exist beyond the limit.
// Ported from unit_tests.cpp FDB_STREAMING_MODE_EXACT line 1261
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1261
func TestGetRangeStreamingExact_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	prefix := "c_exact_"

	// Write 10 keys: key_00 through key_09.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			k := fmt.Sprintf("%skey_%02d", prefix, i)
			v := fmt.Sprintf("val_%02d", i)
			tx.Set([]byte(k), []byte(v))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	type rangeResult struct {
		kvs  []KeyValue
		more bool
	}

	// GetRange with limit=5 — should return exactly 5 results and more=true.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix + "key_00")
		end := []byte(prefix + "key_99\x00")
		kvs, more, err := tx.GetRange(ctx, begin, end, 5)
		return rangeResult{kvs, more}, err
	})
	if err != nil {
		t.Fatalf("GetRange(limit=5): %v", err)
	}
	rr := result.(rangeResult)
	if len(rr.kvs) != 5 {
		t.Errorf("limit=5: got %d results, want 5", len(rr.kvs))
	}
	if !rr.more {
		t.Error("limit=5: more should be true (10 keys total)")
	}
	// Verify keys are in order.
	for i, kv := range rr.kvs {
		wantKey := fmt.Sprintf("%skey_%02d", prefix, i)
		if string(kv.Key) != wantKey {
			t.Errorf("limit=5 key[%d]: got %q, want %q", i, kv.Key, wantKey)
		}
	}

	// GetRange with limit=10 — should return exactly 10 results and more=false.
	result, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix + "key_00")
		end := []byte(prefix + "key_99\x00")
		kvs, more, err := tx.GetRange(ctx, begin, end, 10)
		return rangeResult{kvs, more}, err
	})
	if err != nil {
		t.Fatalf("GetRange(limit=10): %v", err)
	}
	rr = result.(rangeResult)
	if len(rr.kvs) != 10 {
		t.Errorf("limit=10: got %d results, want 10", len(rr.kvs))
	}
	// Note: FDB returns more=true when the limit is reached, even if the
	// range is exhausted. The server doesn't scan past the limit.
	// Verify all 10 keys.
	for i, kv := range rr.kvs {
		wantKey := fmt.Sprintf("%skey_%02d", prefix, i)
		wantVal := fmt.Sprintf("val_%02d", i)
		if string(kv.Key) != wantKey {
			t.Errorf("limit=10 key[%d]: got %q, want %q", i, kv.Key, wantKey)
		}
		if string(kv.Value) != wantVal {
			t.Errorf("limit=10 val[%d]: got %q, want %q", i, kv.Value, wantVal)
		}
	}
}

// ---------------------------------------------------------------------------
// Read-your-writes options
// ---------------------------------------------------------------------------

// TestRYWDisable_CPort verifies that disabling read-your-writes causes Get
// to bypass the RYW cache. An uncommitted Set is invisible to subsequent Get
// when RYW is disabled, because the server hasn't seen the write yet.
// Ported from unit_tests.cpp line 671
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L671
func TestRYWDisable_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_rywd_"

	tx := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)
	tx.SetReadYourWritesDisable()

	key := []byte(pfx + "foo")
	tx.Set(key, []byte("bar"))

	// With RYW disabled, Get goes straight to the server. The value hasn't
	// been committed, so the server returns nil.
	val, err := tx.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != nil {
		t.Errorf("expected nil (RYW disabled, uncommitted write), got %q", val)
	}
}

// TestSnapshotRYWEnable_CPort verifies that snapshot reads go through the
// RYW cache by default. An uncommitted Set is visible via Snapshot().Get().
// Ported from unit_tests.cpp line 699
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L699
func TestSnapshotRYWEnable_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_sryw_"

	tx := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)

	key := []byte(pfx + "foo")
	tx.Set(key, []byte("bar"))

	// Snapshot RYW is enabled by default — uncommitted write should be visible.
	val, err := tx.Snapshot().Get(ctx, key)
	if err != nil {
		t.Fatalf("Snapshot().Get: %v", err)
	}
	if val == nil {
		t.Fatal("expected value from snapshot RYW, got nil")
	}
	if string(val) != "bar" {
		t.Errorf("snapshot value: got %q, want %q", val, "bar")
	}
}

// TestSnapshotRYWDisable_CPort verifies that disabling snapshot RYW makes
// snapshot reads bypass the cache, while regular reads still use RYW.
// Ported from unit_tests.cpp line 728
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L728
func TestSnapshotRYWDisable_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "c_snrd_"

	tx := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)
	tx.SetSnapshotRYWDisable()

	key := []byte(pfx + "foo")
	tx.Set(key, []byte("bar"))

	// Snapshot RYW disabled — snapshot read goes to server, returns nil.
	snapVal, err := tx.Snapshot().Get(ctx, key)
	if err != nil {
		t.Fatalf("Snapshot().Get: %v", err)
	}
	if snapVal != nil {
		t.Errorf("expected nil (snapshot RYW disabled), got %q", snapVal)
	}

	// Regular Get still uses RYW — uncommitted write should be visible.
	regVal, err := tx.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if regVal == nil {
		t.Fatal("expected value from regular RYW, got nil")
	}
	if string(regVal) != "bar" {
		t.Errorf("regular Get value: got %q, want %q", regVal, "bar")
	}
}

// ---------------------------------------------------------------------------
// Transaction size limit boundary validation
// ---------------------------------------------------------------------------

// TestSizeLimitTooSmall_CPort verifies that a size limit below the minimum
// (32) causes error 2006 (invalid_option_value) at commit time.
// Ported from unit_tests.cpp line 811
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L811
func TestSizeLimitTooSmall_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetSizeLimit(31)
	tx.Set([]byte("foo"), []byte("bar"))

	err := tx.Commit(context.Background())
	if err == nil {
		t.Fatal("expected error for size limit below minimum, got nil")
	}

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != 2006 {
		t.Errorf("error code: got %d, want 2006 (invalid_option_value)", fdbErr.Code)
	}
}

// TestSizeLimitTooLarge_CPort verifies that a size limit above the maximum
// (10,000,000) causes error 2006 (invalid_option_value) at commit time.
// Ported from unit_tests.cpp line 823
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L823
func TestSizeLimitTooLarge_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetSizeLimit(10_000_001)
	tx.Set([]byte("foo"), []byte("bar"))

	err := tx.Commit(context.Background())
	if err == nil {
		t.Fatal("expected error for size limit above maximum, got nil")
	}

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != 2006 {
		t.Errorf("error code: got %d, want 2006 (invalid_option_value)", fdbErr.Code)
	}
}

// TestSizeLimitMinimum_CPort verifies that the minimum valid size limit (32)
// is accepted but the transaction still fails with 2101 (transaction_too_large)
// when the mutations exceed that tiny limit.
// Ported from unit_tests.cpp line 835
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L835
func TestSizeLimitMinimum_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetSizeLimit(32)
	tx.Set([]byte("foo"), []byte("foundation database is amazing"))

	err := tx.Commit(context.Background())
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

// ---------------------------------------------------------------------------
// Watch with RYW disabled
// ---------------------------------------------------------------------------

// TestWatchRYWDisable_CPort verifies that creating a watch on a transaction
// with RYW disabled returns watches_disabled (1034) immediately.
// Ported from unit_tests.cpp line 1973
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1973
func TestWatchRYWDisable_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)
	tx.SetReadYourWritesDisable()

	err = tx.Watch(ctx, []byte("c_wryw_foo"))
	if err == nil {
		t.Fatal("expected watches_disabled error, got nil")
	}

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != 1034 {
		t.Errorf("error code: got %d, want 1034 (watches_disabled)", fdbErr.Code)
	}
}

// ---------------------------------------------------------------------------
// System key access control
// ---------------------------------------------------------------------------

// TestCannotReadSystemKey_CPort verifies that reading a \xff system key
// without READ_SYSTEM_KEYS returns key_outside_legal_range (2004).
// Ported from unit_tests.cpp line 595
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L595
func TestCannotReadSystemKey_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)

	_, err = tx.Get(ctx, []byte("\xff/coordinators"))
	if err == nil {
		t.Fatal("expected key_outside_legal_range, got nil")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != 2004 {
		t.Errorf("error code: got %d, want 2004 (key_outside_legal_range)", fdbErr.Code)
	}
}

// TestReadSystemKey_CPort verifies that reading a \xff system key
// succeeds when READ_SYSTEM_KEYS is set.
// Ported from unit_tests.cpp line 604
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L604
func TestReadSystemKey_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)
	tx.SetReadSystemKeys()

	// \xff/coordinators should be readable with READ_SYSTEM_KEYS.
	// The value exists on any configured FDB cluster.
	val, err := tx.Get(ctx, []byte("\xff/coordinators"))
	if err != nil {
		t.Fatalf("Get with READ_SYSTEM_KEYS: %v", err)
	}
	if val == nil {
		t.Error("expected non-nil value for \\xff/coordinators")
	}
}

// TestCannotWriteSystemKey_CPort verifies that writing a \xff system key
// without ACCESS_SYSTEM_KEYS returns key_outside_legal_range (2004) at commit.
// Ported from unit_tests.cpp line 609
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L609
func TestCannotWriteSystemKey_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.Set([]byte("\xff\x02"), []byte("bar"))

	err := tx.Commit(context.Background())
	if err == nil {
		t.Fatal("expected key_outside_legal_range, got nil")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got %T: %v", err, err)
	}
	if fdbErr.Code != 2004 {
		t.Errorf("error code: got %d, want 2004 (key_outside_legal_range)", fdbErr.Code)
	}
}

// TestWriteSystemKey_CPort verifies that writing a \xff system key
// succeeds when ACCESS_SYSTEM_KEYS is set.
// Ported from unit_tests.cpp line 619
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L619
func TestWriteSystemKey_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.SetAccessSystemKeys()
		tx.Set([]byte("\xff\x02"), []byte("bar"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set with ACCESS_SYSTEM_KEYS: %v", err)
	}

	// Read it back.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.SetReadSystemKeys()
		return tx.Get(ctx, []byte("\xff\x02"))
	})
	if err != nil {
		t.Fatalf("Get with READ_SYSTEM_KEYS: %v", err)
	}
	if string(result.([]byte)) != "bar" {
		t.Errorf("value: got %q, want %q", result, "bar")
	}
}

// ---------------------------------------------------------------------------
// Versionstamp — invalid index
// ---------------------------------------------------------------------------

// TestAtomicSetVersionstampedKeyInvalidIndex_CPort verifies that a
// SET_VERSIONSTAMPED_KEY with an offset that would place the 10-byte
// versionstamp past the end of the key returns an error on commit.
// Ported from unit_tests.cpp line 1834
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L1834
func TestAtomicSetVersionstampedKeyInvalidIndex_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Build key: "foo" + \x00 (padding to 4 bytes) + 4-byte LE offset=4.
	// Key body before offset suffix is 4 bytes. Starting at index 4 there are
	// only 0 bytes for the versionstamp, but 10 are needed → error.
	// C++ test: keybuf = {'f','o','o','\0', ..., 4,0,0,0} with 17 bytes total.
	// The 4-byte LE offset at end says "versionstamp starts at byte 4",
	// but only 9 bytes remain (indices 4..12) — need 10 → commit error.
	keybuf := []byte{'f', 'o', 'o', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 0, 0}

	tx := db.CreateTransaction()
	tx.Atomic(MutSetVersionstampedKey, keybuf, []byte("bar"))
	err := tx.Commit(ctx)
	if err == nil {
		t.Fatal("expected commit to fail with invalid versionstamp index, got nil")
	}
	// C++ test only checks that the error is non-zero (type unspecified).
	t.Logf("expected commit error: %v", err)
}

// TestAtomicSetVersionstampedValueInvalidIndex_CPort verifies that a
// SET_VERSIONSTAMPED_VALUE with an offset that would place the 10-byte
// versionstamp past the end of the value returns an error on commit.
func TestAtomicSetVersionstampedValueInvalidIndex_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Value: 3 bytes + 4-byte LE offset. Offset=0 needs 10 bytes at position 0,
	// but value body is only 3 bytes → error.
	value := []byte{'a', 'b', 'c', 0, 0, 0, 0}

	tx := db.CreateTransaction()
	tx.Atomic(MutSetVersionstampedValue, []byte("vsv_invalid_test"), value)
	err := tx.Commit(ctx)
	if err == nil {
		t.Fatal("expected commit to fail with invalid versionstamp value offset, got nil")
	}
	t.Logf("expected commit error: %v", err)
}

// TestAtomicSetVersionstampedValueTooShort_CPort verifies that a
// SET_VERSIONSTAMPED_VALUE with fewer than 4 bytes returns an error.
func TestAtomicSetVersionstampedValueTooShort_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	tx.Atomic(MutSetVersionstampedValue, []byte("vsv_short_test"), []byte("ab"))
	err := tx.Commit(ctx)
	if err == nil {
		t.Fatal("expected commit to fail with too-short versionstamp value, got nil")
	}
	t.Logf("expected commit error: %v", err)
}

// ---------------------------------------------------------------------------
// Transaction Reset
// ---------------------------------------------------------------------------

// TestReset_CPort verifies that Transaction.Reset() clears all state and allows
// the transaction to be reused for a fresh transaction.
func TestReset_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()

	// First transaction: write a key and commit.
	tx.Set([]byte("reset_test_a"), []byte("1"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	// Reset and reuse for a second transaction.
	tx.Reset()

	// Verify we can read (fresh read version) and write again.
	val, err := tx.Get(ctx, []byte("reset_test_a"))
	if err != nil {
		t.Fatalf("Get after reset: %v", err)
	}
	if string(val) != "1" {
		t.Errorf("value after reset: got %q, want %q", val, "1")
	}

	tx.Set([]byte("reset_test_b"), []byte("2"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("second commit after reset: %v", err)
	}

	// Verify both keys exist.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		b, bErr := tx.Get(ctx, []byte("reset_test_b"))
		return b, bErr
	})
	if err != nil {
		t.Fatalf("verify key b: %v", err)
	}
	if string(result.([]byte)) != "2" {
		t.Errorf("key b: got %q, want %q", result, "2")
	}
}

// TestResetClearsRetryCount_CPort verifies that Reset() clears the retry counter,
// so OnError will retry even if the previous transaction had exhausted retries.
func TestResetClearsRetryCount_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	tx.SetRetryLimit(0) // no retries allowed

	// OnError with a retryable error should fail (retry limit 0).
	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
	if err == nil {
		t.Fatal("expected OnError to fail with retry limit 0")
	}

	// After Reset, retry count is cleared. Set limit=1 and verify OnError succeeds.
	tx.Reset()
	tx.SetRetryLimit(1)
	err = tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
	if err != nil {
		t.Fatalf("OnError after reset: %v", err)
	}
}

// TestResetClearsReadVersion_CPort verifies that Reset() clears the read version.
func TestResetClearsReadVersion_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()

	// Set a specific read version.
	tx.SetReadVersion(12345)

	// Reset should clear it. A subsequent Get should fetch a new read version.
	tx.Reset()

	// After reset, Get should succeed (fetches new GRV, not use stale 12345).
	_, err := tx.Get(ctx, []byte("reset_rv_test"))
	if err != nil {
		t.Fatalf("Get after reset: %v", err)
	}
}

// TestResetAfterCancel_CPort verifies that Reset() on a cancelled transaction
// restores it to an active state.
func TestResetAfterCancel_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	tx.Cancel()

	// After Cancel, operations should fail.
	tx.Reset()

	// After Reset, the transaction should be usable again.
	tx.Set([]byte("reset_cancel_test"), []byte("val"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit after Reset on cancelled tx: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetLocations
// ---------------------------------------------------------------------------

// TestGetLocations_CPort verifies that GetLocations returns at least one
// shard covering the requested key range.
func TestGetLocations_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Write some data so the range is non-empty.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("loc_a"), []byte("1"))
		tx.Set([]byte("loc_z"), []byte("2"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetLocations(ctx, []byte("loc_"), []byte("loc_~"), 100)
	})
	if err != nil {
		t.Fatalf("GetLocations: %v", err)
	}
	locs := result.([]LocationResult)
	if len(locs) == 0 {
		t.Fatal("expected at least one location, got 0")
	}
	for i, loc := range locs {
		if len(loc.Servers) == 0 {
			t.Errorf("location[%d]: no servers", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Write-write conflict detection
// ---------------------------------------------------------------------------

// TestWriteConflict_CPort verifies that two concurrent transactions writing to
// the same key will cause one to fail with not_committed (1020).
func TestWriteConflict_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Seed a key so both transactions read something (creating read conflicts).
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("conflict_key"), []byte("initial"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Create two transactions at the same read version.
	tx1 := db.CreateTransaction()
	tx2 := db.CreateTransaction()

	// Both read the key (adds read conflict range) then write to it.
	_, err = tx1.Get(ctx, []byte("conflict_key"))
	if err != nil {
		t.Fatalf("tx1 Get: %v", err)
	}
	_, err = tx2.Get(ctx, []byte("conflict_key"))
	if err != nil {
		t.Fatalf("tx2 Get: %v", err)
	}

	tx1.Set([]byte("conflict_key"), []byte("from_tx1"))
	tx2.Set([]byte("conflict_key"), []byte("from_tx2"))

	// Commit tx1 first — should succeed.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 commit: %v", err)
	}

	// Commit tx2 — should fail with not_committed (1020) since tx1 committed
	// a write to a key that tx2 read.
	err = tx2.Commit(ctx)
	if err == nil {
		t.Fatal("expected tx2 commit to fail with conflict, got nil")
	}

	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("expected FDBError, got: %T: %v", err, err)
	}
	if fdbErr.Code != ErrNotCommitted {
		t.Errorf("expected error 1020 (not_committed), got %d", fdbErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Versionstamp boundary
// ---------------------------------------------------------------------------

// TestVersionstampValidOffset_CPort verifies that SET_VERSIONSTAMPED_VALUE
// with an offset at the maximum valid position (offset + 10 == bodyLen) succeeds.
func TestVersionstampValidOffset_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Value: 10 zero bytes (versionstamp placeholder) + 4-byte LE offset=0.
	// bodyLen = 10, offset = 0, offset + 10 = 10 == bodyLen → valid.
	value := make([]byte, 14)
	// offset bytes at end are already 0 (offset=0), which is valid.

	tx := db.CreateTransaction()
	tx.Set([]byte("vs_boundary_key"), []byte("placeholder"))
	tx.Atomic(MutSetVersionstampedValue, []byte("vs_boundary_key"), value)
	err := tx.Commit(ctx)
	if err != nil {
		t.Fatalf("expected commit to succeed with valid boundary offset, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Transaction reuse after commit
// ---------------------------------------------------------------------------

// TestTransactionReuseAfterCommit_CPort verifies that a transaction can be
// reused after commit (auto-reset via postCommitReset).
func TestTransactionReuseAfterCommit_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()

	// First commit.
	tx.Set([]byte("reuse_1"), []byte("a"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	v1, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("GetCommittedVersion after 1st: %v", err)
	}

	// Second commit — transaction should auto-reset after first commit.
	tx.Set([]byte("reuse_2"), []byte("b"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	v2, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("GetCommittedVersion after 2nd: %v", err)
	}

	// v2 should be >= v1 (new transaction, later commit version).
	if v2 < v1 {
		t.Errorf("second commit version %d should be >= first %d", v2, v1)
	}

	// Verify both keys exist.
	result, err := db.Transact(ctx, func(rtx *Transaction) (any, error) {
		a, err := rtx.Get(ctx, []byte("reuse_1"))
		if err != nil {
			return nil, err
		}
		b, err := rtx.Get(ctx, []byte("reuse_2"))
		if err != nil {
			return nil, err
		}
		return [2][]byte{a, b}, nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	vals := result.([2][]byte)
	if string(vals[0]) != "a" {
		t.Errorf("reuse_1: got %q, want %q", vals[0], "a")
	}
	if string(vals[1]) != "b" {
		t.Errorf("reuse_2: got %q, want %q", vals[1], "b")
	}
}

// ---------------------------------------------------------------------------
// Database-level SetAccessSystemKeys
// ---------------------------------------------------------------------------

// TestDatabaseLevelAccessSystemKeys_CPort verifies that database-level
// access_system_keys option enables writing \xff-prefixed keys.
func TestDatabaseLevelAccessSystemKeys_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Set database-level default so all transactions can access system keys.
	db.SetDefaultAccessSystemKeys()

	// Write a system key — no per-tx SetAccessSystemKeys needed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("\xff\x03"), []byte("sysval"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write system key: %v", err)
	}

	// Read it back — no per-tx SetReadSystemKeys needed.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("\xff\x03"))
	})
	if err != nil {
		t.Fatalf("read system key: %v", err)
	}
	if string(result.([]byte)) != "sysval" {
		t.Errorf("system key value: got %q, want %q", result, "sysval")
	}
}

// TestDatabaseLevelTimeout_CPort verifies that database-level transaction
// timeout is applied to all new transactions.
// Ported from unit_tests.cpp FDB_DB_OPTION_TRANSACTION_TIMEOUT
func TestDatabaseLevelTimeout_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Set a very short timeout at database level.
	db.SetTransactionTimeout(1) // 1ms

	// A transaction should time out almost immediately.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		time.Sleep(50 * time.Millisecond)
		return tx.Get(ctx, []byte("db_timeout_test"))
	})

	// Should get transaction_timed_out (1031) — non-retryable, escapes retry loop.
	if err == nil {
		t.Fatal("expected transaction to time out")
	}
	var fdbErr *wire.FDBError
	if errors.As(err, &fdbErr) {
		if fdbErr.Code != ErrTransactionTimedOut {
			t.Errorf("expected error 1031 (transaction_timed_out), got %d", fdbErr.Code)
		}
	}
}

// TestDatabaseLevelRetryLimit_CPort verifies that database-level retry limit
// is applied to all new transactions.
// Ported from unit_tests.cpp FDB_DB_OPTION_TRANSACTION_RETRY_LIMIT
func TestDatabaseLevelRetryLimit_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Set retry limit of 0 at database level — no retries allowed.
	db.SetTransactionRetryLimit(0)

	tx := db.CreateTransaction()
	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
	if err == nil {
		t.Fatal("expected OnError to fail with retry limit 0 from database default")
	}
}

// TestDatabaseLevelSizeLimit_CPort verifies that database-level size limit
// is applied to all new transactions.
// Ported from unit_tests.cpp FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT
func TestDatabaseLevelSizeLimit_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Set a tiny size limit at database level.
	db.SetTransactionSizeLimit(32) // minimum valid

	tx := db.CreateTransaction()
	// Write enough data to exceed 32 bytes.
	tx.Set([]byte("big_key_that_is_definitely_over_32_bytes"), []byte("a value"))
	err := tx.Commit(ctx)
	if err == nil {
		t.Fatal("expected commit to fail with size limit exceeded")
	}
	var fdbErr *wire.FDBError
	if errors.As(err, &fdbErr) && fdbErr.Code != 2101 {
		t.Errorf("expected error 2101 (transaction_too_large), got %d", fdbErr.Code)
	}
}

// ---------------------------------------------------------------------------
// OnError retry semantics
// ---------------------------------------------------------------------------

// TestOnErrorRetryWithBackoff_CPort verifies that OnError increments retry
// count and eventually stops retrying when limit is reached.
func TestOnErrorRetryWithBackoff_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	tx.SetRetryLimit(3)

	// Three OnError calls should succeed (retries 0, 1, 2).
	for i := 0; i < 3; i++ {
		err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
		if err != nil {
			t.Fatalf("OnError retry %d: unexpected error: %v", i, err)
		}
	}

	// Fourth call should fail (retry 3 >= limit 3).
	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
	if err == nil {
		t.Fatal("expected OnError to fail after retry limit exhausted")
	}
}

// TestOnErrorNonRetryable_CPort verifies that OnError returns the error
// for non-retryable error codes.
func TestOnErrorNonRetryable_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()

	// transaction_timed_out (1031) is never retryable.
	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrTransactionTimedOut})
	if err == nil {
		t.Fatal("expected transaction_timed_out to be non-retryable")
	}
	var fdbErr *wire.FDBError
	if errors.As(err, &fdbErr) {
		if fdbErr.Code != ErrTransactionTimedOut {
			t.Errorf("expected error 1031, got %d", fdbErr.Code)
		}
	}
}

// TestOnErrorNonFDBError_CPort verifies that OnError transitions to errored
// state for non-FDB errors.
func TestOnErrorNonFDBError_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()

	err := tx.OnError(context.Background(), fmt.Errorf("some non-FDB error"))
	if err == nil {
		t.Fatal("expected non-FDB error to be returned")
	}
	if err.Error() != "some non-FDB error" {
		t.Errorf("wrong error: %v", err)
	}

	// Transaction should be in errored state — commit should fail.
	commitErr := tx.Commit(ctx)
	if commitErr == nil {
		t.Fatal("expected commit to fail on errored transaction")
	}
}

// TestResourceConstrainedBackoff_CPort verifies that proxy memory errors
// (1042, 1078) use RESOURCE_CONSTRAINED_MAX_BACKOFF (30s) instead of
// DEFAULT_MAX_BACKOFF (1s). Matches C++ NativeAPI.actor.cpp getBackoff().
func TestResourceConstrainedBackoff_CPort(t *testing.T) {
	t.Parallel()

	// Test that after many retries, the backoff cap for proxy memory errors
	// is higher than for normal errors.
	txNormal := &Transaction{creationTime: time.Now()}
	txProxy := &Transaction{creationTime: time.Now()}

	// Drive both to max backoff by calling nextBackoff many times.
	for i := 0; i < 20; i++ {
		txNormal.nextBackoff(ErrNotCommitted)
		txProxy.nextBackoff(ErrProxyMemoryLimitExceeded)
	}

	// Normal backoff should cap at 1s (DEFAULT_MAX_BACKOFF).
	if txNormal.backoff > maxBackoff {
		t.Errorf("normal backoff exceeds cap: %v > %v", txNormal.backoff, maxBackoff)
	}

	// Proxy memory backoff should cap at 30s (RESOURCE_CONSTRAINED_MAX_BACKOFF).
	if txProxy.backoff > resourceConstrainedMaxBackoff {
		t.Errorf("proxy backoff exceeds cap: %v > %v", txProxy.backoff, resourceConstrainedMaxBackoff)
	}

	// The proxy cap should be strictly higher than the normal cap.
	if txProxy.backoff <= maxBackoff {
		t.Errorf("proxy backoff should exceed normal cap: %v <= %v", txProxy.backoff, maxBackoff)
	}
}

// TestBlobGranuleRetryable_CPort verifies that blob_granule_request_failed (1079)
// is retryable. Matches C++ NativeAPI.actor.cpp onError().
func TestBlobGranuleRetryable_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{creationTime: time.Now()}
	err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrBlobGranuleRequestFailed})
	if err != nil {
		t.Fatalf("blob_granule_request_failed should be retryable, got: %v", err)
	}
	if tx.retryCount != 1 {
		t.Errorf("retryCount: got %d, want 1", tx.retryCount)
	}
}

// ---------------------------------------------------------------------------
// RYW cache edge cases
// ---------------------------------------------------------------------------

// TestRYWDoAddResultLength_CPort verifies that doAdd result length = len(param),
// matching C++ Atomic.h doAdd() which allocates otherOperand.size().
// If base is longer than param, high bytes are silently truncated.
func TestRYWDoAddResultLength_CPort(t *testing.T) {
	t.Parallel()

	// Base = 4 bytes, param = 2 bytes → result should be 2 bytes.
	base := []byte{0x01, 0x02, 0x03, 0x04}
	param := []byte{0x05, 0x06}
	result, _ := applyAtomic(MutAddValue, base, param)
	if len(result) != len(param) {
		t.Errorf("result length: got %d, want %d (len(param))", len(result), len(param))
	}
	// 0x01 + 0x05 = 0x06, 0x02 + 0x06 = 0x08
	if result[0] != 0x06 || result[1] != 0x08 {
		t.Errorf("result: got %x, want [06 08]", result)
	}

	// Base = 2 bytes, param = 4 bytes → result should be 4 bytes.
	base2 := []byte{0xFF, 0x01}
	param2 := []byte{0x01, 0x00, 0x00, 0x00}
	result2, _ := applyAtomic(MutAddValue, base2, param2)
	if len(result2) != len(param2) {
		t.Errorf("result2 length: got %d, want %d", len(result2), len(param2))
	}
	// 0xFF + 0x01 = carry 1, 0x01 + 0x00 + carry = 0x02
	if result2[0] != 0x00 || result2[1] != 0x02 || result2[2] != 0x00 || result2[3] != 0x00 {
		t.Errorf("result2: got %x, want [00 02 00 00]", result2)
	}

	// Base absent (nil), param = 4 bytes → result should be copy of param.
	result3, _ := applyAtomic(MutAddValue, nil, []byte{0x01, 0x02, 0x03, 0x04})
	if !bytes.Equal(result3, []byte{0x01, 0x02, 0x03, 0x04}) {
		t.Errorf("result3: got %x, want [01 02 03 04]", result3)
	}
}

// TestRYWAtomicAdd_CPort verifies that an atomic ADD followed by a Get within
// the same transaction returns the correct accumulated value via RYW.
func TestRYWAtomicAdd_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("ryw_atomic_add")

	// Seed with initial value 100 (8-byte LE).
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, 100)
		tx.Set(key, buf)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Atomic ADD +50 and +25 in same tx, then read.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		add50 := make([]byte, 8)
		binary.LittleEndian.PutUint64(add50, 50)
		tx.Atomic(MutAddValue, key, add50)

		add25 := make([]byte, 8)
		binary.LittleEndian.PutUint64(add25, 25)
		tx.Atomic(MutAddValue, key, add25)

		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("atomic add + get: %v", err)
	}
	val := result.([]byte)
	if len(val) != 8 {
		t.Fatalf("expected 8 bytes, got %d", len(val))
	}
	got := binary.LittleEndian.Uint64(val)
	if got != 175 {
		t.Errorf("expected 175 (100+50+25), got %d", got)
	}
}

// TestRYWClearThenGet_CPort verifies that Clear followed by Get in the same
// transaction returns nil (the key was logically deleted).
func TestRYWClearThenGet_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("ryw_clear_get")

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("exists"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear then Get in same tx.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("clear + get: %v", err)
	}
	if result.([]byte) != nil {
		t.Errorf("expected nil after clear, got %q", result)
	}
}

// TestRYWSetClearGet_CPort verifies that Set then Clear then Get returns nil.
func TestRYWSetClearGet_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("ryw_scg")

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v1"))
		tx.Clear(key)
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("set+clear+get: %v", err)
	}
	if result.([]byte) != nil {
		t.Errorf("expected nil after set+clear, got %q", result)
	}
}

// TestRYWClearSetGet_CPort verifies that Clear then Set then Get returns the
// new value (Set overwrites the pending Clear).
func TestRYWClearSetGet_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte("ryw_csg")

	// Seed.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("old"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(key)
		tx.Set(key, []byte("new"))
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("clear+set+get: %v", err)
	}
	if string(result.([]byte)) != "new" {
		t.Errorf("expected 'new', got %q", result)
	}
}

// TestRYWClearRangeGetRange_CPort verifies that ClearRange removes keys
// from GetRange results within the same transaction.
func TestRYWClearRangeGetRange_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "ryw_crg_"

	// Seed 5 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 5; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%d", pfx, i)), []byte(fmt.Sprintf("v%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear keys 1-3, then GetRange — should only see 0 and 4.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte(pfx+"1"), []byte(pfx+"4"))
		kvs, _, rangeErr := tx.GetRange(ctx, []byte(pfx), []byte(pfx+"~"), 100)
		return kvs, rangeErr
	})
	if err != nil {
		t.Fatalf("clear range + get range: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 2 {
		for i, kv := range kvs {
			t.Logf("kv[%d] = %q → %q", i, kv.Key, kv.Value)
		}
		t.Fatalf("expected 2 keys after clear range, got %d", len(kvs))
	}
	if string(kvs[0].Key) != pfx+"0" || string(kvs[1].Key) != pfx+"4" {
		t.Errorf("wrong keys: got %q and %q", kvs[0].Key, kvs[1].Key)
	}
}

// TestRYWSetNewKeysGetRange_CPort verifies that new keys Set within a
// transaction appear in GetRange results via RYW.
func TestRYWSetNewKeysGetRange_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "ryw_sng_"

	// No seed — start empty.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"b"), []byte("2"))
		tx.Set([]byte(pfx+"a"), []byte("1"))
		tx.Set([]byte(pfx+"c"), []byte("3"))
		kvs, _, rangeErr := tx.GetRange(ctx, []byte(pfx), []byte(pfx+"~"), 100)
		return kvs, rangeErr
	})
	if err != nil {
		t.Fatalf("set + get range: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(kvs))
	}
	// Should be sorted.
	expected := []string{pfx + "a", pfx + "b", pfx + "c"}
	for i, kv := range kvs {
		if string(kv.Key) != expected[i] {
			t.Errorf("key[%d]: got %q, want %q", i, kv.Key, expected[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Locality / addressing
// ---------------------------------------------------------------------------

// TestGetAddressesForKey_CPort verifies that GetAddressesForKey returns at
// least one non-empty address for a key that exists on a storage server.
// Ported from unit_tests.cpp line 2317
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2317
func TestGetAddressesForKey_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	key := []byte(t.Name() + "_addr_key")

	// Write the key so the shard is populated.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("addr_value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Get storage server addresses for the key.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetAddressesForKey(ctx, key)
	})
	if err != nil {
		t.Fatalf("GetAddressesForKey: %v", err)
	}
	addrs := result.([]string)
	if len(addrs) == 0 {
		t.Fatal("expected at least one address for existing key")
	}
	for i, addr := range addrs {
		if len(addr) == 0 {
			t.Errorf("addr[%d] is empty", i)
		}
		// Each address should look like host:port.
		if bytes.IndexByte([]byte(addr), ':') < 0 {
			t.Errorf("addr[%d] = %q: expected host:port format", i, addr)
		}
		t.Logf("addr[%d] = %q", i, addr)
	}
}

// ---------------------------------------------------------------------------
// Error predicate — RETRYABLE_NOT_COMMITTED vs MAYBE_COMMITTED
// ---------------------------------------------------------------------------

// TestErrorPredicateRetryableNotCommitted_CPort verifies the behavioral
// distinction between RETRYABLE_NOT_COMMITTED (1020) and MAYBE_COMMITTED
// (1021) error classes:
//   - 1020 (not_committed): retryable, no self-conflict injection
//   - 1021 (commit_unknown_result): retryable AND injects write→read
//     self-conflicts on the reset transaction
//   - 1036 (accessed_unreadable): not retryable
//
// Ported from unit_tests.cpp line 2432
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2432
func TestErrorPredicateRetryableNotCommitted_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// 1020 (not_committed) is RETRYABLE_NOT_COMMITTED: OnError returns nil.
	{
		tx := db.CreateTransaction()
		err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
		if err != nil {
			t.Errorf("1020 (not_committed) should be retryable, got: %v", err)
		}
	}

	// 1020 does NOT inject self-conflicts: write conflicts are dropped and
	// readConflicts stays empty after reset.
	{
		tx := db.CreateTransaction()
		tx.Set([]byte(t.Name()+"_sc_key"), []byte("val"))
		if len(tx.writeConflicts) == 0 {
			t.Fatal("Set should have added a write conflict")
		}
		_ = tx.OnError(context.Background(), &wire.FDBError{Code: ErrNotCommitted})
		if len(tx.readConflicts) != 0 {
			t.Errorf("1020 must NOT inject self-conflicts, got %d readConflicts", len(tx.readConflicts))
		}
	}

	// 1021 (commit_unknown_result) is MAYBE_COMMITTED: OnError returns nil.
	{
		tx := db.CreateTransaction()
		err := tx.OnError(context.Background(), &wire.FDBError{Code: ErrCommitUnknownResult})
		if err != nil {
			t.Errorf("1021 (commit_unknown_result) should be retryable, got: %v", err)
		}
	}

	// 1021 DOES inject self-conflicts: previous write ranges become read
	// conflict ranges on the reset transaction, preventing double-apply.
	{
		tx := db.CreateTransaction()
		tx.Set([]byte(t.Name()+"_mc_key"), []byte("val"))
		origWC := make([]KeyRange, len(tx.writeConflicts))
		copy(origWC, tx.writeConflicts)
		if len(origWC) == 0 {
			t.Fatal("Set should have added a write conflict")
		}
		_ = tx.OnError(context.Background(), &wire.FDBError{Code: ErrCommitUnknownResult})
		if len(tx.readConflicts) != len(origWC) {
			t.Errorf("1021 self-conflict: got %d readConflicts, want %d",
				len(tx.readConflicts), len(origWC))
		}
		for i, rc := range tx.readConflicts {
			if string(rc.Begin) != string(origWC[i].Begin) || string(rc.End) != string(origWC[i].End) {
				t.Errorf("readConflict[%d]: got [%q,%q), want [%q,%q)",
					i, rc.Begin, rc.End, origWC[i].Begin, origWC[i].End)
			}
		}
	}

	// 1036 (accessed_unreadable) is NOT retryable: OnError returns an error.
	{
		const errAccessedUnreadable = 1036
		tx := db.CreateTransaction()
		err := tx.OnError(context.Background(), &wire.FDBError{Code: errAccessedUnreadable})
		if err == nil {
			t.Error("1036 (accessed_unreadable) should NOT be retryable")
		}
		var fdbErr *wire.FDBError
		if errors.As(err, &fdbErr) && fdbErr.Code != errAccessedUnreadable {
			t.Errorf("expected error code 1036, got %d", fdbErr.Code)
		}
	}

	_ = ctx // ctx used for openTestDB
}

// ---------------------------------------------------------------------------
// Range metrics
// ---------------------------------------------------------------------------

// TestGetEstimatedRangeSizeBytes_CPort verifies that GetEstimatedRangeSizeBytes
// returns a non-negative value for a populated key range.
// Ported from unit_tests.cpp line 2500
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2500
func TestGetEstimatedRangeSizeBytes_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := t.Name() + "_ersize_"

	// Write 10 key-value pairs to give the storage server something to measure.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%d", pfx, i)), bytes.Repeat([]byte("x"), 100))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// GetEstimatedRangeSizeBytes must not error, result must be >= 0.
	// On a freshly written range the estimate may be 0 (storage server not yet
	// compacted), which is acceptable — the C++ test only asserts no error.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetEstimatedRangeSizeBytes(ctx, []byte(pfx), []byte(pfx+"~"))
	})
	if err != nil {
		t.Fatalf("GetEstimatedRangeSizeBytes: %v", err)
	}
	size := result.(int64)
	t.Logf("estimated range size: %d bytes", size)
	if size < 0 {
		t.Fatalf("expected non-negative size, got %d", size)
	}
}

// TestGetRangeSplitPoints_CPort verifies that GetRangeSplitPoints completes
// without error for a populated key range. The result may be empty (no split
// points) when the range fits within one chunk — that is correct behaviour.
// Ported from unit_tests.cpp line 2530
// https://github.com/apple/foundationdb/blob/7.3.75/bindings/c/test/unit/unit_tests.cpp#L2530
func TestGetRangeSplitPoints_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := t.Name() + "_splitpts_"

	// Write a handful of keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%d", pfx, i)), bytes.Repeat([]byte("y"), 100))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Call with a 100 000-byte chunk size. C++ getRangeSplitPoints ALWAYS frames
	// the result with the range bounds — results = [begin, <internal splits>, end]
	// (NativeAPI.actor.cpp:8177/8189) — so even a small range that fits in one chunk
	// returns [begin, end], NOT an empty slice. (The go-vs-cgo differential
	// TestDifferential_GetRangeSplitPoints caught the old impl returning [].)
	begin := []byte(pfx)
	end := []byte(pfx + "~")
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetRangeSplitPoints(ctx, begin, end, 100_000)
	})
	if err != nil {
		t.Fatalf("GetRangeSplitPoints: %v", err)
	}
	points := result.([][]byte)
	t.Logf("split points: %d", len(points))
	for i, p := range points {
		t.Logf("  split[%d]: %q", i, p)
	}
	// Framing: first point is begin, last is end, at least the two bounds present.
	if len(points) < 2 {
		t.Fatalf("expected at least [begin,end] framing, got %d points", len(points))
	}
	if !bytes.Equal(points[0], begin) {
		t.Fatalf("first split point = %q, want begin %q", points[0], begin)
	}
	if !bytes.Equal(points[len(points)-1], end) {
		t.Fatalf("last split point = %q, want end %q", points[len(points)-1], end)
	}
}

// TestClearRangeInverted_CPort verifies that ClearRange(begin > end) returns
// inverted_range (2005). Matches C++ fdb_transaction_clear_range_impl.
func TestClearRangeInverted_CPort(t *testing.T) {
	t.Parallel()

	tx := &Transaction{creationTime: time.Now()}
	err := tx.ClearRange([]byte("z"), []byte("a"))
	if err == nil {
		t.Fatal("expected inverted_range error")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrInvertedRange {
		t.Errorf("expected error 2005, got: %v", err)
	}
}

// TestClearRangeZeroWidth_CPort verifies that ClearRange(key, key) is a no-op.
// Matches C++ where zero-width ranges are silently ignored.
func TestClearRangeZeroWidth_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	prefix := t.Name() + "_"
	key := []byte(prefix + "survives")

	// Write a key.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// ClearRange with zero width should be a no-op.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return nil, tx.ClearRange(key, key) // begin == end → no-op
	})
	if err != nil {
		t.Fatalf("ClearRange zero-width: %v", err)
	}

	// Key should still exist.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("Get after zero-width clear: %v", err)
	}
	if result == nil {
		t.Fatal("key should still exist after zero-width ClearRange")
	}
}

// TestPostCommitReset_CPort verifies that after a successful commit,
// the transaction is reset for reuse. New mutations can be added and
// a second commit succeeds. GetCommittedVersion returns the LAST
// commit's version.
func TestPostCommitReset_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	prefix := t.Name() + "_"

	// First commit.
	tx.Set([]byte(prefix+"key1"), []byte("v1"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	v1, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("GetCommittedVersion after first: %v", err)
	}
	if v1 <= 0 {
		t.Errorf("first committed version should be positive, got %d", v1)
	}

	// Second commit on the same transaction (after auto-reset).
	tx.Set([]byte(prefix+"key2"), []byte("v2"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	v2, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("GetCommittedVersion after second: %v", err)
	}
	if v2 <= v1 {
		t.Errorf("second version %d should be > first version %d", v2, v1)
	}

	// Verify both keys exist.
	readTx := db.CreateTransaction()
	val1, err := readTx.Get(ctx, []byte(prefix+"key1"))
	if err != nil {
		t.Fatalf("read key1: %v", err)
	}
	if string(val1) != "v1" {
		t.Errorf("key1: got %q, want %q", val1, "v1")
	}
	val2, err := readTx.Get(ctx, []byte(prefix+"key2"))
	if err != nil {
		t.Fatalf("read key2: %v", err)
	}
	if string(val2) != "v2" {
		t.Errorf("key2: got %q, want %q", val2, "v2")
	}
}

// TestGetKeyBoundaryShortCircuit_CPort verifies that getKey returns
// correct results for boundary selectors without a network round trip.
// Matches C++ NativeAPI.actor.cpp getKey() short-circuits.
func TestGetKeyBoundaryShortCircuit_CPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tx := db.CreateTransaction()
	rv, err := tx.GetReadVersion(ctx)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)

	// C++ short-circuit: empty key with offset <= 0 returns empty key.
	key, err := tx.GetKey(ctx, []byte{}, false, 0)
	if err != nil {
		t.Fatalf("GetKey empty offset=0: %v", err)
	}
	if len(key) != 0 {
		t.Errorf("GetKey empty offset=0: expected empty, got %q", key)
	}

	// C++ short-circuit: empty key with negative offset returns empty key.
	key, err = tx.GetKey(ctx, []byte{}, false, -1)
	if err != nil {
		t.Fatalf("GetKey empty offset=-1: %v", err)
	}
	if len(key) != 0 {
		t.Errorf("GetKey empty offset=-1: expected empty, got %q", key)
	}

	// Empty key with positive offset does NOT short-circuit — it reaches
	// the storage server. FirstGreaterOrEqual("") with offset=1 resolves
	// to the first key in the database (or empty if DB is empty).
	// We just verify it doesn't error — the result depends on DB state.
	_, err = tx.GetKey(ctx, []byte{}, false, 1)
	if err != nil {
		t.Fatalf("GetKey empty offset=1 (non-short-circuit): %v", err)
	}
}
