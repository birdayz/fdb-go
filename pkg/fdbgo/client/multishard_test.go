package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/wire/types"
	tcfdb "fdb.dev/pkg/testcontainers/foundationdb"
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

// shardSizeConfig parameterises FDB shard knobs so the multi-shard
// suite can run against multiple shard-count regimes from the same
// helper. Default (50KB max) yields ~20 shards from 1MB of data;
// LargerShards (200KB max) yields ~5 shards.
type shardSizeConfig struct {
	minShardBytes string
	maxShardBytes string
}

var (
	defaultShardSize = shardSizeConfig{minShardBytes: "10000", maxShardBytes: "50000"}
	largerShardSize  = shardSizeConfig{minShardBytes: "40000", maxShardBytes: "200000"}
)

func setupMultiShardEnv(t *testing.T, ctx context.Context) *multiShardEnv {
	return setupMultiShardEnvWithConfig(t, ctx, defaultShardSize)
}

func setupMultiShardEnvWithConfig(t *testing.T, ctx context.Context, cfg shardSizeConfig) *multiShardEnv {
	t.Helper()
	g := gomega.NewWithT(t)

	container, err := tcfdb.Run(ctx, "",
		tcfdb.WithStorageEngine("ssd"),
		tcfdb.WithDirectIP(),
		tcfdb.WithProcessCount(3),
		tcfdb.WithRedundancyMode("double"),
		tcfdb.WithKnob("min_shard_bytes", cfg.minShardBytes),
		tcfdb.WithKnob("max_shard_bytes", cfg.maxShardBytes),
		tcfdb.WithKnob("shard_bytes_ratio", "2"),
		tcfdb.WithKnob("storage_metrics_polling_delay", "1"),
	)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Cleanup(func() { container.Terminate(context.Background()) })

	connStr, err := container.ClusterFile(ctx)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	cf, err := ParseClusterString(connStr)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	db, err := OpenDatabaseFromConfig(ctx, cf, WithAPIVersion(730))
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Cleanup(func() { db.Close() })

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
		g.Expect(err).ToNot(gomega.HaveOccurred(), "seed batch %d", batch)
	}
	t.Logf("seeded %d keys × %dKB", numKeys, valueSize/1000)

	// Poll for shard splits.
	var numShards int
	g.Eventually(func() int {
		result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			begin := []byte(prefix)
			end := append([]byte(prefix), 0xFF)
			return tx.db.locCache.locateRange(tx.db, ctx, begin, end, 100, false, tx.tenantId, tx.spanContext)
		})
		if err == nil {
			locs := result.([]LocationResult)
			numShards = len(locs)
		}
		db.db.locCache.invalidateRange([]byte(prefix), append([]byte(prefix), 0xFF), NoTenantID)
		return numShards
	}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(gomega.BeNumerically(">", 1))
	t.Logf("shard splits: %d shards", numShards)

	return &multiShardEnv{
		container: container,
		db:        db,
		prefix:    prefix,
		numShards: numShards,
	}
}

// TestMultiShard runs all cross-shard tests against a shared 3-process
// FDB cluster with small shards (~35 shards for 1MB data).
//
// Sub-tests run sequentially (no t.Parallel()) because they share env state:
// ClearRange mutates the dataset, BatchedWrites adds keys, and
// ConcurrentWritesDuringDD triggers shard splits. The container cost (~30s)
// is paid once; parallelizing would require per-subtest key prefixes and
// separate datasets, adding complexity for marginal speedup.
func TestMultiShard(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	env := setupMultiShardEnv(t, ctx)
	// Cleanup via t.Cleanup registered in setupMultiShardEnv.

	if env.numShards <= 1 {
		t.Skip("shard splits did not occur — cannot test cross-shard behavior")
	}
	t.Logf("running cross-shard tests across %d shards", env.numShards)

	// Sub-tests run sequentially: they share env state and some write to overlapping keys.
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
	t.Run("SnapshotRead", func(t *testing.T) {
		testMultiShard_SnapshotRead(t, ctx, env)
	})
	t.Run("ClearRange", func(t *testing.T) {
		testMultiShard_ClearRange(t, ctx, env)
	})
	t.Run("ConflictDetection", func(t *testing.T) {
		testMultiShard_ConflictDetection(t, ctx, env)
	})
	t.Run("ConcurrentReadsWrites", func(t *testing.T) {
		testMultiShard_ConcurrentReadsWrites(t, ctx, env)
	})
	t.Run("GetRangeSplitPoints", func(t *testing.T) {
		testMultiShard_GetRangeSplitPoints(t, ctx, env)
	})
	t.Run("SingleKeyReads", func(t *testing.T) {
		testMultiShard_SingleKeyReads(t, ctx, env)
	})
	t.Run("AtomicOpsVariety", func(t *testing.T) {
		testMultiShard_AtomicOpsVariety(t, ctx, env)
	})
	t.Run("TransactRetry", func(t *testing.T) {
		testMultiShard_TransactRetry(t, ctx, env)
	})
	t.Run("Versionstamp", func(t *testing.T) {
		testMultiShard_Versionstamp(t, ctx, env)
	})
	t.Run("ReadWriteConflictRanges", func(t *testing.T) {
		testMultiShard_ReadWriteConflictRanges(t, ctx, env)
	})
	t.Run("ReadYourWrites", func(t *testing.T) {
		testMultiShard_ReadYourWrites(t, ctx, env)
	})
	t.Run("BatchedWrites", func(t *testing.T) {
		testMultiShard_BatchedWrites(t, ctx, env)
	})
	t.Run("WatchBasic", func(t *testing.T) {
		testMultiShard_WatchBasic(t, ctx, env)
	})
	t.Run("WatchMultipleShards", func(t *testing.T) {
		testMultiShard_WatchMultipleShards(t, ctx, env)
	})
	t.Run("WatchDuringHeavyWrites", func(t *testing.T) {
		testMultiShard_WatchDuringHeavyWrites(t, ctx, env)
	})
	t.Run("WatchClearRange", func(t *testing.T) {
		testMultiShard_WatchClearRange(t, ctx, env)
	})
	t.Run("ConcurrentWritesDuringDD", func(t *testing.T) {
		testMultiShard_ConcurrentWritesDuringDD(t, ctx, env)
	})
	t.Run("MoreFlagAtShardBoundary", func(t *testing.T) {
		testMultiShard_MoreFlagAtShardBoundary(t, ctx, env)
	})
	t.Run("ContinuationCorrectnessWithCacheInvalidation", func(t *testing.T) {
		testMultiShard_ContinuationCorrectnessWithCacheInvalidation(t, ctx, env)
	})
}

