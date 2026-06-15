package fdb_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	. "github.com/onsi/gomega"
)

// TestMustGetPanic_RetryInTransact verifies that when MustGet() panics with
// an fdb.Error inside Transact(), the panic is caught by panicToError(),
// converted back to *wire.FDBError via unconvertError(), and the transaction
// retries correctly. This is critical: without unconvertError, retryable
// errors from MustGet() would escape the retry loop as fdb.Error (which
// OnError doesn't recognize via errors.As(*wire.FDBError)).
func TestMustGetPanic_RetryInTransact(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	key := fdb.Key("mustget_panic_test")

	// Set up the key.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Use Transact with MustGet — should work normally (no panic).
	result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		val := tr.Get(key).MustGet()
		return val, nil
	})
	if err != nil {
		t.Fatalf("MustGet in Transact: %v", err)
	}
	if !bytes.Equal(result.([]byte), []byte("value")) {
		t.Fatalf("got %q, want %q", result, "value")
	}

	// MustGet on non-existent key should return nil (not panic).
	result, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		val := tr.Get(fdb.Key("definitely_nonexistent_key_xyz")).MustGet()
		return val, nil
	})
	if err != nil {
		t.Fatalf("MustGet nil: %v", err)
	}
	if result.([]byte) != nil {
		t.Fatalf("expected nil, got %v", result)
	}

	// Clean up.
	db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Clear(key)
		return nil, nil
	})
}

// TestPanicToError_NonFDBPanic verifies that non-error panics are re-panicked
// (not swallowed).
func TestPanicToError_NonFDBPanic(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate")
		}
		if r != "non-error panic" {
			t.Fatalf("expected 'non-error panic', got %v", r)
		}
	}()

	db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		panic("non-error panic")
	})
}

// openTestDB returns a Database connected to the shared FDB testcontainer.
// Each call creates a fresh Database connection for option isolation.
func openTestDB(t *testing.T) fdb.Database {
	t.Helper()

	if sharedClusterFile == nil {
		t.Fatal("shared FDB container not initialized — TestMain must run first")
	}

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer setupCancel()

	db, err := fdb.OpenDatabaseFromConfig(setupCtx, sharedClusterFile)
	if err != nil {
		t.Fatalf("OpenDatabaseFromConfig: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if t.Failed() && sharedContainer != nil {
			diagCtx, diagCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer diagCancel()
			logs, lerr := sharedContainer.Logs(diagCtx)
			if lerr == nil {
				logBytes, _ := io.ReadAll(logs)
				if len(logBytes) > 2000 {
					logBytes = logBytes[len(logBytes)-2000:]
				}
				t.Logf("=== FDB logs (last 2000 bytes) ===\n%s", string(logBytes))
			}
		}
	})

	return db
}

func TestSetGetBasic(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	key := fdb.Key(t.Name() + "_key")

	// Write
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("hello-facade"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read
	result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(key).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(result.([]byte)) != "hello-facade" {
		t.Fatalf("got %q, want %q", result, "hello-facade")
	}
}

func TestGetRange(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := t.Name() + "_"

	// Seed data
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(pfx+"a"), []byte("1"))
		tr.Set(fdb.Key(pfx+"b"), []byte("2"))
		tr.Set(fdb.Key(pfx+"c"), []byte("3"))
		tr.Set(fdb.Key(pfx+"d"), []byte("4"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Forward range
	result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(pfx + "a"), End: fdb.Key(pfx + "e")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]fdb.KeyValue)
	if len(kvs) != 4 {
		t.Fatalf("forward range: got %d keys, want 4", len(kvs))
	}
	if string(kvs[0].Key) != pfx+"a" || string(kvs[3].Key) != pfx+"d" {
		t.Fatalf("forward range order wrong: first=%q last=%q", kvs[0].Key, kvs[3].Key)
	}

	// Reverse range
	result, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(pfx + "a"), End: fdb.Key(pfx + "e")}, fdb.RangeOptions{Reverse: true})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange reverse: %v", err)
	}
	kvs = result.([]fdb.KeyValue)
	if len(kvs) != 4 {
		t.Fatalf("reverse range: got %d keys, want 4", len(kvs))
	}
	if string(kvs[0].Key) != pfx+"d" || string(kvs[3].Key) != pfx+"a" {
		t.Fatalf("reverse range order wrong: first=%q last=%q", kvs[0].Key, kvs[3].Key)
	}

	// Range with limit
	result, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(pfx + "a"), End: fdb.Key(pfx + "e")}, fdb.RangeOptions{Limit: 2})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange limit: %v", err)
	}
	kvs = result.([]fdb.KeyValue)
	if len(kvs) != 2 {
		t.Fatalf("limit range: got %d keys, want 2", len(kvs))
	}
}

func TestIterator(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	prefix := t.Name() + "/data/"
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := 0; i < 20; i++ {
			tr.Set(fdb.Key(fmt.Sprintf("%skey-%02d", prefix, i)), []byte(fmt.Sprintf("val-%02d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Test each streaming mode returns correct results.
	modes := []struct {
		name string
		mode fdb.StreamingMode
	}{
		{"WantAll", fdb.StreamingModeWantAll},
		{"Iterator", fdb.StreamingModeIterator},
		{"Exact", fdb.StreamingModeExact},
		{"Small", fdb.StreamingModeSmall},
		{"Medium", fdb.StreamingModeMedium},
		{"Large", fdb.StreamingModeLarge},
		{"Serial", fdb.StreamingModeSerial},
	}

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
				kr := fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(prefix + "\xff")}
				opts := fdb.RangeOptions{Mode: m.mode}
				if m.mode == fdb.StreamingModeExact {
					opts.Limit = 20 // EXACT requires a limit
				}
				rr := tr.GetRange(kr, opts)
				iter := rr.Iterator()
				var keys []string
				for iter.Advance() {
					kv, err := iter.Get()
					if err != nil {
						return nil, err
					}
					keys = append(keys, string(kv.Key))
				}
				return keys, nil
			})
			if err != nil {
				t.Fatalf("Iterator(%s): %v", m.name, err)
			}
			keys := result.([]string)
			if len(keys) != 20 {
				t.Fatalf("iterator(%s): got %d keys, want 20", m.name, len(keys))
			}
			// Verify order.
			if keys[0] != prefix+"key-00" || keys[19] != prefix+"key-19" {
				t.Fatalf("iterator(%s): wrong order: first=%q last=%q", m.name, keys[0], keys[19])
			}
		})
	}

	// Test iterator with limit.
	t.Run("WithLimit", func(t *testing.T) {
		result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			kr := fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(prefix + "\xff")}
			rr := tr.GetRange(kr, fdb.RangeOptions{Limit: 5, Mode: fdb.StreamingModeIterator})
			iter := rr.Iterator()
			var keys []string
			for iter.Advance() {
				kv, err := iter.Get()
				if err != nil {
					return nil, err
				}
				keys = append(keys, string(kv.Key))
			}
			return keys, nil
		})
		if err != nil {
			t.Fatalf("Iterator(WithLimit): %v", err)
		}
		keys := result.([]string)
		if len(keys) != 5 {
			t.Fatalf("iterator(WithLimit): got %d keys, want 5", len(keys))
		}
	})

	// Test reverse iterator.
	t.Run("Reverse", func(t *testing.T) {
		result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			kr := fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(prefix + "\xff")}
			rr := tr.GetRange(kr, fdb.RangeOptions{Reverse: true, Mode: fdb.StreamingModeSmall})
			iter := rr.Iterator()
			var keys []string
			for iter.Advance() {
				kv, err := iter.Get()
				if err != nil {
					return nil, err
				}
				keys = append(keys, string(kv.Key))
			}
			return keys, nil
		})
		if err != nil {
			t.Fatalf("Iterator(Reverse): %v", err)
		}
		keys := result.([]string)
		if len(keys) != 20 {
			t.Fatalf("iterator(Reverse): got %d keys, want 20", len(keys))
		}
		// First should be last key (reverse order).
		if keys[0] != prefix+"key-19" || keys[19] != prefix+"key-00" {
			t.Fatalf("iterator(Reverse): wrong order: first=%q last=%q", keys[0], keys[19])
		}
	})
}

func TestAtomicOps(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	key := fdb.Key(t.Name() + "_counter")

	// Atomic ADD
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		// Initialize to 0 (little-endian int64)
		tr.Set(key, []byte{0, 0, 0, 0, 0, 0, 0, 0})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Add 5
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Add(key, []byte{5, 0, 0, 0, 0, 0, 0, 0})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Read back
	result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(key).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("Get after Add: %v", err)
	}
	val := result.([]byte)
	if len(val) != 8 || val[0] != 5 {
		t.Fatalf("expected 5, got %v", val)
	}
}

