package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// TestInterop_GoWriteCGoRead verifies the pure Go client writes data that the
// CGo client reads back byte-exactly.
func TestInterop_GoWriteCGoRead(t *testing.T) {
	prefix := "interop_gw_"
	pairs := map[string][]byte{
		prefix + "empty":  {},
		prefix + "ascii":  []byte("hello from go client"),
		prefix + "binary": {0x00, 0x01, 0xFE, 0xFF, 0x00, 0x80},
		prefix + "large":  bytes.Repeat([]byte("ABCDEFGHIJ"), 1000), // 10KB
	}

	// Write via Go client.
	_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		for k, v := range pairs {
			tx.Set(gofdb.Key(k), v)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("go write: %v", err)
	}

	// Read via CGo client.
	for k, want := range pairs {
		result, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			return tx.Get(cgofdb.Key(k)).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("cgo read %q: %v", k, err)
		}
		got := result.([]byte)
		if !bytes.Equal(got, want) {
			t.Errorf("key %q: got %d bytes, want %d bytes", k, len(got), len(want))
		}
	}
}

// TestInterop_CGoWriteGoRead verifies the CGo client writes data that the pure
// Go client reads back byte-exactly.
func TestInterop_CGoWriteGoRead(t *testing.T) {
	prefix := "interop_cw_"
	pairs := map[string][]byte{
		prefix + "empty":  {},
		prefix + "ascii":  []byte("hello from cgo client"),
		prefix + "binary": {0x00, 0x01, 0xFE, 0xFF, 0x00, 0x80},
		prefix + "large":  bytes.Repeat([]byte("ZYXWVUTSRQ"), 1000),
	}

	// Write via CGo client.
	_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		for k, v := range pairs {
			tx.Set(cgofdb.Key(k), v)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("cgo write: %v", err)
	}

	// Read via Go client. Invalidate GRV cache to see CGo writes.
	goClient.InvalidateGRVCache()
	for k, want := range pairs {
		result, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			return tx.Get(gofdb.Key(k)).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("go read %q: %v", k, err)
		}
		got := result.([]byte)
		if !bytes.Equal(got, want) {
			t.Errorf("key %q: got %d bytes, want %d bytes", k, len(got), len(want))
		}
	}
}

// TestInterop_MixedWriteBothRead has both clients write different keys in the
// same namespace, then both read all keys and verify consistency.
func TestInterop_MixedWriteBothRead(t *testing.T) {
	prefix := "interop_mix_"
	goKeys := map[string][]byte{
		prefix + "go_1": []byte("value_go_1"),
		prefix + "go_2": []byte("value_go_2"),
		prefix + "go_3": []byte("value_go_3"),
	}
	cgoKeys := map[string][]byte{
		prefix + "cgo_1": []byte("value_cgo_1"),
		prefix + "cgo_2": []byte("value_cgo_2"),
		prefix + "cgo_3": []byte("value_cgo_3"),
	}

	// Go client writes its keys.
	_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		for k, v := range goKeys {
			tx.Set(gofdb.Key(k), v)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("go write: %v", err)
	}

	// CGo client writes its keys.
	_, err = cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		for k, v := range cgoKeys {
			tx.Set(cgofdb.Key(k), v)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("cgo write: %v", err)
	}

	// Merge all expected pairs.
	all := make(map[string][]byte)
	for k, v := range goKeys {
		all[k] = v
	}
	for k, v := range cgoKeys {
		all[k] = v
	}

	// Go client reads all keys. Invalidate cache to see CGo writes.
	goClient.InvalidateGRVCache()
	for k, want := range all {
		result, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			return tx.Get(gofdb.Key(k)).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("go read %q: %v", k, err)
		}
		if !bytes.Equal(result.([]byte), want) {
			t.Errorf("go read %q: mismatch", k)
		}
	}

	// CGo client reads all keys.
	for k, want := range all {
		result, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			return tx.Get(cgofdb.Key(k)).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("cgo read %q: %v", k, err)
		}
		if !bytes.Equal(result.([]byte), want) {
			t.Errorf("cgo read %q: mismatch", k)
		}
	}
}

