package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// multiShardEnv holds a shared multi-shard FDB environment for cross-shard tests.
// A single container with 3 processes and small shard knobs is shared across
// all sub-tests via t.Run. The 60s wait for shard splits is paid once.
type multiShardEnv struct {
	container *tcfdb.Container
	db        *Database
	prefix    string
	numShards int
}

func setupMultiShardEnv(t *testing.T, ctx context.Context) *multiShardEnv {
	t.Helper()

	container, err := tcfdb.Run(ctx, "",
		tcfdb.WithStorageEngine("ssd"),
		tcfdb.WithDirectIP(),
		tcfdb.WithProcessCount(3),
		tcfdb.WithRedundancyMode("double"),
		tcfdb.WithKnob("min_shard_bytes", "10000"),
		tcfdb.WithKnob("max_shard_bytes", "50000"),
		tcfdb.WithKnob("shard_bytes_ratio", "2"),
		tcfdb.WithKnob("storage_metrics_polling_delay", "1"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("cluster file: %v", err)
	}

	cf, err := ParseClusterString(connStr)
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("parse cluster: %v", err)
	}

	db, err := OpenDatabaseFromConfig(ctx, cf, nil)
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("connect: %v", err)
	}

	prefix := "ms_"

	// Seed 1MB: 100 keys × 10KB.
	const numKeys = 100
	const valueSize = 10000
	for batch := 0; batch < 10; batch++ {
		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			for i := 0; i < 10; i++ {
				idx := batch*10 + i
				key := []byte(fmt.Sprintf("%s%04d", prefix, idx))
				tx.Set(key, bytes.Repeat([]byte{byte(idx % 256)}, valueSize))
			}
			return nil, nil
		})
		if err != nil {
			db.Close()
			container.Terminate(ctx)
			t.Fatalf("seed batch %d: %v", batch, err)
		}
	}
	t.Logf("seeded %d keys × %dKB", numKeys, valueSize/1000)

	// Poll for shard splits.
	var numShards int
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			begin := []byte(prefix)
			end := append([]byte(prefix), 0xFF)
			return tx.db.locCache.locateRange(tx.db, ctx, begin, end, 100, false, tx.tenantId)
		})
		if err == nil {
			locs := result.([]LocationResult)
			numShards = len(locs)
			if numShards > 1 {
				break
			}
		}
		db.db.locCache.invalidateRange([]byte(prefix), append([]byte(prefix), 0xFF), NoTenantID)
		time.Sleep(2 * time.Second)
	}
	t.Logf("shard splits: %d shards", numShards)

	return &multiShardEnv{
		container: container,
		db:        db,
		prefix:    prefix,
		numShards: numShards,
	}
}

func (e *multiShardEnv) cleanup(ctx context.Context) {
	e.db.Close()
	e.container.Terminate(ctx)
}

// TestMultiShard runs all cross-shard tests against a shared 3-process
// FDB cluster with small shards (~35 shards for 1MB data).
func TestMultiShard(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	env := setupMultiShardEnv(t, ctx)
	defer env.cleanup(ctx)

	if env.numShards <= 1 {
		t.Skip("shard splits did not occur — cannot test cross-shard behavior")
	}
	t.Logf("running cross-shard tests across %d shards", env.numShards)

	t.Run("GetRange", func(t *testing.T) {
		testMultiShard_GetRange(t, ctx, env)
	})
	t.Run("GetRangeReverse", func(t *testing.T) {
		testMultiShard_GetRangeReverse(t, ctx, env)
	})
	t.Run("GetRangeWithLimit", func(t *testing.T) {
		testMultiShard_GetRangeWithLimit(t, ctx, env)
	})
	t.Run("GetKey", func(t *testing.T) {
		testMultiShard_GetKey(t, ctx, env)
	})
	t.Run("AtomicAdd", func(t *testing.T) {
		testMultiShard_AtomicAdd(t, ctx, env)
	})
	t.Run("GetEstimatedRangeSize", func(t *testing.T) {
		testMultiShard_GetEstimatedRangeSize(t, ctx, env)
	})
}

// GetRange: full forward scan across all shards.
func testMultiShard_GetRange(t *testing.T, ctx context.Context, env *multiShardEnv) {
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(env.prefix)
		end := append([]byte(env.prefix), 0xFF)
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 100 {
		t.Fatalf("expected 100 keys, got %d", len(kvs))
	}
	// Verify ordering.
	for i := 1; i < len(kvs); i++ {
		if bytes.Compare(kvs[i-1].Key, kvs[i].Key) >= 0 {
			t.Fatalf("keys not in order at %d: %s >= %s", i, kvs[i-1].Key, kvs[i].Key)
		}
	}
	t.Logf("forward scan: 100 keys across %d shards", env.numShards)
}