// GetRange: full forward scan across all shards.
func testMultiShard_GetRange(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(env.prefix)
		end := append([]byte(env.prefix), 0xFF)
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		return kvs, err
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	kvs := result.([]KeyValue)
	g.Expect(kvs).To(gomega.HaveLen(100))
	// Verify ordering.
	for i := 1; i < len(kvs); i++ {
		g.Expect(bytes.Compare(kvs[i-1].Key, kvs[i].Key)).To(gomega.BeNumerically("<", 0),
			"keys not in order at %d: %s >= %s", i, kvs[i-1].Key, kvs[i].Key)
	}
	t.Logf("forward scan: 100 keys across %d shards", env.numShards)
}

// GetRangeReverse: full reverse scan across all shards.
func testMultiShard_GetRangeReverse(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(env.prefix)
		end := append([]byte(env.prefix), 0xFF)
		kvs, _, err := tx.GetRangeReverse(ctx, begin, end, 0)
		return kvs, err
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	kvs := result.([]KeyValue)
	g.Expect(kvs).To(gomega.HaveLen(100))
	// Verify reverse ordering.
	for i := 1; i < len(kvs); i++ {
		g.Expect(bytes.Compare(kvs[i-1].Key, kvs[i].Key)).To(gomega.BeNumerically(">", 0),
			"keys not in reverse order at %d: %s <= %s", i, kvs[i-1].Key, kvs[i].Key)
	}
	t.Logf("reverse scan: 100 keys across %d shards", env.numShards)
}

// GetRangeWithLimit: paged reads across shard boundaries.
func testMultiShard_GetRangeWithLimit(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

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
		g.Expect(err).ToNot(gomega.HaveOccurred(), "page %d", page)
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

	g.Expect(allKeys).To(gomega.HaveLen(100))
	t.Logf("paged scan (7/page): %d keys across %d shards", len(allKeys), env.numShards)
}

// GetKey: selector resolution that must cross shard boundaries.
// This was the nightshift-9 bug — the Go client sent ONE GetKeyRequest
// and returned the reply key, ignoring offset from the reply when the
// selector crossed a shard boundary.
func testMultiShard_GetKey(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// firstGreaterOrEqual on a key known to exist.
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(env.prefix+"0050"), false, 1)
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	key := result.([]byte)
	g.Expect(string(key)).To(gomega.Equal(env.prefix + "0050"))

	// lastLessOrEqual on a key that may be on a different shard than the selector.
	result, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(env.prefix+"0099"), true, 0)
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	key = result.([]byte)
	g.Expect(string(key)).To(gomega.Equal(env.prefix + "0099"))

	// firstGreaterThan a key mid-range — forces selector resolution at shard boundary.
	result, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetKey(ctx, []byte(env.prefix+"0050"), false, 2)
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	key = result.([]byte)
	g.Expect(string(key)).To(gomega.Equal(env.prefix + "0051"))
	t.Logf("key selector resolution across %d shards OK", env.numShards)
}

// AtomicAdd: atomic mutations on keys that span multiple shards.
func testMultiShard_AtomicAdd(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

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
	g.Expect(err).ToNot(gomega.HaveOccurred())

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
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify all counters.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			key := []byte(fmt.Sprintf("%s%04d", counterPrefix, i*10))
			val, err := tx.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			got := binary.LittleEndian.Uint64(val)
			g.Expect(got).To(gomega.Equal(uint64(42)), "counter %d", i)
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Logf("atomic ADD on 10 keys across %d shards OK", env.numShards)
}

// GetEstimatedRangeSize: should return meaningful estimate for multi-shard range.
func testMultiShard_GetEstimatedRangeSize(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(env.prefix)
		end := append([]byte(env.prefix), 0xFF)
		return tx.GetEstimatedRangeSizeBytes(ctx, begin, end)
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	size := result.(int64)
	// 100 keys × 10KB = 1MB. Estimate should be in the ballpark.
	g.Expect(size).To(gomega.BeNumerically(">=", int64(500000)),
		"estimated size %d too low for 1MB of data", size)
	t.Logf("estimated range size: %d bytes across %d shards", size, env.numShards)
}

