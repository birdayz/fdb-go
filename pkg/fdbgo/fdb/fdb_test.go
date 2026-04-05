package fdb_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// openTestDB starts an FDB testcontainer and returns a facade Database.
func openTestDB(t *testing.T) fdb.Database {
	t.Helper()
	fdb.MustAPIVersion(730)

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer setupCancel()

	container, err := tcfdb.Run(setupCtx, "")
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			logs, lerr := container.Logs(logCtx)
			if lerr == nil {
				logBytes, _ := io.ReadAll(logs)
				if len(logBytes) > 2000 {
					logBytes = logBytes[len(logBytes)-2000:]
				}
				t.Logf("=== FDB logs (last 2000 bytes) ===\n%s", string(logBytes))
			}
		}
		container.Terminate(context.Background())
	})

	connStr, err := container.ClusterFile(setupCtx)
	if err != nil {
		t.Fatalf("get cluster file: %v", err)
	}

	cf, err := client.ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse cluster string: %v", err)
	}

	exitCode, _, _ := container.Exec(setupCtx, []string{"fdbcli", "--exec", "configure new single ssd"})
	if exitCode != 0 {
		t.Fatalf("fdbcli configure exit: %d", exitCode)
	}
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		code, reader, execErr := container.Exec(setupCtx, []string{"fdbcli", "--exec", "status minimal"})
		if execErr != nil || reader == nil {
			continue
		}
		if code == 0 {
			out, _ := io.ReadAll(reader)
			if strings.Contains(string(out), "Healthy") {
				break
			}
		}
	}

	_, internalReader, err := container.Exec(setupCtx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		t.Fatalf("read internal cluster file: %v", err)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, err := client.ParseClusterString(strings.TrimSpace(internalStr))
	if err != nil {
		t.Fatalf("parse internal cluster: %v", err)
	}

	connectCF := &client.ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	connectCF.InternalKey = internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			connectCF.InternalKey += ","
		}
		connectCF.InternalKey += a
	}

	db, err := fdb.OpenDatabaseFromConfig(setupCtx, connectCF)
	if err != nil {
		t.Fatalf("OpenDatabaseFromConfig: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}

func TestSetGetBasic(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Write
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("facade-test-key"), []byte("hello-facade"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Read
	result, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		return tr.Get(fdb.Key("facade-test-key")).MustGet(), nil
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

	// Seed data
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("range-a"), []byte("1"))
		tr.Set(fdb.Key("range-b"), []byte("2"))
		tr.Set(fdb.Key("range-c"), []byte("3"))
		tr.Set(fdb.Key("range-d"), []byte("4"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Forward range
	result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key("range-a"), End: fdb.Key("range-e")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]fdb.KeyValue)
	if len(kvs) != 4 {
		t.Fatalf("forward range: got %d keys, want 4", len(kvs))
	}
	if string(kvs[0].Key) != "range-a" || string(kvs[3].Key) != "range-d" {
		t.Fatalf("forward range order wrong: first=%q last=%q", kvs[0].Key, kvs[3].Key)
	}

	// Reverse range
	result, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key("range-a"), End: fdb.Key("range-e")}, fdb.RangeOptions{Reverse: true})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange reverse: %v", err)
	}
	kvs = result.([]fdb.KeyValue)
	if len(kvs) != 4 {
		t.Fatalf("reverse range: got %d keys, want 4", len(kvs))
	}
	if string(kvs[0].Key) != "range-d" || string(kvs[3].Key) != "range-a" {
		t.Fatalf("reverse range order wrong: first=%q last=%q", kvs[0].Key, kvs[3].Key)
	}

	// Range with limit
	result, err = db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key("range-a"), End: fdb.Key("range-e")}, fdb.RangeOptions{Limit: 2})
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

	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("iter-1"), []byte("a"))
		tr.Set(fdb.Key("iter-2"), []byte("b"))
		tr.Set(fdb.Key("iter-3"), []byte("c"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key("iter-"), End: fdb.Key("iter-\xff")}, fdb.RangeOptions{})
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
		t.Fatalf("Iterator: %v", err)
	}
	keys := result.([]string)
	if len(keys) != 3 {
		t.Fatalf("iterator: got %d keys, want 3", len(keys))
	}
}