// TestInterop_AtomicAdd verifies that atomic ADD mutations are wire-compatible
// between the Go and CGo clients.
func TestInterop_AtomicAdd(t *testing.T) {
	keyGoAdd := gofdb.Key("interop_atom_go")
	keyCGoAdd := gofdb.Key("interop_atom_cgo")

	addParam := func(n int64) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, uint64(n))
		return b
	}
	readInt64 := func(b []byte) int64 {
		return int64(binary.LittleEndian.Uint64(b))
	}

	// Initialize both keys to 0 via CGo.
	_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Set(cgofdb.Key(keyGoAdd), addParam(0))
		tx.Set(cgofdb.Key(keyCGoAdd), addParam(0))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Go client does atomic ADD +7 on keyGoAdd.
	_, err = goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		tx.Add(gofdb.Key(keyGoAdd), addParam(7))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("go add: %v", err)
	}

	// CGo reads keyGoAdd — should be 7.
	result, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		return tx.Get(cgofdb.Key(keyGoAdd)).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("cgo read go-added: %v", err)
	}
	if got := readInt64(result.([]byte)); got != 7 {
		t.Errorf("go ADD read by cgo: got %d, want 7", got)
	}

	// CGo client does atomic ADD +13 on keyCGoAdd.
	_, err = cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Add(cgofdb.Key(keyCGoAdd), addParam(13))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("cgo add: %v", err)
	}

	// Go reads keyCGoAdd — should be 13.
	// Invalidate GRV cache so Go sees the CGo write (cache staleness = 100ms).
	goClient.InvalidateGRVCache()
	result, err = goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		return tx.Get(gofdb.Key(keyCGoAdd)).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("go read cgo-added: %v", err)
	}
	if got := readInt64(result.([]byte)); got != 13 {
		t.Errorf("cgo ADD read by go: got %d, want 13", got)
	}

	// Both do ADD on the same key: Go +3, CGo +5 → total 8.
	sharedKey := "interop_atom_shared"
	_, err = cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Set(cgofdb.Key(sharedKey), addParam(0))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init shared: %v", err)
	}
	_, err = goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		tx.Add(gofdb.Key(sharedKey), addParam(3))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("go add shared: %v", err)
	}
	_, err = cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Add(cgofdb.Key(sharedKey), addParam(5))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("cgo add shared: %v", err)
	}
	goClient.InvalidateGRVCache()
	result, err = goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		return tx.Get(gofdb.Key(sharedKey)).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("read shared: %v", err)
	}
	if got := readInt64(result.([]byte)); got != 8 {
		t.Errorf("shared atomic ADD: got %d, want 8", got)
	}
}

// TestInterop_ClearRange writes 10 keys via Go, clears a sub-range via CGo,
// then Go reads back to verify the cleared keys are gone.
func TestInterop_ClearRange(t *testing.T) {
	prefix := "interop_cr_"

	// Go writes 10 keys: interop_cr_00 .. interop_cr_09.
	_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set(gofdb.Key(fmt.Sprintf("%s%02d", prefix, i)), []byte(fmt.Sprintf("val_%02d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("go write: %v", err)
	}

	// CGo clears range [03, 07) — keys 03, 04, 05, 06 should be gone.
	_, err = cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.ClearRange(cgofdb.KeyRange{
			Begin: cgofdb.Key(fmt.Sprintf("%s%02d", prefix, 3)),
			End:   cgofdb.Key(fmt.Sprintf("%s%02d", prefix, 7)),
		})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("cgo clear range: %v", err)
	}

	// Go reads all 10 keys; 03-06 should be nil.
	goClient.InvalidateGRVCache()
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("%s%02d", prefix, i)
		result, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			return tx.Get(gofdb.Key(key)).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("go read %q: %v", key, err)
		}
		cleared := i >= 3 && i < 7
		if cleared && result.([]byte) != nil {
			t.Errorf("key %q should be cleared but got %q", key, result.([]byte))
		}
		if !cleared && result.([]byte) == nil {
			t.Errorf("key %q should exist but is nil", key)
		}
	}
}