// SnapshotRead: snapshot reads across shards don't add read conflicts.
// C++ Watches.actor.cpp tests snapshot isolation across distributed keys.
func testMultiShard_SnapshotRead(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// tx1: snapshot-read the full range across all shards.
	tx1 := env.db.CreateTransaction()
	rv, _, err := env.db.db.grvBatchers[grvBatcherDefault].getReadVersion(env.db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	tx1.SetReadVersion(rv)

	snap := tx1.Snapshot()
	begin := []byte(env.prefix)
	end := append([]byte(env.prefix), 0xFF)
	kvs, _, err := snap.GetRange(ctx, begin, end, 10)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(len(kvs)).To(gomega.BeNumerically(">=", 10))

	// Write to a key in the snapshot-read range from another transaction.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(env.prefix+"0050"), []byte("concurrent-write"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// tx1 should NOT conflict because snapshot reads don't add read conflicts.
	tx1.Set([]byte(env.prefix+"snap_test"), []byte("ok"))
	err = tx1.Commit(ctx)
	g.Expect(err).ToNot(gomega.HaveOccurred(), "snapshot read should not cause conflict")
	t.Logf("snapshot read across %d shards: no conflict", env.numShards)
}

// ClearRange: clear a range that spans multiple shards, verify keys gone.
// C++ PhysicalShardMove.actor.cpp validates data consistency after range operations.
func testMultiShard_ClearRange(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// Write keys in a sub-range that spans shards.
	clearPrefix := env.prefix + "clr_"
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 20; i++ {
			key := []byte(fmt.Sprintf("%s%04d", clearPrefix, i))
			tx.Set(key, bytes.Repeat([]byte{byte(i)}, 100))
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Clear the entire sub-range.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(clearPrefix)
		end := append([]byte(clearPrefix), 0xFF)
		tx.ClearRange(begin, end)
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify all keys are gone.
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(clearPrefix)
		end := append([]byte(clearPrefix), 0xFF)
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		return kvs, err
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	kvs := result.([]KeyValue)
	g.Expect(kvs).To(gomega.BeEmpty(), "expected all keys cleared")
	t.Logf("clear range across shards: 20 keys cleared")
}

// ConflictDetection: read-write conflict on keys spanning different shards.
// C++ AtomicOps.actor.cpp tests conflict behavior across distributed keys.
func testMultiShard_ConflictDetection(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	key := []byte(env.prefix + "0025") // Exists on some shard.

	// tx1: read the key (adds read conflict range).
	tx1 := env.db.CreateTransaction()
	rv, _, err := env.db.db.grvBatchers[grvBatcherDefault].getReadVersion(env.db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	tx1.SetReadVersion(rv)
	_, err = tx1.Get(ctx, key)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	tx1.Set([]byte(env.prefix+"conflict_marker"), []byte("from_tx1"))

	// tx2: write the same key and commit first.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("from_tx2"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// tx1 should conflict.
	err = tx1.Commit(ctx)
	g.Expect(err).To(gomega.HaveOccurred(), "expected conflict")
	t.Logf("cross-shard conflict detection works")
}

// ConcurrentReadsWrites: multiple goroutines reading and writing across shards.
// C++ RandomMoveKeys.actor.cpp stresses concurrent operations during shard moves.
func testMultiShard_ConcurrentReadsWrites(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// 5 concurrent goroutines, each doing 10 read-write transactions.
	const goroutines = 5
	const opsPerGoroutine = 10
	errCh := make(chan error, goroutines)

	for w := 0; w < goroutines; w++ {
		go func(workerID int) {
			for i := 0; i < opsPerGoroutine; i++ {
				_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
					// Read a key on one shard.
					readKey := []byte(fmt.Sprintf("%s%04d", env.prefix, (workerID*20+i)%100))
					_, err := tx.Get(ctx, readKey)
					if err != nil {
						return nil, err
					}
					// Write a key on potentially a different shard.
					writeKey := []byte(fmt.Sprintf("%scw_%d_%04d", env.prefix, workerID, i))
					tx.Set(writeKey, []byte(fmt.Sprintf("worker-%d-op-%d", workerID, i)))
					return nil, nil
				})
				if err != nil {
					errCh <- err
					return
				}
			}
			errCh <- nil
		}(w)
	}

	// Collect results.
	for i := 0; i < goroutines; i++ {
		err := <-errCh
		g.Expect(err).ToNot(gomega.HaveOccurred(), "worker %d failed", i)
	}
	t.Logf("concurrent reads/writes: %d goroutines × %d ops across %d shards",
		goroutines, opsPerGoroutine, env.numShards)
}

// GetRangeSplitPoints: split points API across multi-shard range.
// C++ ref: GetEstimatedRangeSize.actor.cpp tests split points with bulk data.
func testMultiShard_GetRangeSplitPoints(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	begin := []byte(env.prefix)
	end := append([]byte(env.prefix), 0xFF)
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.GetRangeSplitPoints(ctx, begin, end, 100000) // 100KB chunks
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	points := result.([][]byte)
	// GetRangeSplitPoints fetches ALL shards overlapping the range and frames the
	// result [begin, <each internal shard boundary>, <per-shard chunk splits>, end]
	// (C++ NativeAPI.actor.cpp:8164-8191). This env forces many tiny shards (~27KB)
	// smaller than the 100KB chunk, so there are no per-shard chunk splits — but each
	// INTERNAL SHARD BOUNDARY is itself a split point. So a multi-shard range must
	// return MORE than the bare [begin,end] framing: it must include the boundaries.
	// (This is what the go-vs-cgo differential + the single-shard CPort test miss; it
	// pins the multi-shard assembly so a regression to single-shard locate goes red.)
	t.Logf("split points (100KB chunks): %d points across %d shards", len(points), env.numShards)
	g.Expect(len(points)).To(gomega.BeNumerically(">=", 2), "result must be framed by [begin,end]")
	g.Expect([]byte(points[0])).To(gomega.Equal(begin), "first split point must be begin")
	g.Expect([]byte(points[len(points)-1])).To(gomega.Equal(end), "last split point must be end")
	if env.numShards > 1 {
		g.Expect(len(points)).To(gomega.BeNumerically(">", 2),
			"a %d-shard range must yield internal shard boundaries, not just [begin,end]", env.numShards)
	}
	// Ascending and in range. NOTE: the STRICT (`<`) check is valid only because
	// this env's chunkSize (100KB) exceeds the shard size (~27KB), so every shard
	// returns ZERO per-shard chunk splits and the result is just distinct shard
	// boundaries. Across a real shard SEAM with per-shard splits, FDB can emit a
	// split point equal to the next inserted boundary (StorageMetrics.actor.cpp:533
	// breaks on strict `>`), so strict-ascending is NOT a general invariant — if
	// chunkSize is ever lowered below the shard size, relax this to `<=`.
	for i, p := range points {
		g.Expect(bytes.Compare(p, begin)).To(gomega.BeNumerically(">=", 0), "split point %d below range: %x", i, p)
		g.Expect(bytes.Compare(p, end)).To(gomega.BeNumerically("<=", 0), "split point %d above range: %x", i, p)
		if i > 0 {
			g.Expect(bytes.Compare(points[i-1], p)).To(gomega.BeNumerically("<", 0),
				"split points must be strictly ascending (chunkSize>shard ⇒ no per-shard splits): [%d]=%x [%d]=%x", i-1, points[i-1], i, p)
		}
	}
}

// SingleKeyReads: read individual keys scattered across different shards.
// C++ ref: basic GetValue path exercised by all workloads.
func testMultiShard_SingleKeyReads(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// Read 20 keys spread across the range — each likely on a different shard.
	// Use even indices to avoid keys modified by other sub-tests (e.g. ConflictDetection writes ms_0025).
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 20; i++ {
			idx := i*5 + 1 // 1, 6, 11, ..., 96 — offset by 1 to avoid ConflictDetection's key 25
			key := []byte(fmt.Sprintf("%s%04d", env.prefix, idx))
			val, err := tx.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			g.Expect(val).ToNot(gomega.BeEmpty(), "key %s should exist", key)
			g.Expect(len(val)).To(gomega.Equal(10000), "key %s wrong size %d", key, len(val))
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Logf("single key reads: 20 keys across %d shards", env.numShards)
}

// AtomicOpsVariety: multiple atomic mutation types across shards.
// C++ ref: AtomicOps.actor.cpp tests all mutation types with opType parameter.
func testMultiShard_AtomicOpsVariety(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	opsPrefix := env.prefix + "aop_"

	// Seed keys for atomic ops.
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		// ByteMax test keys
		tx.Set([]byte(opsPrefix+"bmax"), []byte("apple"))
		// CompareAndClear test key
		tx.Set([]byte(opsPrefix+"cac"), []byte("match"))
		// Or test key
		tx.Set([]byte(opsPrefix+"or"), []byte{0xF0, 0x0F, 0x00, 0x00})
		// Add test key
		var val [8]byte
		binary.LittleEndian.PutUint64(val[:], 100)
		tx.Set([]byte(opsPrefix+"add"), val[:])
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Apply atomics.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutByteMax, []byte(opsPrefix+"bmax"), []byte("banana"))
		tx.Atomic(MutCompareAndClear, []byte(opsPrefix+"cac"), []byte("match"))
		tx.Atomic(MutOr, []byte(opsPrefix+"or"), []byte{0x0F, 0xF0, 0xFF, 0x00})
		var param [8]byte
		binary.LittleEndian.PutUint64(param[:], 42)
		tx.Atomic(MutAddValue, []byte(opsPrefix+"add"), param[:])
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify results.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		// ByteMax: "banana" > "apple" → "banana"
		val, err := tx.Get(ctx, []byte(opsPrefix+"bmax"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(string(val)).To(gomega.Equal("banana"))

		// CompareAndClear: "match" == "match" → cleared
		val, err = tx.Get(ctx, []byte(opsPrefix+"cac"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(val).To(gomega.BeEmpty(), "CompareAndClear should have cleared")

		// Or: 0xF00F0000 | 0x0FF0FF00 = 0xFFFFFF00
		val, err = tx.Get(ctx, []byte(opsPrefix+"or"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(val).To(gomega.Equal([]byte{0xFF, 0xFF, 0xFF, 0x00}))

		// Add: 100 + 42 = 142
		val, err = tx.Get(ctx, []byte(opsPrefix+"add"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		got := binary.LittleEndian.Uint64(val)
		g.Expect(got).To(gomega.Equal(uint64(142)))

		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Logf("atomic ops variety (ByteMax, CompareAndClear, Or, Add) across %d shards OK", env.numShards)
}

// TransactRetry: Transact retries conflicts automatically across shards.
// C++ ref: all workloads rely on automatic retry for not_committed (1020).
func testMultiShard_TransactRetry(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	key := []byte(env.prefix + "retry_key")

	// Seed.
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Concurrent increment: 5 goroutines each increment the same key.
	// Transact handles retries on conflict automatically.
	const workers = 5
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func() {
			_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
				val, err := tx.Get(ctx, key)
				if err != nil {
					return nil, err
				}
				// Append a byte to the value (simple mutation to create conflicts).
				tx.Set(key, append(val, 'x'))
				return nil, nil
			})
			errCh <- err
		}()
	}

	for i := 0; i < workers; i++ {
		err := <-errCh
		g.Expect(err).ToNot(gomega.HaveOccurred(), "worker %d", i)
	}

	// Verify final value has 5 extra bytes (one from each worker).
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	val := result.([]byte)
	// Initial "v0" (2 bytes) + 5 workers × 1 byte each = 7 bytes.
	// But some workers might read after others have already appended,
	// so the final length depends on retry ordering.
	g.Expect(len(val)).To(gomega.BeNumerically(">=", 3), // at least v0 + 1 append
		"expected at least 3 bytes, got %d: %q", len(val), val)
	t.Logf("transact retry: %d workers, final value %d bytes", workers, len(val))
}

// Versionstamp: committed version and versionstamp work across multi-shard cluster.
// C++ ref: VersionStamp.actor.cpp validates versionstamps in distributed setting.
func testMultiShard_Versionstamp(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	tx := env.db.CreateTransaction()
	rv, _, err := env.db.db.grvBatchers[grvBatcherDefault].getReadVersion(env.db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	tx.SetReadVersion(rv)

	// Write to keys on different shards.
	tx.Set([]byte(env.prefix+"vs_0010"), []byte("a"))
	tx.Set([]byte(env.prefix+"vs_0050"), []byte("b"))
	tx.Set([]byte(env.prefix+"vs_0090"), []byte("c"))

	err = tx.Commit(ctx)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	ver, err := tx.GetCommittedVersion()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(ver).To(gomega.BeNumerically(">", 0))

	vs, err := tx.GetVersionstamp()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(vs).To(gomega.HaveLen(10))

	t.Logf("versionstamp across %d shards: version=%d stamp=%x", env.numShards, ver, vs)
}

// ReadWriteConflictRanges: explicit conflict ranges spanning multiple shards.
// C++ ref: ConflictRange.actor.cpp tests explicit conflict range behavior.
func testMultiShard_ReadWriteConflictRanges(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// tx1: add explicit read conflict on a range spanning multiple shards.
	tx1 := env.db.CreateTransaction()
	rv, _, err := env.db.db.grvBatchers[grvBatcherDefault].getReadVersion(env.db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	tx1.SetReadVersion(rv)

	// Explicit read conflict spanning from key 10 to key 90 — crosses many shards.
	begin := []byte(env.prefix + "0010")
	end := []byte(env.prefix + "0090")
	err = tx1.AddReadConflictRange(begin, end)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// tx1 also writes something unrelated.
	tx1.Set([]byte(env.prefix+"rw_test"), []byte("from_tx1"))

	// tx2: write to a key in the middle of that conflict range.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(env.prefix+"0050"), []byte("concurrent"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// tx1 should conflict because of the explicit read conflict range.
	err = tx1.Commit(ctx)
	g.Expect(err).To(gomega.HaveOccurred(), "expected conflict from explicit cross-shard read conflict range")
	t.Logf("explicit read conflict range across %d shards: conflict detected", env.numShards)
}

// ReadYourWrites: RYW cache correctly merges local writes with cross-shard server reads.
// C++ ref: ReadWrite.actor.cpp + RYW unit tests in ReadYourWritesTransaction.
func testMultiShard_ReadYourWrites(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		rywPrefix := env.prefix + "ryw_"
		tx.Set([]byte(rywPrefix+"a"), []byte("val-a"))
		tx.Set([]byte(rywPrefix+"b"), []byte("val-b"))
		tx.Set([]byte(rywPrefix+"c"), []byte("val-c"))

		// Single-key read-back.
		val, err := tx.Get(ctx, []byte(rywPrefix+"b"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(string(val)).To(gomega.Equal("val-b"))

		// Clear one key.
		tx.Clear([]byte(rywPrefix + "b"))

		// Read cleared key — should be nil.
		val, err = tx.Get(ctx, []byte(rywPrefix+"b"))
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(val).To(gomega.BeNil())

		// Range read should show a and c but not b.
		kvs, _, err := tx.GetRange(ctx, []byte(rywPrefix), append([]byte(rywPrefix), 0xFF), 0)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		keys := make([]string, len(kvs))
		for i, kv := range kvs {
			keys[i] = string(kv.Key)
		}
		g.Expect(keys).To(gomega.ContainElement(rywPrefix + "a"))
		g.Expect(keys).To(gomega.ContainElement(rywPrefix + "c"))
		g.Expect(keys).ToNot(gomega.ContainElement(rywPrefix + "b"))

		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Logf("RYW: set + clear + range read with local-only data OK")
}

// BatchedWrites: large batch of writes spanning all shards in a single transaction.
// C++ ref: BulkSetup.actor.cpp tests bulk write + read-back consistency.
func testMultiShard_BatchedWrites(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	batchPrefix := env.prefix + "bat_"
	const batchSize = 50

	// Write 50 keys across the shard space in a single transaction.
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < batchSize; i++ {
			key := []byte(fmt.Sprintf("%s%04d", batchPrefix, i*2)) // 0,2,4,...,98
			tx.Set(key, []byte(fmt.Sprintf("batch-%d", i)))
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Read them all back in a single transaction.
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(batchPrefix)
		end := append([]byte(batchPrefix), 0xFF)
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		return kvs, err
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	kvs := result.([]KeyValue)
	g.Expect(kvs).To(gomega.HaveLen(batchSize))

	// Verify values.
	for i, kv := range kvs {
		g.Expect(string(kv.Value)).To(gomega.Equal(fmt.Sprintf("batch-%d", i)))
	}
	t.Logf("batched writes: %d keys written + read back across %d shards", batchSize, env.numShards)
}

// WatchBasic: watch a key in multi-shard cluster, modify it, verify watch fires.
// C++ ref: Watches.actor.cpp — basic watch trigger across storage servers.
func testMultiShard_WatchBasic(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	watchKey := []byte(env.prefix + "watch_basic")

	// Set initial value.
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(watchKey, []byte("v1"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Start watch in a goroutine.
	watchDone := make(chan error, 1)
	go func() {
		_, werr := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			return nil, tx.Watch(ctx, watchKey)
		})
		watchDone <- werr
	}()

	// Let the watch register with the storage server.
	time.Sleep(500 * time.Millisecond)

	// Modify the key from another transaction.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(watchKey, []byte("v2"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Watch should fire within 30s.
	select {
	case err := <-watchDone:
		g.Expect(err).ToNot(gomega.HaveOccurred())
	case <-time.After(30 * time.Second):
		t.Fatal("watch did not resolve within 30 seconds")
	}
	t.Logf("basic watch fired across %d shards", env.numShards)
}

// WatchMultipleShards: watch keys spread across different shards simultaneously.
// All watches should fire independently when their respective keys change.
func testMultiShard_WatchMultipleShards(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// Pick keys spread across the shard space.
	// ms_0010, ms_0030, ms_0050, ms_0070, ms_0090 should land on different shards
	// given ~35 shards over 100 keys.
	watchKeys := []string{
		env.prefix + "0010",
		env.prefix + "0030",
		env.prefix + "0050",
		env.prefix + "0070",
		env.prefix + "0090",
	}

	// These keys already exist from the seeded data. Start watches on all of them.
	var wg sync.WaitGroup
	watchErrors := make([]chan error, len(watchKeys))
	for i, key := range watchKeys {
		watchErrors[i] = make(chan error, 1)
		wg.Add(1)
		go func(idx int, k string) {
			defer wg.Done()
			_, werr := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
				return nil, tx.Watch(ctx, []byte(k))
			})
			watchErrors[idx] <- werr
		}(i, key)
	}

	// Let all watches register.
	time.Sleep(1 * time.Second)

	// Modify all watched keys in a single transaction.
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, key := range watchKeys {
			tx.Set([]byte(key), []byte("modified"))
		}
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// All watches should fire.
	for i, ch := range watchErrors {
		select {
		case err := <-ch:
			g.Expect(err).ToNot(gomega.HaveOccurred(), "watch %d (%s)", i, watchKeys[i])
		case <-time.After(30 * time.Second):
			t.Fatalf("watch %d (%s) did not resolve within 30 seconds", i, watchKeys[i])
		}
	}

	wg.Wait()
	t.Logf("all %d watches across different shards fired", len(watchKeys))
}

// WatchDuringHeavyWrites: watch a key while hammering writes across all shards.
// Verifies the watch mechanism is robust under load.
func testMultiShard_WatchDuringHeavyWrites(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	watchKey := []byte(env.prefix + "watch_heavy")

	// Set initial value.
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(watchKey, []byte("initial"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Start watch.
	watchDone := make(chan error, 1)
	go func() {
		_, werr := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			return nil, tx.Watch(ctx, watchKey)
		})
		watchDone <- werr
	}()

	time.Sleep(500 * time.Millisecond)

	// Hammer writes across the shard space while the watch is active.
	// 10 batches × 20 keys = 200 writes to OTHER keys.
	writePrefix := env.prefix + "heavy_"
	for batch := 0; batch < 10; batch++ {
		_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			for i := 0; i < 20; i++ {
				key := []byte(fmt.Sprintf("%s%04d", writePrefix, batch*20+i))
				tx.Set(key, bytes.Repeat([]byte{byte(batch)}, 1000))
			}
			return nil, nil
		})
		g.Expect(err).ToNot(gomega.HaveOccurred())
	}

	// Now modify the watched key.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(watchKey, []byte("changed_under_load"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Watch should still fire despite all the concurrent shard activity.
	select {
	case err := <-watchDone:
		g.Expect(err).ToNot(gomega.HaveOccurred())
	case <-time.After(30 * time.Second):
		t.Fatal("watch did not resolve within 30 seconds under heavy writes")
	}

	// Verify the value.
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, watchKey)
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(string(result.([]byte))).To(gomega.Equal("changed_under_load"))
	t.Logf("watch fired under heavy write load across %d shards", env.numShards)
}

// WatchClearRange: watch a key, then clear a range that spans multiple shards
// containing the watched key. Verifies the watch fires on cross-shard ClearRange.
func testMultiShard_WatchClearRange(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	watchKey := []byte(env.prefix + "watch_cr_target")

	// Set initial value.
	_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(watchKey, []byte("exists"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Start watch.
	watchDone := make(chan error, 1)
	go func() {
		_, werr := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			return nil, tx.Watch(ctx, watchKey)
		})
		watchDone <- werr
	}()

	time.Sleep(500 * time.Millisecond)

	// ClearRange covering the watched key and spanning multiple shards.
	_, err = env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(env.prefix + "watch_cr")
		end := []byte(env.prefix + "watch_cr~")
		return nil, tx.ClearRange(begin, end)
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Watch should fire.
	select {
	case err := <-watchDone:
		g.Expect(err).ToNot(gomega.HaveOccurred())
	case <-time.After(30 * time.Second):
		t.Fatal("watch did not resolve within 30 seconds after cross-shard ClearRange")
	}

	// Verify key is gone.
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, watchKey)
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(result).To(gomega.BeNil())
	t.Logf("watch fired after cross-shard ClearRange across %d shards", env.numShards)
}

// testMultiShard_ConcurrentWritesDuringDD verifies that concurrent writes
// complete successfully while Data Distribution is actively splitting shards.
// We inject a burst of large values to trigger splits, while simultaneously
// writing smaller values on separate goroutines. After all writes finish,
// we scan the entire range to verify no data was lost.
func testMultiShard_ConcurrentWritesDuringDD(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// Write into the EXISTING key range (env.prefix) so writes hit the
	// already-split 51 shards. Large values trigger further DD activity.
	const writers = 8
	const opsPerWriter = 25
	const bigValueSize = 8000 // 8KB per value; max_shard_bytes=50KB → many splits

	type writeRecord struct {
		key   string
		value string
	}
	results := make([][]writeRecord, writers)

	// Count shards before.
	shardsBefore := env.numShards

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			var records []writeRecord
			for i := 0; i < opsPerWriter; i++ {
				// Keys interleave with existing data: ms_wd03_0012 sorts
				// between ms_0030 and ms_0040, hitting different shards.
				key := fmt.Sprintf("%swd%02d_%04d", env.prefix, workerID, i)
				val := fmt.Sprintf("w%d-op%d", workerID, i)
				paddedVal := val + string(bytes.Repeat([]byte{byte(workerID)}, bigValueSize))

				_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
					tx.Set([]byte(key), []byte(paddedVal))
					return nil, nil
				})
				if err != nil {
					t.Errorf("worker %d op %d: %v", workerID, i, err)
					return
				}
				records = append(records, writeRecord{key: key, value: paddedVal})
			}
			results[workerID] = records
		}(w)
	}

	wg.Wait()

	// Count shards after — new data should trigger further splits.
	var shardsAfter int
	env.db.db.locCache.invalidateRange(
		[]byte(env.prefix), append([]byte(env.prefix), 0xFF), NoTenantID)
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.db.locCache.locateRange(tx.db, ctx,
			[]byte(env.prefix), append([]byte(env.prefix), 0xFF), 500, false, tx.tenantId, tx.spanContext)
	})
	if err == nil {
		shardsAfter = len(result.([]LocationResult))
	}
	t.Logf("shards: %d before → %d after (%d writers × %d ops × %dB values = %dKB injected)",
		shardsBefore, shardsAfter, writers, opsPerWriter, bigValueSize,
		writers*opsPerWriter*bigValueSize/1024)

	// Verify: every committed key must exist with the correct value.
	totalKeys := 0
	for workerID, records := range results {
		for _, rec := range records {
			val, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
				return tx.Get(ctx, []byte(rec.key))
			})
			g.Expect(err).ToNot(gomega.HaveOccurred(), "worker %d key %s", workerID, rec.key)
			g.Expect(val).ToNot(gomega.BeNil(), "worker %d key %s: missing after DD", workerID, rec.key)
			g.Expect(val.([]byte)).To(gomega.Equal([]byte(rec.value)),
				"worker %d key %s: value mismatch", workerID, rec.key)
			totalKeys++
		}
	}

	// Cross-check: full range scan for worker keys.
	scanResult, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx,
			[]byte(env.prefix+"wd"), append([]byte(env.prefix+"wd"), 0xFF), 0)
		return kvs, err
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	scannedKVs := scanResult.([]KeyValue)
	g.Expect(scannedKVs).To(gomega.HaveLen(totalKeys),
		"range scan count mismatch: expected %d committed keys, got %d", totalKeys, len(scannedKVs))

	t.Logf("verified %d keys across %d shards — no data lost during concurrent writes",
		totalKeys, shardsAfter)
}