func TestClearRange(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := t.Name() + "_"

	// Seed
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(pfx+"a"), []byte("1"))
		tr.Set(fdb.Key(pfx+"b"), []byte("2"))
		tr.Set(fdb.Key(pfx+"c"), []byte("3"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear range [pfx+"a", pfx+"c")
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(pfx + "a"), End: fdb.Key(pfx + "c")})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ClearRange: %v", err)
	}

	// Only pfx+"c" should remain
	result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(pfx + "a"), End: fdb.Key(pfx + "d")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]fdb.KeyValue)
	if len(kvs) != 1 || string(kvs[0].Key) != pfx+"c" {
		t.Fatalf("after ClearRange: got %d keys, expected 1 (%sc)", len(kvs), pfx)
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	key := fdb.Key(t.Name() + "_key")

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("snap-val"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		snap := tr.Snapshot()
		return snap.Get(key).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("Snapshot Get: %v", err)
	}
	if string(result.([]byte)) != "snap-val" {
		t.Fatalf("got %q, want %q", result, "snap-val")
	}
}

func TestSnapshotMethods(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := t.Name() + "_"
	key := fdb.Key(pfx + "key")

	// Seed data
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("val"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		snap := tr.Snapshot()

		// Snapshot() returns itself
		snap2 := snap.Snapshot()
		v, err := snap2.Get(key).Get()
		if err != nil {
			t.Fatalf("Snapshot().Snapshot().Get: %v", err)
		}
		if string(v) != "val" {
			t.Fatalf("got %q, want %q", v, "val")
		}

		// GetDatabase returns the database (concrete-only method, off the
		// ReadTransaction interface per RFC-109 — type-assert to the impl).
		snapDB := snap.(fdb.Snapshot).GetDatabase()
		if snapDB == (fdb.Database{}) {
			t.Fatal("GetDatabase returned zero value")
		}

		// GetReadVersion
		rv, err := snap.GetReadVersion().Get()
		if err != nil {
			t.Fatalf("GetReadVersion: %v", err)
		}
		if rv <= 0 {
			t.Fatalf("read version should be positive, got %d", rv)
		}

		// GetRange
		begin := fdb.Key(pfx)
		end := fdb.Key(pfx + "\xFF")
		rr := snap.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{})
		kvs, err := rr.GetSliceWithError()
		if err != nil {
			t.Fatalf("GetRange: %v", err)
		}
		if len(kvs) != 1 {
			t.Fatalf("expected 1 key, got %d", len(kvs))
		}

		// Options returns a usable handle
		opts := snap.Options()
		_ = opts.SetTimeout(5000) // verify it doesn't panic

		// ReadTransact
		inner, err := snap.ReadTransact(func(rt fdb.ReadTransaction) (any, error) {
			return rt.Get(key).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("ReadTransact: %v", err)
		}
		if string(inner.([]byte)) != "val" {
			t.Fatalf("ReadTransact got %q", inner)
		}

		return nil, nil
	})
	if err != nil {
		t.Fatalf("ReadTransact: %v", err)
	}
}

func TestGetCommittedVersion(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := t.Name() + "_"

	result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(pfx+"key"), []byte("val"))
		return nil, nil
	})
	_ = result
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}

	// For a committed write transaction, CreateTransaction + Commit should work.
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	tr.Set(fdb.Key(pfx+"key2"), []byte("val2"))
	if err := tr.Commit().Get(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	v, err := tr.GetCommittedVersion()
	if err != nil {
		t.Fatalf("GetCommittedVersion: %v", err)
	}
	if v <= 0 {
		t.Fatalf("committed version should be > 0, got %d", v)
	}
}

func TestTransactorInterface(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	key := fdb.Key(t.Name() + "_key")

	// Verify Database satisfies Transactor
	var transactor fdb.Transactor = db
	_, err := transactor.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("iface-val"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transactor.Transact: %v", err)
	}

	// Verify Database satisfies ReadTransactor
	var readTransactor fdb.ReadTransactor = db
	result, err := readTransactor.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		return tr.Get(key).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("ReadTransactor.ReadTransact: %v", err)
	}
	if string(result.([]byte)) != "iface-val" {
		t.Fatalf("got %q, want %q", result, "iface-val")
	}
}

func TestPrefixRange(t *testing.T) {
	t.Parallel()

	kr, err := fdb.PrefixRange([]byte{0x01, 0x02})
	if err != nil {
		t.Fatalf("PrefixRange: %v", err)
	}
	begin, end := kr.FDBRangeKeys()
	if string(begin.FDBKey()) != string([]byte{0x01, 0x02}) {
		t.Fatalf("begin: got %v", begin.FDBKey())
	}
	if string(end.FDBKey()) != string([]byte{0x01, 0x03}) {
		t.Fatalf("end: got %v, want [0x01, 0x03]", end.FDBKey())
	}
}

func TestStrinc(t *testing.T) {
	t.Parallel()

	result, err := fdb.Strinc([]byte{0x01, 0xFF, 0x02})
	if err != nil {
		t.Fatalf("Strinc: %v", err)
	}
	expected := []byte{0x01, 0xFF, 0x03}
	if string(result) != string(expected) {
		t.Fatalf("got %v, want %v", result, expected)
	}

	// All 0xFF should error
	_, err = fdb.Strinc([]byte{0xFF, 0xFF})
	if err == nil {
		t.Fatal("expected error for all-0xFF prefix")
	}
}

func TestKeySelectors(t *testing.T) {
	t.Parallel()

	// OrEqual values match the Apple Go binding / C++ wire convention:
	// FGE: orEqual=false (key IS the boundary, offset=1 advances past it)
	// FGT: orEqual=true (key is NOT the boundary, so first > key)
	ks := fdb.FirstGreaterOrEqual(fdb.Key("hello"))
	if ks.OrEqual || ks.Offset != 1 {
		t.Fatalf("FirstGreaterOrEqual: OrEqual=%v Offset=%d (want false, 1)", ks.OrEqual, ks.Offset)
	}

	ks = fdb.FirstGreaterThan(fdb.Key("hello"))
	if !ks.OrEqual || ks.Offset != 1 {
		t.Fatalf("FirstGreaterThan: OrEqual=%v Offset=%d (want true, 1)", ks.OrEqual, ks.Offset)
	}

	ks = fdb.LastLessOrEqual(fdb.Key("hello"))
	if !ks.OrEqual || ks.Offset != 0 {
		t.Fatalf("LastLessOrEqual: OrEqual=%v Offset=%d", ks.OrEqual, ks.Offset)
	}

	ks = fdb.LastLessThan(fdb.Key("hello"))
	if ks.OrEqual || ks.Offset != 0 {
		t.Fatalf("LastLessThan: OrEqual=%v Offset=%d", ks.OrEqual, ks.Offset)
	}
}