// TestInterop_GetRange writes 100 keys via Go, CGo does GetRange and verifies
// all 100. Then CGo writes 100 different keys, Go does GetRange.
func TestInterop_GetRange(t *testing.T) {
	t.Run("GoWrite_CGoRange", func(t *testing.T) {
		prefix := "interop_gr_gw_"

		// Go writes 100 keys.
		_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			for i := 0; i < 100; i++ {
				tx.Set(gofdb.Key(fmt.Sprintf("%s%04d", prefix, i)), []byte(fmt.Sprintf("v%04d", i)))
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("go write: %v", err)
		}

		// CGo reads all 100 via GetRange.
		result, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			rr := tx.GetRange(cgofdb.KeyRange{
				Begin: cgofdb.Key(prefix + "0000"),
				End:   cgofdb.Key(prefix + "9999"),
			}, cgofdb.RangeOptions{})
			return rr.GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("cgo getrange: %v", err)
		}
		kvs := result.([]cgofdb.KeyValue)
		if len(kvs) != 100 {
			t.Fatalf("cgo getrange: got %d keys, want 100", len(kvs))
		}
		for i, kv := range kvs {
			wantKey := fmt.Sprintf("%s%04d", prefix, i)
			wantVal := fmt.Sprintf("v%04d", i)
			if string(kv.Key) != wantKey {
				t.Errorf("kv[%d] key: got %q, want %q", i, kv.Key, wantKey)
			}
			if string(kv.Value) != wantVal {
				t.Errorf("kv[%d] val: got %q, want %q", i, kv.Value, wantVal)
			}
		}
	})

	t.Run("CGoWrite_GoRange", func(t *testing.T) {
		prefix := "interop_gr_cw_"

		// CGo writes 100 keys.
		_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			for i := 0; i < 100; i++ {
				tx.Set(cgofdb.Key(fmt.Sprintf("%s%04d", prefix, i)), []byte(fmt.Sprintf("v%04d", i)))
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("cgo write: %v", err)
		}

		// Go reads all 100 via GetRange. Invalidate cache to see CGo writes.
		goClient.InvalidateGRVCache()
		result, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			rr := tx.GetRange(gofdb.KeyRange{
				Begin: gofdb.Key(prefix + "0000"),
				End:   gofdb.Key(prefix + "9999"),
			}, gofdb.RangeOptions{})
			return rr.GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("go getrange: %v", err)
		}
		kvs := result.([]gofdb.KeyValue)
		if len(kvs) != 100 {
			t.Fatalf("go getrange: got %d keys, want 100", len(kvs))
		}
		for i, kv := range kvs {
			wantKey := fmt.Sprintf("%s%04d", prefix, i)
			wantVal := fmt.Sprintf("v%04d", i)
			if string(kv.Key) != wantKey {
				t.Errorf("kv[%d] key: got %q, want %q", i, kv.Key, wantKey)
			}
			if string(kv.Value) != wantVal {
				t.Errorf("kv[%d] val: got %q, want %q", i, kv.Value, wantVal)
			}
		}
	})
}