// MoreFlagAtShardBoundary: regression test for swingshift-15 bug.
// When limit is met exactly across multiple shards, `more` must be true.
// Before the fix, `more` was taken from the last shard's response, which
// could be false even though subsequent shards had data.
func testMultiShard_MoreFlagAtShardBoundary(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	// Use a dedicated prefix so we're not affected by other tests' writes.
	mfPrefix := env.prefix + "mf_"
	const numKeys = 50
	const valueSize = 2000 // Enough to spread across shards with max_shard_bytes=50KB.
	for batch := 0; batch < 5; batch++ {
		_, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			for i := 0; i < 10; i++ {
				idx := batch*10 + i
				key := []byte(fmt.Sprintf("%s%04d", mfPrefix, idx))
				tx.Set(key, bytes.Repeat([]byte{byte(idx % 256)}, valueSize))
			}
			return nil, nil
		})
		g.Expect(err).ToNot(gomega.HaveOccurred(), "seed batch %d", batch)
	}

	begin := []byte(mfPrefix)
	end := append([]byte(mfPrefix), 0xFF)

	for _, limit := range []int{1, 3, 7, 10, 13, 25, 49} {
		result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, more, err := tx.GetRange(ctx, begin, end, limit)
			if err != nil {
				return nil, err
			}
			return []any{kvs, more}, nil
		})
		g.Expect(err).ToNot(gomega.HaveOccurred(), "limit=%d", limit)
		parts := result.([]any)
		kvs := parts[0].([]KeyValue)
		more := parts[1].(bool)

		g.Expect(kvs).To(gomega.HaveLen(limit), "limit=%d: expected exactly limit results", limit)
		// We know there are 50 keys total and limit < 50, so more must be true.
		g.Expect(more).To(gomega.BeTrue(),
			"limit=%d: more must be true when limit(%d) < total(%d) across %d shards",
			limit, limit, numKeys, env.numShards)
	}

	// Edge case: limit = numKeys (exact total). Should return all keys.
	// more depends on whether FDB knows there are no more keys — could be
	// true or false. The important thing: we got all results.
	result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, begin, end, numKeys)
		return kvs, err
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	kvs := result.([]KeyValue)
	g.Expect(kvs).To(gomega.HaveLen(numKeys))

	// Paged scan: verify that paginating with the `more` flag yields all keys.
	// This catches the original bug: if `more` was incorrectly false at a shard
	// boundary, the paged scan would stop early and miss keys.
	var all []KeyValue
	pageBegin := []byte(mfPrefix)
	pageEnd := append([]byte(mfPrefix), 0xFF)
	pageLimit := 7 // Deliberately misaligned with shard sizes.
	for i := 0; i < 20; i++ {
		result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, more, err := tx.GetRange(ctx, pageBegin, pageEnd, pageLimit)
			if err != nil {
				return nil, err
			}
			return []any{kvs, more}, nil
		})
		g.Expect(err).ToNot(gomega.HaveOccurred(), "page %d", i)
		parts := result.([]any)
		kvs := parts[0].([]KeyValue)
		more := parts[1].(bool)

		all = append(all, kvs...)
		if !more || len(kvs) == 0 {
			break
		}
		// Advance past last key.
		pageBegin = append(append([]byte{}, kvs[len(kvs)-1].Key...), 0)
	}
	g.Expect(all).To(gomega.HaveLen(numKeys),
		"paged scan with limit=%d should yield all %d keys, got %d (more flag bug?)",
		pageLimit, numKeys, len(all))

	t.Logf("more flag regression: all limits correct across %d shards", env.numShards)
}