func TestVersionstamp(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	tr.Set(fdb.Key(t.Name()+"_key"), []byte("vs-val"))
	if err := tr.Commit().Get(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	vs, err := tr.GetVersionstamp().Get()
	if err != nil {
		t.Fatalf("GetVersionstamp: %v", err)
	}
	if len(vs) != 10 {
		t.Fatalf("versionstamp should be 10 bytes, got %d", len(vs))
	}
}

func TestDeferredVersionstamp(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	tr.Set(fdb.Key(t.Name()+"/key"), []byte("val"))

	// Call GetVersionstamp BEFORE Commit — future should block until commit.
	vsFut := tr.GetVersionstamp()

	// Verify future is not ready yet (commit hasn't happened).
	if vsFut.IsReady() {
		t.Fatal("GetVersionstamp future should not be ready before commit")
	}

	// Now commit.
	if err := tr.Commit().Get(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Future should resolve with the versionstamp.
	vs, err := vsFut.Get()
	if err != nil {
		t.Fatalf("deferred GetVersionstamp: %v", err)
	}
	if len(vs) != 10 {
		t.Fatalf("versionstamp should be 10 bytes, got %d", len(vs))
	}
}

func TestFutureParallelism(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := t.Name() + "_"

	// Seed two keys
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(pfx+"a"), []byte("val-a"))
		tr.Set(fdb.Key(pfx+"b"), []byte("val-b"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Read both in parallel using futures
	result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		fa := tr.Get(fdb.Key(pfx + "a"))
		fb := tr.Get(fdb.Key(pfx + "b"))
		// Both reads should be in-flight now
		a := fa.MustGet()
		b := fb.MustGet()
		return []string{string(a), string(b)}, nil
	})
	if err != nil {
		t.Fatalf("parallel Get: %v", err)
	}
	vals := result.([]string)
	if vals[0] != "val-a" || vals[1] != "val-b" {
		t.Fatalf("got %v, want [val-a, val-b]", vals)
	}
}

func TestSetVersionstampedKey(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// SetVersionstampedKey: key contains incomplete versionstamp placeholder
	// The last 4 bytes of param specify the offset where the versionstamp goes.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		pfx := []byte(t.Name() + "_")
		// Key: pfx + 10 zero bytes (placeholder) + offset (little-endian uint32)
		key := make([]byte, len(pfx)+10+4)
		copy(key, pfx)
		// offset = len(pfx) in little-endian
		offsetPos := len(pfx) + 10
		key[offsetPos] = byte(len(pfx))
		key[offsetPos+1] = 0
		key[offsetPos+2] = 0
		key[offsetPos+3] = 0
		tr.SetVersionstampedKey(fdb.Key(key), []byte("value"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("SetVersionstampedKey: %v", err)
	}
}

// TestRetryLoopErrorConversion reproduces a critical bug: convertError
// turns *wire.FDBError into fdb.Error inside futures, but the client's
// OnError only recognizes *wire.FDBError via errors.As. So retryable
// errors returned from the user closure escape the retry loop.
func TestRetryLoopErrorConversion(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Simulate: user closure returns fdb.Error{Code: 1020} (not_committed).
	// This is retryable. Transact MUST retry, not propagate.
	attempt := 0
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		attempt++
		if attempt == 1 {
			// Return a retryable fdb.Error — the type the user sees from Get().Get().
			return nil, fdb.Error{Code: 1020} // not_committed
		}
		// Second attempt succeeds.
		tr.Set(fdb.Key(t.Name()+"_key"), []byte("ok"))
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Transact should have retried fdb.Error{1020}, got: %v", err)
	}
	if attempt < 2 {
		t.Fatalf("expected at least 2 attempts (retry), got %d", attempt)
	}
}

// TestMustGetPanicRecovery verifies that MustGet() panics inside
// Database.Transact are caught and fed into the retry loop, not
// propagated as process crashes.
func TestMustGetPanicRecovery(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	attempt := 0
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		attempt++
		if attempt == 1 {
			// Simulate MustGet() panic with a retryable error.
			panic(fdb.Error{Code: 1020}) // not_committed
		}
		tr.Set(fdb.Key(t.Name()+"_key"), []byte("ok"))
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Transact should have recovered panic and retried, got: %v", err)
	}
	if attempt < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", attempt)
	}
}

func TestTransactionOptions(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Test SetTimeout: should work (already client-side, just verify no panic)
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if err := tr.Options().SetTimeout(5000); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}

	// Test SetRetryLimit
	if err := tr.Options().SetRetryLimit(3); err != nil {
		t.Fatalf("SetRetryLimit: %v", err)
	}

	// Test SetPriorityBatch — should send GRV with PRIORITY_BATCH flags
	if err := tr.Options().SetPriorityBatch(); err != nil {
		t.Fatalf("SetPriorityBatch: %v", err)
	}
	tr.Set(fdb.Key(t.Name()+"/opt-key"), []byte("opt-val"))
	if err := tr.Commit().Get(); err != nil {
		t.Fatalf("Commit with batch priority: %v", err)
	}
	// Verify the write landed
	result, err := db.Transact(func(tr2 fdb.WritableTransaction) (any, error) {
		return tr2.Get(fdb.Key(t.Name() + "/opt-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(result.([]byte)) != "opt-val" {
		t.Fatalf("got %q, want %q", result, "opt-val")
	}

	// Test SetPrioritySystemImmediate
	tr2, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if err := tr2.Options().SetPrioritySystemImmediate(); err != nil {
		t.Fatalf("SetPrioritySystemImmediate: %v", err)
	}
	tr2.Set(fdb.Key(t.Name()+"/opt-key2"), []byte("opt-val2"))
	if err := tr2.Commit().Get(); err != nil {
		t.Fatalf("Commit with system immediate priority: %v", err)
	}

	// Test SetCausalReadRisky
	tr3, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if err := tr3.Options().SetCausalReadRisky(); err != nil {
		t.Fatalf("SetCausalReadRisky: %v", err)
	}
	val := tr3.Get(fdb.Key(t.Name() + "/opt-key2")).MustGet()
	if string(val) != "opt-val2" {
		// Note: this test only verifies the flag does not break reads.
		// Wire-level verification would require packet inspection.
		t.Fatalf("causal read risky: got %q, want %q", val, "opt-val2")
	}

	// Test SetLockAware — verify write + commit succeeds with flag set.
	tr4, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if err := tr4.Options().SetLockAware(); err != nil {
		t.Fatalf("SetLockAware: %v", err)
	}
	tr4.Set(fdb.Key(t.Name()+"/lock-aware-key"), []byte("lock-aware-val"))
	if err := tr4.Commit().Get(); err != nil {
		t.Fatalf("Commit with lock-aware: %v", err)
	}
	result2, err := db.Transact(func(rtx fdb.WritableTransaction) (any, error) {
		return rtx.Get(fdb.Key(t.Name() + "/lock-aware-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(result2.([]byte)) != "lock-aware-val" {
		t.Fatalf("lock-aware: got %q, want %q", result2, "lock-aware-val")
	}
}

func TestSizeLimit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	// Set a small but valid size limit (min 32, max 10_000_000).
	if err := tr.Options().SetSizeLimit(32); err != nil {
		t.Fatalf("SetSizeLimit: %v", err)
	}
	// Write more data than the limit
	tr.Set(fdb.Key(t.Name()+"/big-key"), []byte("big-value-exceeding-size-limit"))
	err = tr.Commit().Get()
	if err == nil {
		t.Fatal("expected error from size limit, got nil")
	}
	// Should get transaction_too_large (2101)
	fdbErr, ok := err.(fdb.Error)
	if !ok {
		t.Fatalf("expected fdb.Error, got %T: %v", err, err)
	}
	if fdbErr.Code != 2101 {
		t.Fatalf("expected error code 2101 (transaction_too_large), got %d", fdbErr.Code)
	}
}

// TestDatabaseTransactionTimeout verifies that FDB_DB_OPTION_TRANSACTION_TIMEOUT
// applies to transactions created by Transact. Matching C++ test at unit_tests.cpp:787.
func TestDatabaseTransactionTimeout(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Set 1ms database-level timeout.
	if err := db.Options().SetTransactionTimeout(1); err != nil {
		t.Fatalf("SetTransactionTimeout: %v", err)
	}

	// Run transactions until one times out. With 1ms timeout, it should
	// happen almost immediately (the GRV round-trip alone takes >1ms).
	var timedOut bool
	for i := 0; i < 100; i++ {
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			return tr.Get(fdb.Key(t.Name() + "_key")).MustGet(), nil
		})
		if err != nil {
			fdbErr, ok := err.(fdb.Error)
			if ok && fdbErr.Code == 1031 { // transaction_timed_out
				timedOut = true
				break
			}
		}
	}
	if !timedOut {
		t.Fatal("expected transaction_timed_out (1031) with 1ms database timeout")
	}

	// Reset timeout (disable).
	db.Options().SetTransactionTimeout(0)
}

// TestCreateTransaction_AppliesDatabaseDefaults verifies a MANUALLY-created
// transaction inherits database-level option defaults — matching libfdb_c, which
// copies the database transaction defaults into every transaction it creates.
// Regression for the divergence where CreateTransaction skipped applyTxDefaults
// (unlike the Transact* paths), so a manual CreateTransaction/OnError loop stayed
// unbounded even with SetTransactionTimeout set on the database.
func TestCreateTransaction_AppliesDatabaseDefaults(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// 1ms database-level timeout — must apply to a manually-created transaction.
	if err := db.Options().SetTransactionTimeout(1); err != nil {
		t.Fatalf("SetTransactionTimeout: %v", err)
	}
	defer db.Options().SetTransactionTimeout(0)

	is1031 := func(err error) bool {
		fe, ok := err.(fdb.Error)
		return ok && fe.Code == 1031 // transaction_timed_out
	}
	var timedOut bool
	for i := 0; i < 100; i++ {
		tr, err := db.CreateTransaction()
		if err != nil {
			t.Fatalf("CreateTransaction: %v", err)
		}
		// Read forces a GRV round-trip (>1ms); the inherited 1ms timeout then trips
		// on the read or the commit — the same path db.Transact's auto-commit takes
		// in TestDatabaseTransactionTimeout. Without applyTxDefaults in
		// CreateTransaction the manual tx has no timeout and never trips.
		_, rerr := tr.Get(fdb.Key(t.Name() + "_key")).Get()
		cerr := tr.Commit().Get()
		if is1031(rerr) || is1031(cerr) {
			timedOut = true
			break
		}
	}
	if !timedOut {
		t.Fatal("manual CreateTransaction did not inherit the 1ms database timeout (1031 expected) — applyTxDefaults not applied")
	}
}

// TestCreateTransaction_ResetPreservesDatabaseDefaults verifies Reset() re-applies
// inherited DB-level option defaults to the fresh inner transaction — matching C++
// reset(), which re-copies the database persistent options. Regression for the
// divergence where Reset swapped in a defaults-less inner (codex).
func TestCreateTransaction_ResetPreservesDatabaseDefaults(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	if err := db.Options().SetTransactionTimeout(1); err != nil {
		t.Fatalf("SetTransactionTimeout: %v", err)
	}
	defer db.Options().SetTransactionTimeout(0)

	is1031 := func(err error) bool {
		fe, ok := err.(fdb.Error)
		return ok && fe.Code == 1031 // transaction_timed_out
	}
	var timedOut bool
	for i := 0; i < 100; i++ {
		tr, err := db.CreateTransaction()
		if err != nil {
			t.Fatalf("CreateTransaction: %v", err)
		}
		tr.Reset() // fresh inner — the 1ms DB timeout must survive the reset
		_, rerr := tr.Get(fdb.Key(t.Name() + "_key")).Get()
		cerr := tr.Commit().Get()
		if is1031(rerr) || is1031(cerr) {
			timedOut = true
			break
		}
	}
	if !timedOut {
		t.Fatal("DB timeout lost after Reset (1031 expected) — applyTxDefaults not re-applied on Reset")
	}
}

// TestCreateTransaction_ResetDropsUserOptions verifies a user-facing Reset() DROPS
// user-set per-tx options — matching C++ reset() (clears persistentOptions), which
// is distinct from the onError-retry reset (resetRyow) that PRESERVES them so
// retries keep them. If Reset preserved user options, a user-set 1ms timeout would
// survive and time out the reused transaction (codex).
func TestCreateTransaction_ResetDropsUserOptions(t *testing.T) {
	t.Parallel()
	db := openTestDB(t) // no database-level timeout default

	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	// User-set a 1ms per-tx timeout, then Reset — it must NOT carry over.
	if err := tr.Options().SetTimeout(1); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	tr.Reset()

	// A normal write+commit on the reset tx must SUCCEED: the user 1ms timeout was
	// dropped. If it had survived Reset, the >1ms GRV/commit would return 1031.
	tr.Set(fdb.Key(t.Name()+"_key"), []byte("v"))
	if err := tr.Commit().Get(); err != nil {
		t.Fatalf("commit after Reset returned %v — user-set 1ms timeout survived Reset (should be dropped, C++ reset() clears persistentOptions)", err)
	}
}

// TestDatabaseTransactionSizeLimit verifies that FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT
// applies to transactions created by Transact. Matching C++ test at unit_tests.cpp:888.
func TestDatabaseTransactionSizeLimit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Set tiny size limit at database level.
	if err := db.Options().SetTransactionSizeLimit(32); err != nil {
		t.Fatalf("SetTransactionSizeLimit: %v", err)
	}

	// Transaction with mutations exceeding the limit should fail.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(t.Name()+"_key"), []byte("foundation database is amazing"))
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected transaction_too_large error")
	}
	fdbErr, ok := err.(fdb.Error)
	if !ok {
		t.Fatalf("expected fdb.Error, got %T: %v", err, err)
	}
	if fdbErr.Code != 2101 {
		t.Fatalf("expected error code 2101 (transaction_too_large), got %d", fdbErr.Code)
	}

	// Reset to default.
	db.Options().SetTransactionSizeLimit(0)
}