// TestInterop_Versionstamp writes a versionstamped key via Go, then CGo reads
// it back and verifies the key contains a valid 10-byte versionstamp.
func TestInterop_Versionstamp(t *testing.T) {
	prefix := []byte("interop_vs_")
	// Build the versionstamped key template: prefix + 10 zero bytes (placeholder) + 4-byte offset (little-endian).
	// The offset points to where the versionstamp placeholder starts (= len(prefix)).
	vsKeyTemplate := make([]byte, 0, len(prefix)+10+4)
	vsKeyTemplate = append(vsKeyTemplate, prefix...)
	vsKeyTemplate = append(vsKeyTemplate, make([]byte, 10)...) // 10 placeholder bytes
	offset := make([]byte, 4)
	binary.LittleEndian.PutUint32(offset, uint32(len(prefix)))
	vsKeyTemplate = append(vsKeyTemplate, offset...)

	value := []byte("versionstamped_value")

	// Write via Go client using SetVersionstampedKey.
	_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		tx.SetVersionstampedKey(gofdb.Key(vsKeyTemplate), value)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("go versionstamp write: %v", err)
	}

	// CGo reads the range under prefix to find the materialized key.
	result, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		rr := tx.GetRange(cgofdb.KeyRange{
			Begin: cgofdb.Key(prefix),
			End:   cgofdb.Key(append(append([]byte{}, prefix...), 0xFF)),
		}, cgofdb.RangeOptions{Limit: 1})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("cgo versionstamp read: %v", err)
	}

	kvs := result.([]cgofdb.KeyValue)
	if len(kvs) != 1 {
		t.Fatalf("expected 1 versionstamped key, got %d", len(kvs))
	}

	key := kvs[0].Key
	// Key should be: prefix (11 bytes) + versionstamp (10 bytes) = 21 bytes.
	expectedLen := len(prefix) + 10
	if len(key) != expectedLen {
		t.Fatalf("versionstamped key length: got %d, want %d", len(key), expectedLen)
	}

	// Verify prefix is intact.
	if !bytes.Equal(key[:len(prefix)], prefix) {
		t.Errorf("prefix mismatch: got %q", key[:len(prefix)])
	}

	// Verify the 10-byte versionstamp is not all zeros (would mean it was never stamped).
	vs := key[len(prefix):]
	allZero := true
	for _, b := range vs {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("versionstamp is all zeros — was not materialized")
	}

	// Verify value matches.
	if !bytes.Equal(kvs[0].Value, value) {
		t.Errorf("value mismatch: got %q, want %q", kvs[0].Value, value)
	}
}

// TestInterop_ConflictDetection verifies that conflict ranges are wire-
// compatible: a Go tx and a CGo tx that both read+write the same key cause
// exactly one to fail with not_committed (1020).
func TestInterop_ConflictDetection(t *testing.T) {
	conflictKey := "interop_conflict_key"

	// Seed the key so reads don't see "not found" (which wouldn't add a conflict range).
	_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Set(cgofdb.Key(conflictKey), []byte("seed"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Create manual transactions so we control commit ordering.
	goTx, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go create tx: %v", err)
	}

	cgoTx, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo create tx: %v", err)
	}

	// Both read the same key (establishes read conflict ranges).
	goTx.Get(gofdb.Key(conflictKey)).MustGet()
	cgoTx.Get(cgofdb.Key(conflictKey)).MustGet()

	// Both write to the same key.
	goTx.Set(gofdb.Key(conflictKey), []byte("from_go"))
	cgoTx.Set(cgofdb.Key(conflictKey), []byte("from_cgo"))

	// Commit Go first.
	goErr := goTx.Commit().Get()

	// Commit CGo second — should conflict.
	cgoErr := cgoTx.Commit().Get()

	// Exactly one should succeed and one should fail with not_committed (1020).
	goOK := goErr == nil
	cgoOK := cgoErr == nil

	if goOK == cgoOK {
		t.Fatalf("expected exactly one conflict: goErr=%v, cgoErr=%v", goErr, cgoErr)
	}

	// Verify the failing one is error code 1020 (not_committed).
	var failErr error
	if !goOK {
		failErr = goErr
	} else {
		failErr = cgoErr
	}

	// Check both error types since the failing tx could be either client.
	isConflict := false
	var goFDBErr gofdb.Error
	var cgoFDBErr cgofdb.Error
	if ok := errorAs(failErr, &goFDBErr); ok && goFDBErr.Code == 1020 {
		isConflict = true
	}
	if ok := errorAs(failErr, &cgoFDBErr); ok && cgoFDBErr.Code == 1020 {
		isConflict = true
	}

	if !isConflict {
		t.Fatalf("conflict error should be code 1020, got: %v", failErr)
	}
}

// errorAs is a generic helper that avoids importing errors for a simple
// type assertion (both Error types are concrete structs, not wrapped).
func errorAs[T any](err error, target *T) bool {
	if err == nil {
		return false
	}
	e, ok := err.(T)
	if ok {
		*target = e
	}
	return ok
}