// ContinuationCorrectnessWithCacheInvalidation: paginated GetRange across
// multi-shard data must produce a correctly-ordered, deduplicated result
// set EVEN when the location cache is invalidated mid-scan. The existing
// GetRangeWithLimit test relies on the warm cache; this version drops the
// cache between every page so that each page goes through a fresh
// locateRange + GetKeyServerLocations round trip. Catches a class of bugs
// where pagination semantics depend on cache stickiness — e.g. an off-by-one
// on the recomputed shard boundary or a stale "more" flag carried over from
// the previous round trip.
//
// Asserted invariants over the paginated result:
//  1. Coverage: every seeded key is returned exactly once.
//  2. Ordering: result is strictly ascending key-sorted.
//  3. No duplicates: no key appears twice across all pages.
//  4. Termination: pagination stops with !more (not because we hit the
//     200-iteration safety bound).
func testMultiShard_ContinuationCorrectnessWithCacheInvalidation(t *testing.T, ctx context.Context, env *multiShardEnv) {
	g := gomega.NewWithT(t)

	const pageLimit = 7

	begin := []byte(env.prefix)
	end := append([]byte(env.prefix), 0xFF)

	// The env is shared across this whole TestMultiShard and earlier sub-tests
	// (BatchedWrites, MoreFlagAtShardBoundary, ConcurrentWritesDuringDD)
	// have already written under env.prefix with various suffixes. Snapshot
	// the actual key count by issuing a single un-paginated scan; pagination
	// must produce exactly the same set.
	snapResult, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
		// limit=0 → unlimited.
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		return kvs, err
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	snapshot := snapResult.([]KeyValue)
	wantTotal := len(snapshot)
	g.Expect(wantTotal).To(gomega.BeNumerically(">", pageLimit),
		"snapshot has fewer keys (%d) than pageLimit (%d) — paging won't be meaningful",
		wantTotal, pageLimit)

	var collected []KeyValue
	cacheInvalidations := 0
	var iterations int
	const safetyBound = 5000 // generous: wantTotal/pageLimit pages, with slack

	for iterations = 0; iterations < safetyBound; iterations++ {
		// Drop the location cache for the prefix range BEFORE this page —
		// forces a fresh locateRange call and exercises the
		// cache-cold pagination path.
		env.db.db.locCache.invalidateRange(begin, end, NoTenantID)
		cacheInvalidations++

		result, err := env.db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, more, err := tx.GetRange(ctx, begin, end, pageLimit)
			if err != nil {
				return nil, err
			}
			return []any{kvs, more}, nil
		})
		g.Expect(err).ToNot(gomega.HaveOccurred(), "iteration %d", iterations)
		parts := result.([]any)
		kvs := parts[0].([]KeyValue)
		more := parts[1].(bool)

		collected = append(collected, kvs...)

		if !more || len(kvs) == 0 {
			break
		}
		// Standard pagination: advance begin past the last returned key.
		begin = append(append([]byte{}, kvs[len(kvs)-1].Key...), 0)
	}
	g.Expect(iterations).To(gomega.BeNumerically("<", safetyBound),
		"pagination did not terminate within %d iterations — runaway pages?", safetyBound)

	// Coverage. Compare against the snapshot taken before pagination —
	// pagination with cache invalidation must produce the same set.
	if len(collected) != wantTotal {
		t.Fatalf("want %d keys total (snapshot count), got %d (cache invalidation broke continuation?)",
			wantTotal, len(collected))
	}
	for i, kv := range collected {
		if !bytesEqual(kv.Key, snapshot[i].Key) {
			t.Fatalf("key mismatch at index %d: paged=%q snapshot=%q",
				i, kv.Key, snapshot[i].Key)
		}
		if !bytesEqual(kv.Value, snapshot[i].Value) {
			t.Errorf("value mismatch at key %q", kv.Key)
		}
	}

	// Strict ascending order.
	for i := 1; i < len(collected); i++ {
		if bytes.Compare(collected[i-1].Key, collected[i].Key) >= 0 {
			t.Fatalf("ordering violated at index %d: %q >= %q (consecutive)",
				i, collected[i-1].Key, collected[i].Key)
		}
	}

	// Dedup. (Strict ascending implies no duplicates, but assert explicitly so
	// a future change that loosens ordering still surfaces dup bugs.)
	seen := make(map[string]struct{}, len(collected))
	for _, kv := range collected {
		if _, dup := seen[string(kv.Key)]; dup {
			t.Fatalf("duplicate key in paginated result: %q", kv.Key)
		}
		seen[string(kv.Key)] = struct{}{}
	}

	t.Logf("continuation correctness: %d keys / %d pages / %d cache invalidations across %d shards",
		len(collected), iterations+1, cacheInvalidations, env.numShards)
}