func TestAtomicOps(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	key := fdb.Key("atomic-counter")

	// Atomic ADD
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		// Initialize to 0 (little-endian int64)
		tr.Set(key, []byte{0, 0, 0, 0, 0, 0, 0, 0})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Add 5
	_, err = db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Add(key, []byte{5, 0, 0, 0, 0, 0, 0, 0})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Read back
	result, err := db.Transact(func(tr fdb.Transaction) (any, error) {
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

	// Seed
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("clear-a"), []byte("1"))
		tr.Set(fdb.Key("clear-b"), []byte("2"))
		tr.Set(fdb.Key("clear-c"), []byte("3"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Clear range [clear-a, clear-c)
	_, err = db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.ClearRange(fdb.KeyRange{Begin: fdb.Key("clear-a"), End: fdb.Key("clear-c")})
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ClearRange: %v", err)
	}

	// Only clear-c should remain
	result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key("clear-a"), End: fdb.Key("clear-d")}, fdb.RangeOptions{})
		return rr.GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]fdb.KeyValue)
	if len(kvs) != 1 || string(kvs[0].Key) != "clear-c" {
		t.Fatalf("after ClearRange: got %d keys, expected 1 (clear-c)", len(kvs))
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("snap-key"), []byte("snap-val"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	result, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		snap := tr.Snapshot()
		return snap.Get(fdb.Key("snap-key")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("Snapshot Get: %v", err)
	}
	if string(result.([]byte)) != "snap-val" {
		t.Fatalf("got %q, want %q", result, "snap-val")
	}
}

func TestGetCommittedVersion(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	result, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("ver-key"), []byte("val"))
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
	tr.Set(fdb.Key("ver-key2"), []byte("val2"))
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

	// Verify Database satisfies Transactor
	var transactor fdb.Transactor = db
	_, err := transactor.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("iface-key"), []byte("iface-val"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transactor.Transact: %v", err)
	}

	// Verify Database satisfies ReadTransactor
	var readTransactor fdb.ReadTransactor = db
	result, err := readTransactor.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		return tr.Get(fdb.Key("iface-key")).MustGet(), nil
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

	ks := fdb.FirstGreaterOrEqual(fdb.Key("hello"))
	if !ks.OrEqual || ks.Offset != 1 {
		t.Fatalf("FirstGreaterOrEqual: OrEqual=%v Offset=%d", ks.OrEqual, ks.Offset)
	}

	ks = fdb.FirstGreaterThan(fdb.Key("hello"))
	if ks.OrEqual || ks.Offset != 1 {
		t.Fatalf("FirstGreaterThan: OrEqual=%v Offset=%d", ks.OrEqual, ks.Offset)
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
	tr.Set(fdb.Key("vs-key"), []byte("vs-val"))
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

func TestFutureParallelism(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Seed two keys
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(fdb.Key("par-a"), []byte("val-a"))
		tr.Set(fdb.Key("par-b"), []byte("val-b"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Read both in parallel using futures
	result, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		fa := tr.Get(fdb.Key("par-a"))
		fb := tr.Get(fdb.Key("par-b"))
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
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		// Key: "vs" + 10 zero bytes (placeholder) + offset=2 (little-endian uint32)
		key := make([]byte, 2+10+4)
		key[0] = 'v'
		key[1] = 's'
		// offset 2 in little-endian
		key[12] = 2
		key[13] = 0
		key[14] = 0
		key[15] = 0
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
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		attempt++
		if attempt == 1 {
			// Return a retryable fdb.Error — the type the user sees from Get().Get().
			return nil, fdb.Error{Code: 1020} // not_committed
		}
		// Second attempt succeeds.
		tr.Set(fdb.Key("retry-conv-key"), []byte("ok"))
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
	_, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		attempt++
		if attempt == 1 {
			// Simulate MustGet() panic with a retryable error.
			panic(fdb.Error{Code: 1020}) // not_committed
		}
		tr.Set(fdb.Key("panic-recovery-key"), []byte("ok"))
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
	tr.Set(fdb.Key("opt-key"), []byte("opt-val"))
	if err := tr.Commit().Get(); err != nil {
		t.Fatalf("Commit with batch priority: %v", err)
	}
	// Verify the write landed
	result, err := db.Transact(func(tr2 fdb.Transaction) (any, error) {
		return tr2.Get(fdb.Key("opt-key")).MustGet(), nil
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
	tr2.Set(fdb.Key("opt-key2"), []byte("opt-val2"))
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
	val := tr3.Get(fdb.Key("opt-key2")).MustGet()
	if string(val) != "opt-val2" {
		t.Fatalf("causal read risky: got %q, want %q", val, "opt-val2")
	}
}

func TestSizeLimit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	// Set a tiny size limit
	if err := tr.Options().SetSizeLimit(10); err != nil {
		t.Fatalf("SetSizeLimit: %v", err)
	}
	// Write more data than the limit
	tr.Set(fdb.Key("big-key-exceeding-size-limit"), []byte("big-value-exceeding-size-limit"))
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