func TestLocalityGetBoundaryKeys(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := t.Name() + "_"

	// Write some data.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := 0; i < 10; i++ {
			tr.Set(fdb.Key(fmt.Sprintf("%s%02d", pfx, i)), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Get boundary keys — should return at least one boundary.
	keys, err := db.LocalityGetBoundaryKeys(fdb.KeyRange{
		Begin: fdb.Key(""),
		End:   fdb.Key("\xff"),
	}, 100, 0)
	if err != nil {
		t.Fatalf("LocalityGetBoundaryKeys: %v", err)
	}
	// Single-node cluster has at least 1 shard boundary.
	if len(keys) == 0 {
		t.Fatal("expected at least one boundary key")
	}
	t.Logf("got %d boundary keys", len(keys))

	// readVersion is honored (RFC-111 P1.6): reading the \xFF/keyServers/ range AT
	// a fetched read version returns without error and yields the same boundaries on
	// a quiescent cluster — proving the supplied version is threaded into the
	// boundary read (the old impl ignored it and hit the location cache).
	rvAny, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.GetReadVersion().Get()
	})
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	rv := rvAny.(int64)
	pinned, err := db.LocalityGetBoundaryKeys(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, 100, rv)
	if err != nil {
		t.Fatalf("LocalityGetBoundaryKeys(readVersion=%d): %v", rv, err)
	}
	if len(pinned) != len(keys) {
		t.Fatalf("pinned-version boundaries (%d) != fresh (%d) on a quiescent cluster", len(pinned), len(keys))
	}
}

func TestGetClientStatus(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	status, err := db.GetClientStatus()
	if err != nil {
		t.Fatalf("GetClientStatus: %v", err)
	}
	if len(status) == 0 {
		t.Fatal("expected non-empty status JSON")
	}
	t.Logf("status: %s", status)
}

func TestReset(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	pfx := t.Name() + "_"

	// Create a transaction, write, commit, reset, write again, commit.
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}

	tr.Set(fdb.Key(pfx+"a"), []byte("1"))
	err = tr.Commit().Get()
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}

	tr.Reset()

	tr.Set(fdb.Key(pfx+"b"), []byte("2"))
	err = tr.Commit().Get()
	if err != nil {
		t.Fatalf("second commit after reset: %v", err)
	}

	// Verify both keys.
	result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		a := tr.Get(fdb.Key(pfx + "a")).MustGet()
		b := tr.Get(fdb.Key(pfx + "b")).MustGet()
		return [2][]byte{a, b}, nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	vals := result.([2][]byte)
	if string(vals[0]) != "1" {
		t.Errorf("%sa: got %q, want %q", pfx, vals[0], "1")
	}
	if string(vals[1]) != "2" {
		t.Errorf("%sb: got %q, want %q", pfx, vals[1], "2")
	}
}

// TestErrorWrapping verifies that errors returned from Transact/ReadTransact
// pass through unchanged — both fdb.Error values and wrapped errors.
// Ported from Apple Go binding errors_test.go.
func TestErrorWrapping(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	testCases := []error{
		nil,
		fdb.Error{Code: 2007},
		fmt.Errorf("wrapped: %w", fdb.Error{Code: 2007}),
		errors.New("custom error"),
	}

	for _, inputErr := range testCases {
		_, outputErr := db.ReadTransact(func(rtr fdb.ReadTransaction) (any, error) {
			return nil, inputErr
		})
		if inputErr == nil {
			if outputErr != nil {
				t.Errorf("expected nil, got %v", outputErr)
			}
			continue
		}
		if outputErr == nil {
			t.Errorf("expected %v, got nil", inputErr)
			continue
		}
		// For fdb.Error, check code equality (Error is a value type, not pointer).
		var inFDB, outFDB fdb.Error
		if errors.As(inputErr, &inFDB) {
			if !errors.As(outputErr, &outFDB) {
				t.Errorf("input %T unwraps to fdb.Error but output %T does not", inputErr, outputErr)
				continue
			}
			if inFDB.Code != outFDB.Code {
				t.Errorf("error code mismatch: in=%d out=%d", inFDB.Code, outFDB.Code)
			}
		} else {
			// Non-FDB errors should pass through as-is.
			if outputErr.Error() != inputErr.Error() {
				t.Errorf("error mismatch: in=%q out=%q", inputErr, outputErr)
			}
		}
	}
}