// GetRangeReverse: full reverse scan across all shards.
func testMultiShard_GetRangeReverse(t *testing.T, ctx context.Context, env *multiShardEnv) {
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(env.prefix)
		end := append([]byte(env.prefix), 0xFF)
		kvs, _, err := tx.GetRangeReverse(ctx, begin, end, 0)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRangeReverse: %v", err)
	}
	kvs := result.([]KeyValue)
	if len(kvs) != 100 {
		t.Fatalf("expected 100 keys, got %d", len(kvs))
	}
	// Verify reverse ordering.
	for i := 1; i < len(kvs); i++ {
		if bytes.Compare(kvs[i-1].Key, kvs[i].Key) <= 0 {
			t.Fatalf("keys not in reverse order at %d: %s <= %s", i, kvs[i-1].Key, kvs[i].Key)
		}
	}
	t.Logf("reverse scan: 100 keys across %d shards", env.numShards)
}

// GetRangeWithLimit: paged reads across shard boundaries.
func testMultiShard_GetRangeWithLimit(t *testing.T, ctx context.Context, env *multiShardEnv) {
	// Read in pages of 7 — a non-power-of-2 that won't align with shard boundaries.
	var allKeys []KeyValue
	begin := []byte(env.prefix)
	end := append([]byte(env.prefix), 0xFF)

	for page := 0; page < 20; page++ {
		result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, more, err := tx.GetRange(ctx, begin, end, 7)
			if err != nil {
				return nil, err
			}
			return []any{kvs, more}, nil
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		parts := result.([]any)
		kvs := parts[0].([]KeyValue)
		more := parts[1].(bool)

		allKeys = append(allKeys, kvs...)

		if !more || len(kvs) == 0 {
			break
		}
		// Advance begin past last key.
		begin = append(append([]byte{}, kvs[len(kvs)-1].Key...), 0)
	}

	if len(allKeys) != 100 {
		t.Fatalf("paged scan: expected 100 keys, got %d", len(allKeys))
	}
	t.Logf("paged scan (7/page): %d keys across %d shards", len(allKeys), env.numShards)
}

// GetKey: selector resolution that must cross shard boundaries.
// This was the nightshift-9 bug — the Go client sent ONE GetKeyRequest
// and returned the reply key, ignoring offset from the reply when the
// selector crossed a shard boundary.
func testMultiShard_GetKey(t *testing.T, ctx context.Context, env *multiShardEnv) {
	// firstGreaterOrEqual on a key known to exist.
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(env.prefix+"0050"), false, 1)
	})
	if err != nil {
		t.Fatalf("GetKey FGE: %v", err)
	}
	key := result.([]byte)
	if string(key) != env.prefix+"0050" {
		t.Errorf("FGE(%s0050): got %q", env.prefix, key)
	}

	// lastLessOrEqual on a key that may be on a different shard than the selector.
	result, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(env.prefix+"0099"), true, 0)
	})
	if err != nil {
		t.Fatalf("GetKey LLE: %v", err)
	}
	key = result.([]byte)
	if string(key) != env.prefix+"0099" {
		t.Errorf("LLE(%s0099): got %q", env.prefix, key)
	}

	// firstGreaterThan a key mid-range — forces selector resolution at shard boundary.
	result, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(env.prefix+"0050"), false, 2)
	})
	if err != nil {
		t.Fatalf("GetKey FGT: %v", err)
	}
	key = result.([]byte)
	if string(key) != env.prefix+"0051" {
		t.Errorf("FGT(%s0050): got %q, want %s0051", env.prefix, key, env.prefix)
	}
	t.Logf("key selector resolution across %d shards OK", env.numShards)
}

// AtomicAdd: atomic mutations on keys that span multiple shards.
func testMultiShard_AtomicAdd(t *testing.T, ctx context.Context, env *multiShardEnv) {
	// Set counter keys across the shard range.
	counterPrefix := env.prefix + "ctr_"
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			key := []byte(fmt.Sprintf("%s%04d", counterPrefix, i*10))
			var val [8]byte
			binary.LittleEndian.PutUint64(val[:], 0)
			tx.Set(key, val[:])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed counters: %v", err)
	}

	// Atomically increment all counters.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			key := []byte(fmt.Sprintf("%s%04d", counterPrefix, i*10))
			var param [8]byte
			binary.LittleEndian.PutUint64(param[:], 42)
			tx.Atomic(MutAddValue, key, param[:])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("atomic ADD: %v", err)
	}

	// Verify all counters.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			key := []byte(fmt.Sprintf("%s%04d", counterPrefix, i*10))
			val, err := tx.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			got := binary.LittleEndian.Uint64(val)
			if got != 42 {
				t.Errorf("counter %d: got %d, want 42", i, got)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify counters: %v", err)
	}
	t.Logf("atomic ADD on 10 keys across %d shards OK", env.numShards)
}

// GetEstimatedRangeSize: should return meaningful estimate for multi-shard range.
func testMultiShard_GetEstimatedRangeSize(t *testing.T, ctx context.Context, env *multiShardEnv) {
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(env.prefix)
		end := append([]byte(env.prefix), 0xFF)
		return tx.GetEstimatedRangeSizeBytes(ctx, begin, end)
	})
	if err != nil {
		t.Fatalf("GetEstimatedRangeSizeBytes: %v", err)
	}
	size := result.(int64)
	// 100 keys × 10KB = 1MB. Estimate should be in the ballpark.
	if size < 500000 {
		t.Errorf("estimated size %d too low for 1MB of data", size)
	}
	t.Logf("estimated range size: %d bytes across %d shards", size, env.numShards)
}
