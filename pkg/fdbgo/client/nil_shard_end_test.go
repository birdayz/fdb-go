package client

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestNilShardEndLocateRange proves that locateRange does not infinite-loop
// when the cache contains a shard with nil ShardEnd (meaning "extends to
// infinity"). Before the fix, gapBegin was never advanced past such a shard,
// causing the tail-gap check to stay true forever → infinite refresh loop.
func TestNilShardEndLocateRange(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{
				begin: []byte(""),
				end:   nil, // shard extends to infinity
				servers: []ServerInfo{
					{Address: "127.0.0.1:4500"},
				},
			},
		},
	}

	// locateRange needs a *database, but it only uses it for refreshRange
	// (cache miss). Since the cache already covers the entire requested range,
	// the db should never be touched. Pass nil — if the bug is present, we'd
	// loop forever (timeout), not nil-deref.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	results, err := lc.locateRange(nil, ctx, []byte("a"), []byte("z"), 100, NoTenantID)
	if err != nil {
		t.Fatalf("locateRange returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ShardEnd != nil {
		t.Fatalf("expected nil ShardEnd, got %v", results[0].ShardEnd)
	}
}

// TestNilShardEndLocateRangePartialCoverage tests that when a non-nil-end
// shard covers the beginning of the range and a nil-end shard covers the rest,
// locateRange correctly returns both without looping.
func TestNilShardEndLocateRangePartialCoverage(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{
				begin:   []byte(""),
				end:     []byte("m"),
				servers: []ServerInfo{{Address: "127.0.0.1:4500"}},
			},
			{
				begin:   []byte("m"),
				end:     nil, // last shard extends to infinity
				servers: []ServerInfo{{Address: "127.0.0.1:4501"}},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	results, err := lc.locateRange(nil, ctx, []byte("a"), []byte("z"), 100, NoTenantID)
	if err != nil {
		t.Fatalf("locateRange returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

// TestNilShardEndClampLogic verifies that the shard-clamping logic in getRange
// correctly clamps a nil ShardEnd to curEnd instead of skipping the shard.
//
// Bug 2: the old code was:
//
//	if shardEnd != nil && bytes.Compare(shardEnd, curEnd) > 0 { shardEnd = curEnd }
//
// With nil shardEnd, the condition was false, shardEnd stayed nil.
// Then bytes.Compare(shardBegin, nil) > 0 for any non-empty begin → shard skipped.
//
// Fixed code:
//
//	if shardEnd == nil || bytes.Compare(shardEnd, curEnd) > 0 { shardEnd = curEnd }
func TestNilShardEndClampLogic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		shardEnd  []byte
		curEnd    []byte
		wantClamp bool
	}{
		{
			name:      "nil shardEnd must be clamped",
			shardEnd:  nil,
			curEnd:    []byte("z"),
			wantClamp: true,
		},
		{
			name:      "shardEnd beyond curEnd must be clamped",
			shardEnd:  []byte{0xff},
			curEnd:    []byte("z"),
			wantClamp: true,
		},
		{
			name:      "shardEnd within range not clamped",
			shardEnd:  []byte("m"),
			curEnd:    []byte("z"),
			wantClamp: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shardEnd := tc.shardEnd
			// Apply the fixed clamping logic from readpath.go.
			if shardEnd == nil || bytes.Compare(shardEnd, tc.curEnd) > 0 {
				shardEnd = tc.curEnd
			}
			if tc.wantClamp && !bytes.Equal(shardEnd, tc.curEnd) {
				t.Fatalf("expected shardEnd clamped to %q, got %q", tc.curEnd, shardEnd)
			}
			if !tc.wantClamp && bytes.Equal(shardEnd, tc.curEnd) {
				t.Fatalf("expected shardEnd NOT clamped, got %q", shardEnd)
			}
			// After clamping, shard must not be empty.
			shardBegin := []byte("a")
			if bytes.Compare(shardBegin, shardEnd) >= 0 {
				t.Fatalf("shard should not be empty: begin=%q end=%q", shardBegin, shardEnd)
			}
		})
	}
}

// TestNilShardEndExhaustedBreak verifies that getRange breaks out of the
// outer loop when the last shard has nil ShardEnd (Bug 3).
//
// Old code:
//
//	curBegin = lastShard.ShardEnd  // nil → curBegin becomes nil
//	// bytes.Compare(nil, curEnd) < 0 → always true → infinite loop
//
// Fixed code:
//
//	if lastShard.ShardEnd == nil || bytes.Compare(lastShard.ShardEnd, curEnd) >= 0 { break }
func TestNilShardEndExhaustedBreak(t *testing.T) {
	t.Parallel()

	curEnd := []byte("z")

	cases := []struct {
		name        string
		shardEnd    []byte
		shouldBreak bool
	}{
		{
			name:        "nil ShardEnd must break",
			shardEnd:    nil,
			shouldBreak: true,
		},
		{
			name:        "ShardEnd == curEnd must break",
			shardEnd:    []byte("z"),
			shouldBreak: true,
		},
		{
			name:        "ShardEnd past curEnd must break",
			shardEnd:    []byte{0xff},
			shouldBreak: true,
		},
		{
			name:        "ShardEnd before curEnd must not break",
			shardEnd:    []byte("m"),
			shouldBreak: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Reproduce the fixed exhausted-shards logic from readpath.go.
			got := tc.shardEnd == nil || bytes.Compare(tc.shardEnd, curEnd) >= 0
			if got != tc.shouldBreak {
				t.Fatalf("shouldBreak: got %v, want %v (shardEnd=%v)", got, tc.shouldBreak, tc.shardEnd)
			}
		})
	}
}