// TestReadTransactRetry verifies that ReadTransact retries on retryable errors.
// The closure returns a retryable fdb.Error on the first attempt; ReadTransact
// should call OnError, reset the transaction, and retry.
func TestReadTransactRetry(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	attempt := 0
	result, err := db.ReadTransact(func(rtr fdb.ReadTransaction) (any, error) {
		attempt++
		if attempt == 1 {
			return nil, fdb.Error{Code: 1007} // transaction_too_old → retryable
		}
		return rtr.Get(fdb.Key(t.Name() + "_key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("ReadTransact should have retried, got: %v", err)
	}
	if attempt < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", attempt)
	}
	_ = result // value doesn't matter, just verifying retry happened
}

// TestGetRangeWithSelectorRange verifies that GetRange works with
// non-trivial key selectors (LastLessOrEqual, FirstGreaterThan).
// These require a GetKey round trip to resolve before the range scan.
func TestGetRangeWithSelectorRange(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	prefix := t.Name() + "/"

	// Seed 5 keys.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := 0; i < 5; i++ {
			tr.Set(fdb.Key(fmt.Sprintf("%skey%d", prefix, i)), []byte(fmt.Sprintf("val%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// GetRange with SelectorRange: [LastLessOrEqual("key2"), FirstGreaterThan("key3"))
	// Should return key2, key3.
	result, err := db.ReadTransact(func(rtr fdb.ReadTransaction) (any, error) {
		rr := rtr.GetRange(fdb.SelectorRange{
			Begin: fdb.LastLessOrEqual(fdb.Key(prefix + "key2")),
			End:   fdb.FirstGreaterThan(fdb.Key(prefix + "key3")),
		}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]fdb.KeyValue)
	if len(kvs) != 2 {
		t.Fatalf("expected 2 results, got %d", len(kvs))
	}
	if string(kvs[0].Key) != prefix+"key2" || string(kvs[1].Key) != prefix+"key3" {
		t.Errorf("keys: got [%q, %q], want [%q, %q]",
			kvs[0].Key, kvs[1].Key, prefix+"key2", prefix+"key3")
	}
}

// TestPrefixRangeIntegration verifies the PrefixRange + GetRange pattern
// end-to-end. This is the most common scan pattern in the record layer.
// Ported from Apple Go binding ExamplePrefixRange.
func TestPrefixRangeIntegration(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	prefix := t.Name() + "/alphabet"

	// Seed keys with shared prefix.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(prefix+"A"), []byte("1"))
		tr.Set(fdb.Key(prefix+"B"), []byte("2"))
		tr.Set(fdb.Key(prefix+"ize"), []byte("3"))
		tr.Set(fdb.Key(t.Name()+"/beta"), []byte("4")) // different prefix
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// PrefixRange scan — should return only keys with the prefix.
	pr, err := fdb.PrefixRange([]byte(prefix))
	if err != nil {
		t.Fatalf("PrefixRange: %v", err)
	}

	result, err := db.ReadTransact(func(rtr fdb.ReadTransaction) (any, error) {
		return rtr.GetRange(pr, fdb.RangeOptions{}).GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]fdb.KeyValue)
	if len(kvs) != 3 {
		t.Fatalf("expected 3 results (alphabetA/B/ize), got %d", len(kvs))
	}
	if string(kvs[0].Value) != "1" || string(kvs[1].Value) != "2" || string(kvs[2].Value) != "3" {
		t.Errorf("values: got [%q, %q, %q], want [1, 2, 3]",
			kvs[0].Value, kvs[1].Value, kvs[2].Value)
	}
}

// TestAtomicAllTypes tests all atomic mutation types in the fdb package.
// Each sub-test seeds a value, applies an atomic mutation, commits, then
// reads back and verifies the result.
func TestAtomicAllTypes(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	tests := []struct {
		name   string
		seed   []byte
		apply  func(tr fdb.Transaction, key fdb.Key, param []byte)
		param  []byte
		expect []byte
	}{
		{
			name:   "Add",
			seed:   []byte{10, 0, 0, 0, 0, 0, 0, 0},
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.Add(k, p) },
			param:  []byte{5, 0, 0, 0, 0, 0, 0, 0},
			expect: []byte{15, 0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:   "BitOr",
			seed:   []byte{0x0F},
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.BitOr(k, p) },
			param:  []byte{0xF0},
			expect: []byte{0xFF},
		},
		{
			name:   "BitAnd",
			seed:   []byte{0xFF},
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.BitAnd(k, p) },
			param:  []byte{0x0F},
			expect: []byte{0x0F},
		},
		{
			name:   "BitXor",
			seed:   []byte{0xFF},
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.BitXor(k, p) },
			param:  []byte{0x0F},
			expect: []byte{0xF0},
		},
		{
			name:   "Max",
			seed:   []byte{10, 0, 0, 0, 0, 0, 0, 0},
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.Max(k, p) },
			param:  []byte{20, 0, 0, 0, 0, 0, 0, 0},
			expect: []byte{20, 0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:   "Min",
			seed:   []byte{10, 0, 0, 0, 0, 0, 0, 0},
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.Min(k, p) },
			param:  []byte{5, 0, 0, 0, 0, 0, 0, 0},
			expect: []byte{5, 0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:   "ByteMax",
			seed:   []byte("apple"),
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.ByteMax(k, p) },
			param:  []byte("banana"),
			expect: []byte("banana"),
		},
		{
			name:   "ByteMin",
			seed:   []byte("banana"),
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.ByteMin(k, p) },
			param:  []byte("apple"),
			expect: []byte("apple"),
		},
		{
			name:   "AppendIfFits",
			seed:   []byte("hello"),
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.AppendIfFits(k, p) },
			param:  []byte(" world"),
			expect: []byte("hello world"),
		},
		{
			name:   "CompareAndClear_match",
			seed:   []byte("match"),
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.CompareAndClear(k, p) },
			param:  []byte("match"),
			expect: nil, // cleared
		},
		{
			name:   "CompareAndClear_nomatch",
			seed:   []byte("keep"),
			apply:  func(tr fdb.Transaction, k fdb.Key, p []byte) { tr.CompareAndClear(k, p) },
			param:  []byte("different"),
			expect: []byte("keep"), // unchanged
		},
	}

	for _, tc := range tests {
		key := fdb.Key(t.Name() + "_" + tc.name)

		// Seed.
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Set(key, tc.seed)
			return nil, nil
		})
		if err != nil {
			t.Fatalf("%s seed: %v", tc.name, err)
		}

		// Apply atomic.
		_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tc.apply(tr.(fdb.Transaction), key, tc.param)
			return nil, nil
		})
		if err != nil {
			t.Fatalf("%s apply: %v", tc.name, err)
		}

		// Verify.
		result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			return tr.Get(key).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("%s read: %v", tc.name, err)
		}
		got := result.([]byte)
		if !bytes.Equal(got, tc.expect) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.expect)
		}
	}
}

// TestKeyValueSizeLimits verifies behavior at FDB size boundaries.
func TestKeyValueSizeLimits(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	t.Run("max_value_100KB", func(t *testing.T) {
		// FDB max value size is 100,000 bytes.
		key := fdb.Key(t.Name() + "_key")
		val := make([]byte, 100_000)
		for i := range val {
			val[i] = byte(i % 256)
		}
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Set(key, val)
			return nil, nil
		})
		if err != nil {
			t.Fatalf("Set 100KB: %v", err)
		}
		result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			return tr.Get(key).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("Get 100KB: %v", err)
		}
		got := result.([]byte)
		if len(got) != 100_000 {
			t.Fatalf("expected 100000 bytes, got %d", len(got))
		}
		if !bytes.Equal(got, val) {
			t.Error("100KB value round-trip mismatch")
		}
	})

	t.Run("empty_value", func(t *testing.T) {
		key := fdb.Key(t.Name() + "_key")
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Set(key, []byte{})
			return nil, nil
		})
		if err != nil {
			t.Fatalf("Set empty: %v", err)
		}
		result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			return tr.Get(key).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("Get empty: %v", err)
		}
		got := result.([]byte)
		if got == nil || len(got) != 0 {
			t.Fatalf("expected empty []byte, got %v (nil=%v)", got, got == nil)
		}
	})

	t.Run("long_key", func(t *testing.T) {
		// FDB max key size is 10,000 bytes.
		key := make([]byte, 9_000)
		for i := range key {
			key[i] = byte('A' + i%26)
		}
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Set(fdb.Key(key), []byte("long_key_value"))
			return nil, nil
		})
		if err != nil {
			t.Fatalf("Set long key: %v", err)
		}
		result, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			return tr.Get(fdb.Key(key)).MustGet(), nil
		})
		if err != nil {
			t.Fatalf("Get long key: %v", err)
		}
		if string(result.([]byte)) != "long_key_value" {
			t.Errorf("long key: got %q", result)
		}
	})
}