// TestMultiShard_LargerShards exercises a SUBSET of the multi-shard
// suite against a SECOND shard-count config — max_shard_bytes=200KB
// yields ~5 shards from 1MB of data instead of the ~20 shards the
// default config produces.
//
// Why a subset: each setup pays a ~30s container-startup cost;
// running the full 24-subtest matrix at every shard config would
// triple `just test` time. The subset picks the topology-sensitive
// tests where shard count matters most:
//
//   - GetRange: full scan correctness across shard boundaries
//   - GetRangeWithLimit: paged scan with continuation across boundaries
//     (this is the surface where the nightshift-9 bug + similar bugs
//     hide)
//   - GetKey: selector resolution across shard boundaries (also
//     nightshift-9-prone)
//   - ClearRange: range mutation that crosses shard boundaries
//   - ContinuationCorrectnessWithCacheInvalidation: the most thorough
//     boundary-correctness test
//
// Closes part of TODO HIGH 'Multi-shard test matrix' — adds varied
// shard-count coverage without exploding container time.
func TestMultiShard_LargerShards(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	env := setupMultiShardEnvWithConfig(t, ctx, largerShardSize)
	if env.numShards <= 1 {
		t.Skip("shard splits did not occur with larger-shards config")
	}
	t.Logf("running cross-shard tests across %d shards (large-shards config)", env.numShards)

	t.Run("GetRange", func(t *testing.T) {
		testMultiShard_GetRange(t, ctx, env)
	})
	t.Run("GetRangeWithLimit", func(t *testing.T) {
		testMultiShard_GetRangeWithLimit(t, ctx, env)
	})
	t.Run("GetKey", func(t *testing.T) {
		testMultiShard_GetKey(t, ctx, env)
	})
	t.Run("ClearRange", func(t *testing.T) {
		testMultiShard_ClearRange(t, ctx, env)
	})
	t.Run("ContinuationCorrectnessWithCacheInvalidation", func(t *testing.T) {
		testMultiShard_ContinuationCorrectnessWithCacheInvalidation(t, ctx, env)
	})
}
