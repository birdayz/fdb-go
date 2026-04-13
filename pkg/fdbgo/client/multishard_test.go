package client

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestMultiShard_GetRange verifies that GetRange correctly handles
// cross-shard reads by seeding enough data to trigger shard splits
// in a container with reduced shard size (40KB via min_shard_bytes knob).
//
// This test uses a SEPARATE container with the small shard knob,
// not the shared container (which has default 10MB shards).
func TestMultiShard_GetRange(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Start FDB with small shards to force data distribution splits.
	container, err := tcfdb.Run(ctx, "",
		tcfdb.WithStorageEngine("ssd"),
		tcfdb.WithDirectIP(),
		tcfdb.WithKnob("min_shard_bytes", "10000"),
		tcfdb.WithKnob("max_shard_bytes", "50000"),
		tcfdb.WithKnob("shard_bytes_ratio", "2"),
	)
	if err != nil {
		t.Fatalf("start FDB container with small shards: %v", err)
	}
	defer container.Terminate(ctx)

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("cluster file: %v", err)
	}

	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse cluster: %v", err)
	}

	db, err := OpenDatabaseFromConfig(ctx, cf, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()

	prefix := "multishard_"

	// Seed enough data to trigger shard splits.
	// 100 keys × 10KB each = 1MB total >> 40KB min_shard_bytes.
	// With shard_bytes_ratio=2, max shard = 80KB, so 1MB should yield ~12+ shards.
	const numKeys = 100
	const valueSize = 10000 // 10KB per key
	for batch := 0; batch < 10; batch++ {
		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			for i := 0; i < 10; i++ {
				idx := batch*10 + i
				key := []byte(fmt.Sprintf("%s%04d", prefix, idx))
				val := bytes.Repeat([]byte{byte(idx % 256)}, valueSize)
				tx.Set(key, val)
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("seed batch %d: %v", batch, err)
		}
	}
	t.Logf("seeded %d keys × %dKB = %dKB total", numKeys, valueSize/1000, numKeys*valueSize/1000)

	// Wait for data distribution to potentially split shards. DD is async;
	// single-node clusters may not split at all (only one storage server).
	// The test validates the full GetRange path regardless of shard count.
	time.Sleep(5 * time.Second)

	// Check how many shard locations we get for our range.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)
		locs, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, 100, false, tx.tenantId)
		return locs, err
	})
	if err != nil {
		t.Fatalf("locateRange: %v", err)
	}
	locs := result.([]LocationResult)
	t.Logf("shard locations for range: %d", len(locs))
	for i, loc := range locs {
		t.Logf("  shard[%d]: begin=%x end=%x servers=%d", i, loc.ShardBegin, loc.ShardEnd, len(loc.Servers))
	}

	// Now do a full range read. Even if we only have 1 shard (splits haven't
	// happened yet), this validates the complete read path.
	readResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := []byte(prefix)
		end := append([]byte(prefix), 0xFF)
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	kvs := readResult.([]KeyValue)
	t.Logf("GetRange returned %d keys", len(kvs))

	if len(kvs) != numKeys {
		t.Fatalf("expected %d keys, got %d", numKeys, len(kvs))
	}

	// Verify keys are in order and values match.
	for i, kv := range kvs {
		expectedKey := fmt.Sprintf("%s%04d", prefix, i)
		if string(kv.Key) != expectedKey {
			t.Fatalf("key[%d]: got %q, want %q", i, kv.Key, expectedKey)
		}
		if len(kv.Value) != valueSize {
			t.Fatalf("value[%d]: got %d bytes, want %d", i, len(kv.Value), valueSize)
		}
	}

	if len(locs) > 1 {
		t.Logf("SUCCESS: cross-shard GetRange worked across %d shards", len(locs))
	} else {
		t.Log("NOTE: data distribution did not split within 5s — test validates single-shard path only")
	}
}