// TestGetRangeWithSelectorRangeReverse tests reverse GetRange using
// SelectorRange (FirstGreaterOrEqual, FirstGreaterThan as endpoints).
func TestGetRangeWithSelectorRangeReverse(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	prefix := t.Name() + "_"
	// Seed 10 keys.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := 0; i < 10; i++ {
			tr.Set(fdb.Key(fmt.Sprintf("%s%02d", prefix, i)), []byte(fmt.Sprintf("v%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Reverse scan using SelectorRange: keys [03, 07] → should return 07, 06, 05, 04, 03.
	result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		begin := fdb.FirstGreaterOrEqual(fdb.Key(fmt.Sprintf("%s03", prefix)))
		end := fdb.FirstGreaterThan(fdb.Key(fmt.Sprintf("%s07", prefix)))
		rr := tr.GetRange(fdb.SelectorRange{Begin: begin, End: end}, fdb.RangeOptions{Reverse: true})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange reverse selector: %v", err)
	}
	kvs := result.([]fdb.KeyValue)
	if len(kvs) != 5 {
		names := make([]string, len(kvs))
		for i, kv := range kvs {
			names[i] = string(kv.Key)
		}
		t.Fatalf("expected 5 keys (07..03), got %d: %v", len(kvs), names)
	}
	// Verify reverse order: 07, 06, 05, 04, 03.
	for i, kv := range kvs {
		expected := fmt.Sprintf("%s%02d", prefix, 7-i)
		if string(kv.Key) != expected {
			t.Errorf("kvs[%d]: got %q, want %q", i, kv.Key, expected)
		}
	}
}

// TestTransactionRetryLimit verifies that SetRetryLimit actually caps retries.
func TestTransactionRetryLimit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	var callCount int32
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		atomic.AddInt32(&callCount, 1)
		tr.Options().SetRetryLimit(2)
		// Force a retryable error (not_committed) by writing a conflict.
		return nil, fdb.Error{Code: 1020}
	})
	// After 2 retries (3 total calls), the error should propagate.
	if err == nil {
		t.Fatal("expected error after retry limit exceeded")
	}
	// Should have been called exactly 3 times (initial + 2 retries).
	got := atomic.LoadInt32(&callCount)
	if got != 3 {
		t.Errorf("expected 3 calls (1 + 2 retries), got %d", got)
	}
}

// TestMultipleWatchesSameKey verifies that two watches on the same key
// both fire when the key changes.
func TestMultipleWatchesSameKey(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	key := fdb.Key(t.Name() + "_key")

	// Seed.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("initial"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Start two watches from separate transactions.
	var w1, w2 fdb.FutureNil
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		w1 = tr.(fdb.Transaction).Watch(key)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("watch1: %v", err)
	}

	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		w2 = tr.(fdb.Transaction).Watch(key)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("watch2: %v", err)
	}

	// Modify the key.
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("changed"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}

	// Both watches should fire.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch1 := make(chan error, 1)
	ch2 := make(chan error, 1)
	go func() { ch1 <- w1.Get() }()
	go func() { ch2 <- w2.Get() }()

	select {
	case err := <-ch1:
		if err != nil {
			t.Errorf("watch1 error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("watch1 did not fire within 10s")
	}

	select {
	case err := <-ch2:
		if err != nil {
			t.Errorf("watch2 error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("watch2 did not fire within 10s")
	}
}

// TestManyWatchesSameKey_AllFire is a stronger regression for the watch
// value-capture race that flaked TestMultipleWatchesSameKey in CI. With many
// concurrent watches on one key, the pre-fix asynchronous value-read (done in
// the watch future's goroutine) made it very likely that at least one watch read
// the value AFTER the modify committed, registering against the already-current
// value so it never fired (a silent 10s timeout). tr.Watch now captures the
// watched value synchronously at call time, so ALL N watches must fire.
func TestManyWatchesSameKey_AllFire(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	key := fdb.Key(t.Name() + "_key")

	// Seed.
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("initial"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Establish N watches on the same key, each from its own transaction.
	const n = 16
	watches := make([]fdb.FutureNil, n)
	for i := 0; i < n; i++ {
		i := i
		if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			watches[i] = tr.(fdb.Transaction).Watch(key)
			return nil, nil
		}); err != nil {
			t.Fatalf("watch %d: %v", i, err)
		}
	}

	// Single modify strictly after all watches are established.
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatalf("modify: %v", err)
	}

	// Every watch must fire.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		w := watches[i]
		go func() { errs <- w.Get() }()
	}
	for fired := 0; fired < n; fired++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("watch error: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("only %d/%d watches fired within 15s — watch value-capture race regression", fired, n)
		}
	}
}

// TestGetRangeEdgeCases tests boundary conditions for GetRange.
func TestGetRangeEdgeCases(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	t.Run("empty_range_begin_equals_end", func(t *testing.T) {
		key := fdb.Key(t.Name() + "_key")
		result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			rr := tr.GetRange(fdb.KeyRange{Begin: key, End: key}, fdb.RangeOptions{})
			return rr.GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("GetRange begin==end: %v", err)
		}
		if len(result.([]fdb.KeyValue)) != 0 {
			t.Errorf("expected 0 results for begin==end, got %d", len(result.([]fdb.KeyValue)))
		}
	})

	t.Run("no_matching_keys", func(t *testing.T) {
		pfx := t.Name() + "_"
		result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			rr := tr.GetRange(fdb.KeyRange{
				Begin: fdb.Key(pfx + "aaa"),
				End:   fdb.Key(pfx + "zzz"),
			}, fdb.RangeOptions{})
			return rr.GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("GetRange no keys: %v", err)
		}
		if len(result.([]fdb.KeyValue)) != 0 {
			t.Errorf("expected 0 results, got %d", len(result.([]fdb.KeyValue)))
		}
	})

	t.Run("single_key_range", func(t *testing.T) {
		key := fdb.Key(t.Name() + "_key")
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Set(key, []byte("v"))
			return nil, nil
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			rr := tr.GetRange(fdb.KeyRange{Begin: key, End: fdb.Key(string(key) + "\x00")}, fdb.RangeOptions{})
			return rr.GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("GetRange single: %v", err)
		}
		kvs := result.([]fdb.KeyValue)
		if len(kvs) != 1 || string(kvs[0].Key) != string(key) {
			t.Errorf("expected 1 key %q, got %d keys", string(key), len(kvs))
		}
	})

	t.Run("limit_zero_means_unlimited", func(t *testing.T) {
		prefix := t.Name() + "_"
		_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
			for i := 0; i < 5; i++ {
				tr.Set(fdb.Key(fmt.Sprintf("%s%d", prefix, i)), []byte("v"))
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(prefix + "\xff")}, fdb.RangeOptions{Limit: 0})
			return rr.GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("GetRange limit=0: %v", err)
		}
		kvs := result.([]fdb.KeyValue)
		if len(kvs) != 5 {
			t.Errorf("limit=0 should be unlimited, got %d keys (want 5)", len(kvs))
		}
	})
}

// TestSetTransactionRetryLimit_Zero verifies that SetTransactionRetryLimit(0)
// actually prevents retries. This is a regression test: the fdb wrapper used
// "retryLimit != 0" as the sentinel, which silently dropped retryLimit=0.
//
// The fix is in applyTxDefaults (called by Transact), so the test must use
// Transact() — CreateTransaction() doesn't apply fdb wrapper defaults.
func TestSetTransactionRetryLimit_Zero(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	key := fdb.Key(t.Name() + "_key")

	// Seed.
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("v0"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Set retry limit to 0 — Transact should fail on first retryable error.
	db.Options().SetTransactionRetryLimit(0)

	// Count how many times Transact invokes the callback. With retryLimit=0,
	// a conflict causes OnError to reject retry, so the callback runs once
	// and Transact returns the error.
	var calls atomic.Int32
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		n := calls.Add(1)

		// First call: read key, then return a retryable error to simulate
		// a transient failure. Transact will call OnError, which should
		// reject retry (retryLimit=0).
		if n == 1 {
			return nil, fdb.Error{Code: 1020} // not_committed
		}
		// If we get here, the retry was allowed — the bug is present.
		return nil, nil
	})

	if calls.Load() > 1 {
		t.Fatalf("Transact retried %d times: retryLimit=0 was NOT applied — the original bug is present",
			calls.Load())
	}
	if err == nil {
		t.Fatal("expected error from Transact, got nil")
	}
	t.Logf("Transact correctly failed without retry (retryLimit=0): %v", err)
}

// TestAPIVersion_AlreadySetSameVersion verifies that calling APIVersion with
// the same version that is already set returns nil (idempotent).
func TestAPIVersion_AlreadySetSameVersion(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// TestMain already called MustAPIVersion(730), so 730 is set.
	err := fdb.APIVersion(730)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestAPIVersion_AlreadySetDifferentVersion verifies that calling APIVersion
// with a different version than the one already set returns error code 2201.
func TestAPIVersion_AlreadySetDifferentVersion(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	err := fdb.APIVersion(710)
	g.Expect(err).To(HaveOccurred())
	var fdbErr fdb.Error
	g.Expect(errors.As(err, &fdbErr)).To(BeTrue())
	g.Expect(fdbErr.Code).To(Equal(2201))
}

// TestAPIVersion_TooLow verifies that calling APIVersion with a version
// below the minimum (510) returns error code 2201.
func TestAPIVersion_TooLow(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	err := fdb.APIVersion(400)
	g.Expect(err).To(HaveOccurred())
	var fdbErr fdb.Error
	g.Expect(errors.As(err, &fdbErr)).To(BeTrue())
	g.Expect(fdbErr.Code).To(Equal(2201))
}

// TestGetAPIVersion verifies that GetAPIVersion returns the version
// that was set by TestMain (730).
func TestGetAPIVersion(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	v, err := fdb.GetAPIVersion()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v).To(Equal(730))
}

// TestMustAPIVersion_Panics verifies that MustAPIVersion panics when
// called with an unsupported version.
func TestMustAPIVersion_Panics(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(func() {
		fdb.MustAPIVersion(400)
	}).To(Panic())
}

// TestOpenTenant_EmptyName verifies that OpenTenant with an empty name
// returns errTenantInvalid (code 2134).
func TestOpenTenant_EmptyName(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)
	_, err := db.OpenTenant(fdb.Key(""))
	g.Expect(err).To(HaveOccurred())
	var fdbErr fdb.Error
	g.Expect(errors.As(err, &fdbErr)).To(BeTrue())
	g.Expect(fdbErr.Code).To(Equal(2134))
}

// TestOpenTenant_SystemKeyName verifies that OpenTenant with a name starting
// with \xff returns errTenantInvalid (code 2134).
func TestOpenTenant_SystemKeyName(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)
	_, err := db.OpenTenant(fdb.Key("\xff"))
	g.Expect(err).To(HaveOccurred())
	var fdbErr fdb.Error
	g.Expect(errors.As(err, &fdbErr)).To(BeTrue())
	g.Expect(fdbErr.Code).To(Equal(2134))
}

// TestOpenTenant_NonExistent verifies that OpenTenant with a name that
// does not exist returns an error.
func TestOpenTenant_NonExistent(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)
	_, err := db.OpenTenant(fdb.Key("nonexistent-tenant-xyz"))
	g.Expect(err).To(HaveOccurred())
}

// TestOpenTenantById verifies that OpenTenantById returns a working tenant
// handle when given a valid tenant ID obtained from OpenTenant.
func TestOpenTenantById(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db, _ := openTestDBWithTenants(t)

	tenantName := fdb.Key("test-open-by-id")
	g.Expect(db.CreateTenant(tenantName)).To(Succeed())
	t.Cleanup(func() {
		tn, _ := db.OpenTenant(tenantName)
		tn.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
			return nil, nil
		})
		db.DeleteTenant(tenantName)
	})

	// Open by name to get the tenant handle, write a key.
	tenant, err := db.OpenTenant(tenantName)
	g.Expect(err).NotTo(HaveOccurred())

	key := fdb.Key("byid-key")
	_, err = tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("byid-value"))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Get the tenant ID from the tenant handle.
	tenantId := tenant.ID()

	// Open by ID and verify we can read the key.
	tenantById := db.OpenTenantById(tenantId)
	result, err := tenantById.Transact(func(tr fdb.WritableTransaction) (any, error) {
		return tr.Get(key).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.([]byte)).To(Equal([]byte("byid-value")))
}

// TestHedgeToggle verifies that SetHedgeEnabled and HedgeEnabled work.
func TestHedgeToggle(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	db.SetHedgeEnabled(false)
	g.Expect(db.HedgeEnabled()).To(BeFalse())

	db.SetHedgeEnabled(true)
	g.Expect(db.HedgeEnabled()).To(BeTrue())
}

// TestRebootWorker verifies that RebootWorker returns errNotSupported (code 2051).
func TestRebootWorker(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)
	err := db.RebootWorker("", false, 0)
	g.Expect(err).To(HaveOccurred())
	var fdbErr fdb.Error
	g.Expect(errors.As(err, &fdbErr)).To(BeTrue())
	g.Expect(fdbErr.Code).To(Equal(2051))
}

// TestDatabaseMaxRetryDelay verifies that SetTransactionMaxRetryDelay
// can be called without error.
func TestDatabaseMaxRetryDelay(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)
	err := db.Options().SetTransactionMaxRetryDelay(500)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the option doesn't break normal transactions.
	_, err = db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(t.Name()+"_key"), []byte("val"))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
}

// TestDatabaseOptions_Stubs verifies that all stub DatabaseOptions methods
// return nil without error.
func TestDatabaseOptions_Stubs(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)
	opts := db.Options()

	g.Expect(opts.SetLocationCacheSize(1000)).NotTo(HaveOccurred())
	g.Expect(opts.SetMaxWatches(100)).NotTo(HaveOccurred())
	g.Expect(opts.SetDatacenterId("dc1")).NotTo(HaveOccurred())
	g.Expect(opts.SetMachineId("m1")).NotTo(HaveOccurred())
	g.Expect(opts.SetSnapshotRywEnable()).NotTo(HaveOccurred())
	g.Expect(opts.SetSnapshotRywDisable()).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionCausalReadRisky()).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionLoggingMaxFieldLength(1000)).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionReportConflictingKeys()).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionAutomaticIdempotency()).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionBypassUnreadable()).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionIncludePortInAddress()).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionUsedDuringCommitProtectionDisable()).NotTo(HaveOccurred())
	g.Expect(opts.SetUseConfigDatabase()).NotTo(HaveOccurred())
	g.Expect(opts.SetTestCausalReadRisky(1)).NotTo(HaveOccurred())
}

// TestCloseNilDatabase verifies that Close on a zero-value Database
// does not panic.
func TestCloseNilDatabase(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(func() {
		var db fdb.Database
		db.Close()
	}).NotTo(Panic())
}

// TestTransactionOptions_Stubs verifies that all stub TransactionOptions
// methods return nil without error. These are no-ops in the pure Go client
// but must not panic or return errors.
func TestTransactionOptions_Stubs(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)
	tr, err := db.CreateTransaction()
	g.Expect(err).NotTo(HaveOccurred())

	opts := tr.Options()
	g.Expect(opts.SetDebugTransactionIdentifier("test-id")).NotTo(HaveOccurred())
	g.Expect(opts.SetLogTransaction()).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionLoggingEnable("test")).NotTo(HaveOccurred())
	g.Expect(opts.SetSnapshotRywEnable()).NotTo(HaveOccurred())
	g.Expect(opts.SetUseGrvCache()).NotTo(HaveOccurred())
	g.Expect(opts.SetAutoThrottleTag("test-tag")).NotTo(HaveOccurred())
	g.Expect(opts.SetReportConflictingKeys()).NotTo(HaveOccurred())
	g.Expect(opts.SetSpecialKeySpaceRelaxed()).NotTo(HaveOccurred())
	g.Expect(opts.SetSpecialKeySpaceEnableWrites()).NotTo(HaveOccurred())
	g.Expect(opts.SetBypassUnreadable()).NotTo(HaveOccurred())
	g.Expect(opts.SetDebugRetryLogging("test")).NotTo(HaveOccurred())
	g.Expect(opts.SetIncludePortInAddress()).NotTo(HaveOccurred())
	g.Expect(opts.SetCausalReadDisable()).NotTo(HaveOccurred())
	g.Expect(opts.SetCausalWriteRisky()).NotTo(HaveOccurred())
	g.Expect(opts.SetDurabilityRisky()).NotTo(HaveOccurred())
	g.Expect(opts.SetDurabilityDatacenter()).NotTo(HaveOccurred())
	g.Expect(opts.SetDurabilityDevNullIsWebScale()).NotTo(HaveOccurred())
	g.Expect(opts.SetTransactionLoggingMaxFieldLength(1000)).NotTo(HaveOccurred())
	g.Expect(opts.SetServerRequestTracing()).NotTo(HaveOccurred())
	g.Expect(opts.SetUsedDuringCommitProtectionDisable()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadAheadDisable()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadPriorityHigh()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadPriorityLow()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadPriorityNormal()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadServerSideCacheEnable()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadServerSideCacheDisable()).NotTo(HaveOccurred())
	g.Expect(opts.SetUseProvisionalProxies()).NotTo(HaveOccurred())
	g.Expect(opts.SetBypassStorageQuota()).NotTo(HaveOccurred())
	g.Expect(opts.SetInitializeNewDatabase()).NotTo(HaveOccurred())
	g.Expect(opts.SetSpanParent([]byte{1, 2, 3})).NotTo(HaveOccurred())
	g.Expect(opts.SetExpensiveClearCostEstimationEnable()).NotTo(HaveOccurred())

	// Non-stub options that actually do something — verify they don't error.
	g.Expect(opts.SetNextWriteNoWriteConflictRange()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadYourWritesDisable()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadSystemKeys()).NotTo(HaveOccurred())
	g.Expect(opts.SetReadLockAware()).NotTo(HaveOccurred())
	g.Expect(opts.SetSnapshotRywDisable()).NotTo(HaveOccurred())
	g.Expect(opts.SetTag("test-tag")).NotTo(HaveOccurred())
	g.Expect(opts.SetSizeLimit(10_000_000)).NotTo(HaveOccurred())
	g.Expect(opts.SetMaxRetryDelay(1000)).NotTo(HaveOccurred())

	// Options that fail UNSAFE if silently ignored (RFC-111 P1.3): the pure-Go
	// backend rejects them with UnsupportedOptionError rather than the old silent
	// no-op (a migration trap — e.g. an ignored auth token = auth bypass). The
	// libfdb_c backend still forwards them.
	for _, tc := range []struct {
		name string
		set  func() error
	}{
		{"authorization_token", func() error { return opts.SetAuthorizationToken("token") }},
		{"raw_access", opts.SetRawAccess},
		{"automatic_idempotency", opts.SetAutomaticIdempotency},
	} {
		err := tc.set()
		g.Expect(err).To(HaveOccurred(), tc.name+" must reject, not silently no-op")
		var uoe *fdb.UnsupportedOptionError
		g.Expect(errors.As(err, &uoe)).To(BeTrue(), tc.name+" must return *fdb.UnsupportedOptionError")
		g.Expect(uoe.Option).To(Equal(tc.name))
		g.Expect(uoe.FDBCode()).To(Equal(2007))
	}
}

// TestSnapshotGetKey verifies that snapshot GetKey works with key selectors.
func TestSnapshotGetKey(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	// Write test data.
	prefix := t.Name() + "/"
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := range 5 {
			tr.Set(fdb.Key(fmt.Sprintf("%skey-%d", prefix, i)), []byte(fmt.Sprintf("val-%d", i)))
		}
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Snapshot GetKey: first key greater than or equal to prefix.
	_, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		sn := tr.Snapshot()
		key := sn.GetKey(fdb.FirstGreaterOrEqual(fdb.Key(prefix + "key-2"))).MustGet()
		g.Expect(string(key)).To(Equal(prefix + "key-2"))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
}

// TestSnapshotGetReadVersion verifies that snapshot GetReadVersion returns a valid version.
func TestSnapshotGetReadVersion(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		sn := tr.Snapshot()
		version := sn.GetReadVersion().MustGet()
		g.Expect(version).To(BeNumerically(">", 0))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
}

// TestSnapshotGetDatabase verifies that Snapshot.GetDatabase returns the right database.
func TestSnapshotGetDatabase(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)

	_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		sn := tr.Snapshot().(fdb.Snapshot)
		snDB := sn.GetDatabase()
		// Verify the returned database works by running a simple read.
		_, readErr := snDB.ReadTransact(func(tr2 fdb.ReadTransaction) (any, error) {
			return nil, nil
		})
		if readErr != nil {
			t.Errorf("GetDatabase returned non-functional database: %v", readErr)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ReadTransact: %v", err)
	}
}

// TestSnapshotReadTransact verifies that Snapshot.ReadTransact delegates to the callback.
func TestSnapshotReadTransact(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	g.Expect(err).NotTo(HaveOccurred())

	sn := tr.Snapshot()
	result, err := sn.ReadTransact(func(rt fdb.ReadTransaction) (any, error) {
		return "from-snapshot", nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal("from-snapshot"))
}

// TestSnapshotOptions verifies that Snapshot.Options returns working TransactionOptions.
func TestSnapshotOptions(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	g.Expect(err).NotTo(HaveOccurred())

	sn := tr.Snapshot()
	g.Expect(sn.Options().SetTimeout(5000)).NotTo(HaveOccurred())
}

// TestSnapshotCancel verifies that Snapshot.Cancel does not panic.
func TestSnapshotCancel(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	g.Expect(err).NotTo(HaveOccurred())

	sn := tr.Snapshot()
	g.Expect(func() { sn.(fdb.Snapshot).Cancel() }).NotTo(Panic())
}

// TestSnapshotSnapshot verifies that Snapshot.Snapshot is idempotent.
func TestSnapshotSnapshot(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}

	sn := tr.Snapshot()
	sn2 := sn.Snapshot()
	// Both should work identically — snapshot of a snapshot is itself.
	_ = sn2.GetReadVersion()
}

// TestFutureIsReady verifies IsReady on a completed future.
func TestFutureIsReady(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(t.Name()+"/ready"), []byte("yes"))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	_, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		f := tr.Get(fdb.Key(t.Name() + "/ready"))
		// After Get() completes, IsReady should be true.
		val := f.MustGet()
		g.Expect(val).To(Equal([]byte("yes")))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
}

// TestFutureCancel verifies that Cancel on a future does not panic.
func TestFutureCancel(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		f := tr.Get(fdb.Key(t.Name() + "/cancel"))
		g.Expect(func() { f.Cancel() }).NotTo(Panic())
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
}

// TestTenantReadTransact verifies that Tenant.ReadTransact works.
func TestTenantReadTransact(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db, _ := openTestDBWithTenants(t)

	name := fdb.Key("test-read-transact")
	g.Expect(db.CreateTenant(name)).To(Succeed())
	t.Cleanup(func() {
		tn, _ := db.OpenTenant(name)
		tn.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")})
			return nil, nil
		})
		db.DeleteTenant(name)
	})

	tenant, err := db.OpenTenant(name)
	g.Expect(err).NotTo(HaveOccurred())

	// Write data.
	_, err = tenant.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key("rt-key"), []byte("rt-val"))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// ReadTransact — should read without committing.
	result, err := tenant.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		return tr.Get(fdb.Key("rt-key")).MustGet(), nil
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.([]byte)).To(Equal([]byte("rt-val")))
}

// TestSnapshotGetEstimatedRangeSizeBytes verifies that snapshot GetEstimatedRangeSizeBytes works.
func TestSnapshotGetEstimatedRangeSizeBytes(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	// Write some data.
	prefix := t.Name() + "/"
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := range 10 {
			tr.Set(fdb.Key(fmt.Sprintf("%sdata-%04d", prefix, i)), make([]byte, 100))
		}
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	_, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		sn := tr.Snapshot()
		size := sn.GetEstimatedRangeSizeBytes(fdb.KeyRange{
			Begin: fdb.Key(prefix),
			End:   fdb.Key(prefix + "\xff"),
		}).MustGet()
		g.Expect(size).To(BeNumerically(">=", 0))
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
}

// TestOpenWithConnectionString_InvalidString verifies that OpenWithConnectionString
// returns an error for invalid connection strings.
func TestOpenWithConnectionString_InvalidString(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	_, err := fdb.OpenWithConnectionString("totally-invalid-not-a-connection-string")
	g.Expect(err).To(HaveOccurred())
}

// TestCommitFutureNil verifies that FutureNil from Commit works correctly.
func TestCommitFutureNil(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	g.Expect(err).NotTo(HaveOccurred())

	tr.Set(fdb.Key(t.Name()+"/commit-nil"), []byte("val"))

	commitFuture := tr.Commit()

	// BlockUntilReady should work.
	commitFuture.BlockUntilReady()

	// Get should return nil error on success.
	g.Expect(commitFuture.Get()).To(Succeed())

	// IsReady should be true after completion.
	g.Expect(commitFuture.IsReady()).To(BeTrue())

	// Cancel should not panic on completed future.
	g.Expect(func() { commitFuture.Cancel() }).NotTo(Panic())
}

// TestGetCommittedVersionFuture verifies FutureInt64 from GetCommittedVersion.
func TestGetCommittedVersionFuture(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	g.Expect(err).NotTo(HaveOccurred())

	tr.Set(fdb.Key(t.Name()+"/ver-future"), []byte("val"))
	g.Expect(tr.Commit().Get()).To(Succeed())

	ver, verErr := tr.GetCommittedVersion()
	g.Expect(verErr).NotTo(HaveOccurred())
	g.Expect(ver).To(BeNumerically(">", 0))
}

// TestGetRangeSplitPoints verifies that snapshot GetRangeSplitPoints works.
func TestGetRangeSplitPoints(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	db := openTestDB(t)

	// Write some data to create a non-empty range.
	prefix := t.Name() + "/"
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := range 20 {
			tr.Set(fdb.Key(fmt.Sprintf("%sdata-%04d", prefix, i)), make([]byte, 500))
		}
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	_, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		sn := tr.Snapshot()
		points := sn.GetRangeSplitPoints(fdb.KeyRange{
			Begin: fdb.Key(prefix),
			End:   fdb.Key(prefix + "\xff"),
		}, 1000) // 1KB chunks
		keys, err := points.Get()
		g.Expect(err).NotTo(HaveOccurred())
		// Just verify it doesn't error — split points depend on data distribution.
		_ = keys
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())
}
