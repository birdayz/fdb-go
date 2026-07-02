package client

import (
	"bytes"
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestLocateBinarySearch tests O(log N) lookup via the sorted cache.
func TestLocateBinarySearch(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1"}}},
			{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s2"}}},
			{tenantId: NoTenantID, begin: []byte("g"), end: []byte("k"), servers: []ServerInfo{{Address: "s3"}}},
			{tenantId: NoTenantID, begin: []byte("k"), end: nil, servers: []ServerInfo{{Address: "s4"}}},
		},
	}

	ctx := context.Background()

	tests := []struct {
		name    string
		key     []byte
		wantHit bool
		wantSrv string
	}{
		{"exact begin of first shard", []byte("a"), true, "s1"},
		{"middle of first shard", []byte("b"), true, "s1"},
		{"boundary between first and second", []byte("d"), true, "s2"},
		{"middle of second shard", []byte("e"), true, "s2"},
		{"middle of third shard", []byte("h"), true, "s3"},
		{"exact begin of last shard", []byte("k"), true, "s4"},
		{"deep in last shard (nil end)", []byte("z"), true, "s4"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, err := lc.locate(nil, ctx, tc.key, NoTenantID, types.SpanContext{}, false)
			if err != nil {
				t.Fatalf("locate(%q): %v", tc.key, err)
			}
			if !tc.wantHit {
				t.Fatalf("expected cache miss for %q", tc.key)
			}
			if len(result.Servers) == 0 || result.Servers[0].Address != tc.wantSrv {
				t.Fatalf("locate(%q): want server %s, got %v", tc.key, tc.wantSrv, result.Servers)
			}
		})
	}
}

// TestLocateEmptyCache verifies that locate on an empty cache does not panic
// and returns a cache miss (which would normally trigger a refresh, but we
// check the lookup path directly here).
func TestLocateEmptyCache(t *testing.T) {
	t.Parallel()

	lc := &locationCache{maxSize: 1000}

	lc.mu.RLock()
	_, ok := lc.lookupLocked(NoTenantID, []byte("anything"), false)
	lc.mu.RUnlock()

	if ok {
		t.Fatal("expected cache miss on empty cache")
	}
}

// TestLocateSingleEntry verifies lookup with exactly one entry.
func TestLocateSingleEntry(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("m"), end: []byte("z"), servers: []ServerInfo{{Address: "s1"}}},
		},
	}

	ctx := context.Background()

	// Hit: key inside range.
	result, err := lc.locate(nil, ctx, []byte("p"), NoTenantID, types.SpanContext{}, false)
	if err != nil {
		t.Fatalf("locate(p): %v", err)
	}
	if result.Servers[0].Address != "s1" {
		t.Fatalf("expected s1, got %v", result.Servers)
	}

	// Miss: key before range.
	lc.mu.RLock()
	_, ok := lc.lookupLocked(NoTenantID, []byte("a"), false)
	lc.mu.RUnlock()
	if ok {
		t.Fatal("expected miss for key before range")
	}

	// Miss: key at or after end.
	lc.mu.RLock()
	_, ok = lc.lookupLocked(NoTenantID, []byte("z"), false)
	lc.mu.RUnlock()
	if ok {
		t.Fatal("expected miss for key at end boundary")
	}
}

// TestLookupLocked_BackwardSelectorOnBoundary pins finding #10: a BACKWARD key selector (offset<=0 &&
// !orEqual) anchored EXACTLY on a shard boundary must resolve to the shard ENDING at the key (the one
// holding keyBefore(key)), not the shard beginning at it. Two adjacent shards [a,m)@SSA and [m,z)@SSB:
// forward "m" → SSB (begins at m); backward "m" → SSA (ends at m). Mirrors C++ getCachedLocation's
// rangeContainingKeyBefore (NativeAPI.actor.cpp:1944-1955). Revert-proof: dropping the isBackward branch
// in lookupLocked makes backward "m" resolve to SSB — the wrong SS, which sends getKey's loop re-locating
// forever (the livelock this closes).
func TestLookupLocked_BackwardSelectorOnBoundary(t *testing.T) {
	t.Parallel()
	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("m"), servers: []ServerInfo{{Address: "SSA"}}},
			{tenantId: NoTenantID, begin: []byte("m"), end: []byte("z"), servers: []ServerInfo{{Address: "SSB"}}},
		},
	}
	lc.mu.RLock()
	defer lc.mu.RUnlock()

	// Forward "m" → the shard BEGINNING at m (SSB).
	if r, ok := lc.lookupLocked(NoTenantID, []byte("m"), false); !ok || r.Servers[0].Address != "SSB" {
		t.Fatalf("forward lookup of boundary key m: got ok=%v %v, want SSB", ok, r.Servers)
	}
	// Backward "m" → the shard ENDING at m (SSA); keyBefore(m) lives in [a,m).
	if r, ok := lc.lookupLocked(NoTenantID, []byte("m"), true); !ok || r.Servers[0].Address != "SSA" {
		t.Fatalf("backward lookup of boundary key m must resolve to the shard ENDING at m (SSA); got ok=%v %v (#10)", ok, r.Servers)
	}
	// Backward "p" (strictly inside [m,z)) → SSB, same as forward — only the boundary differs.
	if r, ok := lc.lookupLocked(NoTenantID, []byte("p"), true); !ok || r.Servers[0].Address != "SSB" {
		t.Fatalf("backward lookup of interior key p: got ok=%v %v, want SSB", ok, r.Servers)
	}
}

// TestLocate_SystemKeyClampIgnoresBackward pins the codex #10 P2: locate clamps the \xff\xff system
// keyspace to the single 0xff sentinel, so a BACKWARD selector on allKeysEnd must NOT then apply a
// backward lookup to the sentinel — that would route to the last USER shard ending at 0xff instead of the
// SYS shard containing 0xff (wrong-shard/retry on a multi-SS cluster). Revert-proof: dropping the
// `isBackward = false` after the clamp resolves to USER.
func TestLocate_SystemKeyClampIgnoresBackward(t *testing.T) {
	t.Parallel()
	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte{0xff}, servers: []ServerInfo{{Address: "USER"}}},
			{tenantId: NoTenantID, begin: []byte{0xff}, end: []byte{0xff, 0xff, 0xff}, servers: []ServerInfo{{Address: "SYS"}}},
		},
	}
	// Backward selector on allKeysEnd (0xff 0xff), clamped to 0xff → must land on SYS, not USER.
	loc, err := lc.locate(nil, context.Background(), []byte{0xff, 0xff}, NoTenantID, types.SpanContext{}, true)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if len(loc.Servers) == 0 || loc.Servers[0].Address != "SYS" {
		t.Fatalf("backward getKey on allKeysEnd (clamped to 0xff) must route to the SYS shard, not the USER shard ending at 0xff; got %v (codex #10 P2)", loc.Servers)
	}
}

// TestInvalidate_BackwardSelectorEvictsShardEndingAtKey pins the backward branch of invalidate:
// getKey's wrong-shard invalidate for a BACKWARD selector must evict the shard ENDING at the
// boundary key (the one it resolved to), not the one beginning at it. [a,m)@SSA + [m,z)@SSB:
// invalidate("m", backward) evicts SSA. Revert-proof: dropping the backward branch in invalidate uses the
// forward logic → evicts SSB (wrong) → SSA stays cached → the getKey loop keeps hitting the stale shard.
func TestInvalidate_BackwardSelectorEvictsShardEndingAtKey(t *testing.T) {
	t.Parallel()
	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("m"), servers: []ServerInfo{{Address: "SSA"}}},
			{tenantId: NoTenantID, begin: []byte("m"), end: []byte("z"), servers: []ServerInfo{{Address: "SSB"}}},
		},
	}
	lc.invalidate([]byte("m"), NoTenantID, true) // backward: evict the shard ending at m (SSA)

	lc.mu.RLock()
	defer lc.mu.RUnlock()
	if _, ok := lc.lookupLocked(NoTenantID, []byte("m"), true); ok {
		t.Fatal("backward invalidate must evict the shard ending at m (SSA); it is still cached (#10)")
	}
	if r, ok := lc.lookupLocked(NoTenantID, []byte("m"), false); !ok || r.Servers[0].Address != "SSB" {
		t.Fatalf("backward invalidate must NOT evict the forward shard SSB; got ok=%v %v", ok, r.Servers)
	}
}

// TestLocateTenantIsolation verifies that entries from different tenants don't
// interfere with each other.
func TestLocateTenantIsolation(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			// tenantId -1 (NoTenantID) sorts before tenantId 42.
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "default-tenant"}}},
			{tenantId: 42, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "tenant-42"}}},
			{tenantId: 99, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "tenant-99"}}},
		},
	}

	ctx := context.Background()

	// Each tenant should get its own server.
	r1, err := lc.locate(nil, ctx, []byte("m"), NoTenantID, types.SpanContext{}, false)
	if err != nil || r1.Servers[0].Address != "default-tenant" {
		t.Fatalf("NoTenantID: want default-tenant, got %v (err=%v)", r1.Servers, err)
	}

	r2, err := lc.locate(nil, ctx, []byte("m"), 42, types.SpanContext{}, false)
	if err != nil || r2.Servers[0].Address != "tenant-42" {
		t.Fatalf("tenant 42: want tenant-42, got %v (err=%v)", r2.Servers, err)
	}

	r3, err := lc.locate(nil, ctx, []byte("m"), 99, types.SpanContext{}, false)
	if err != nil || r3.Servers[0].Address != "tenant-99" {
		t.Fatalf("tenant 99: want tenant-99, got %v (err=%v)", r3.Servers, err)
	}

	// Unknown tenant should miss.
	lc.mu.RLock()
	_, ok := lc.lookupLocked(77, []byte("m"), false)
	lc.mu.RUnlock()
	if ok {
		t.Fatal("expected miss for unknown tenant 77")
	}
}

// TestInsertSortedDedup verifies that inserting an entry with the same
// (tenantId, begin) replaces the existing one.
func TestInsertSortedDedup(t *testing.T) {
	t.Parallel()

	lc := &locationCache{maxSize: 1000}

	lc.insertSorted([]locationEntry{
		{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1"}}},
		{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s2"}}},
	})
	if len(lc.entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(lc.entries))
	}

	// Insert duplicate begin="a" with different server — should replace.
	lc.insertSorted([]locationEntry{
		{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1-replaced"}}},
	})
	if len(lc.entries) != 2 {
		t.Fatalf("expected 2 entries after dedup, got %d", len(lc.entries))
	}
	if lc.entries[0].servers[0].Address != "s1-replaced" {
		t.Fatalf("expected replaced server, got %s", lc.entries[0].servers[0].Address)
	}
}

// TestInsertSortedOrder verifies that entries are inserted in sorted order
// even when provided out of order.
func TestInsertSortedOrder(t *testing.T) {
	t.Parallel()

	lc := &locationCache{maxSize: 1000}

	// Insert out of order.
	lc.insertSorted([]locationEntry{
		{tenantId: NoTenantID, begin: []byte("g"), end: []byte("k"), servers: []ServerInfo{{Address: "s3"}}},
		{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1"}}},
		{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s2"}}},
	})

	if len(lc.entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(lc.entries))
	}

	// Verify sorted order.
	for i := 1; i < len(lc.entries); i++ {
		if !entryLess(&lc.entries[i-1], &lc.entries[i]) {
			t.Fatalf("entries not sorted at index %d: %q >= %q",
				i, lc.entries[i-1].begin, lc.entries[i].begin)
		}
	}
}

// TestInsertSortedMultiTenant verifies insertion ordering across tenants.
func TestInsertSortedMultiTenant(t *testing.T) {
	t.Parallel()

	lc := &locationCache{maxSize: 1000}

	lc.insertSorted([]locationEntry{
		{tenantId: 42, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "t42"}}},
		{tenantId: NoTenantID, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "default"}}},
		{tenantId: 10, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "t10"}}},
	})

	if len(lc.entries) != 3 {
		t.Fatalf("expected 3, got %d", len(lc.entries))
	}

	// Sorted by tenantId: -1, 10, 42.
	if lc.entries[0].tenantId != NoTenantID {
		t.Fatalf("expected NoTenantID first, got %d", lc.entries[0].tenantId)
	}
	if lc.entries[1].tenantId != 10 {
		t.Fatalf("expected tenant 10 second, got %d", lc.entries[1].tenantId)
	}
	if lc.entries[2].tenantId != 42 {
		t.Fatalf("expected tenant 42 third, got %d", lc.entries[2].tenantId)
	}
}

// TestInvalidateBinarySearch verifies O(log N) invalidation.
func TestInvalidateBinarySearch(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1"}}},
			{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s2"}}},
			{tenantId: NoTenantID, begin: []byte("g"), end: []byte("k"), servers: []ServerInfo{{Address: "s3"}}},
		},
	}

	// Invalidate key "e" — falls in [d, g) shard.
	lc.invalidate([]byte("e"), NoTenantID, false)

	if len(lc.entries) != 2 {
		t.Fatalf("expected 2 entries after invalidate, got %d", len(lc.entries))
	}
	// Should have removed [d,g), kept [a,d) and [g,k).
	if !bytes.Equal(lc.entries[0].begin, []byte("a")) {
		t.Fatalf("expected first entry begin=a, got %q", lc.entries[0].begin)
	}
	if !bytes.Equal(lc.entries[1].begin, []byte("g")) {
		t.Fatalf("expected second entry begin=g, got %q", lc.entries[1].begin)
	}
}

// TestInvalidateExactBegin verifies invalidation when the key matches exactly
// the begin of a shard.
func TestInvalidateExactBegin(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1"}}},
			{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s2"}}},
		},
	}

	// Invalidate key "d" — exact begin of second shard.
	lc.invalidate([]byte("d"), NoTenantID, false)

	if len(lc.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(lc.entries))
	}
	if !bytes.Equal(lc.entries[0].begin, []byte("a")) {
		t.Fatalf("expected begin=a, got %q", lc.entries[0].begin)
	}
}

// TestInvalidateMiss verifies that invalidating a key not in any shard is a no-op.
func TestInvalidateMiss(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s1"}}},
		},
	}

	// Key "a" is before the only shard — should be a no-op.
	lc.invalidate([]byte("a"), NoTenantID, false)
	if len(lc.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(lc.entries))
	}

	// Key "z" is after the shard — no-op.
	lc.invalidate([]byte("z"), NoTenantID, false)
	if len(lc.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(lc.entries))
	}
}

// TestInvalidateEmptyCache verifies no panic on empty cache.
func TestInvalidateEmptyCache(t *testing.T) {
	t.Parallel()

	lc := &locationCache{maxSize: 1000}
	lc.invalidate([]byte("a"), NoTenantID, false) // should not panic
	if len(lc.entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(lc.entries))
	}
}

// TestInvalidateTenantIsolation verifies that invalidate only removes entries
// for the specified tenant.
func TestInvalidateTenantIsolation(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "default"}}},
			{tenantId: 42, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "t42"}}},
		},
	}

	// Invalidate for tenant 42 only.
	lc.invalidate([]byte("m"), 42, false)

	if len(lc.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(lc.entries))
	}
	if lc.entries[0].tenantId != NoTenantID {
		t.Fatalf("expected NoTenantID entry to survive, got tenant %d", lc.entries[0].tenantId)
	}
}

// TestLocateRangeBinarySearch verifies the binary-search-based locateRange.
func TestLocateRangeBinarySearch(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1"}}},
			{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s2"}}},
			{tenantId: NoTenantID, begin: []byte("g"), end: []byte("k"), servers: []ServerInfo{{Address: "s3"}}},
			{tenantId: NoTenantID, begin: []byte("k"), end: []byte("z"), servers: []ServerInfo{{Address: "s4"}}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Range [c, h) should overlap [a,d), [d,g), [g,k).
	results, err := lc.locateRange(nil, ctx, []byte("c"), []byte("h"), 100, false, NoTenantID, types.SpanContext{})
	if err != nil {
		t.Fatalf("locateRange: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].Servers[0].Address != "s1" {
		t.Fatalf("first result: want s1, got %s", results[0].Servers[0].Address)
	}
	if results[2].Servers[0].Address != "s3" {
		t.Fatalf("third result: want s3, got %s", results[2].Servers[0].Address)
	}
}

// TestEvictReSorts verifies that eviction re-sorts the entries.
func TestEvictReSorts(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 3,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1"}}},
			{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s2"}}},
			{tenantId: NoTenantID, begin: []byte("g"), end: []byte("k"), servers: []ServerInfo{{Address: "s3"}}},
			{tenantId: NoTenantID, begin: []byte("k"), end: []byte("n"), servers: []ServerInfo{{Address: "s4"}}},
			{tenantId: NoTenantID, begin: []byte("n"), end: []byte("z"), servers: []ServerInfo{{Address: "s5"}}},
		},
	}

	lc.mu.Lock()
	lc.evictIfNeeded()
	lc.mu.Unlock()

	if len(lc.entries) != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", len(lc.entries))
	}

	// Verify sorted order is maintained after eviction.
	for i := 1; i < len(lc.entries); i++ {
		if !entryLess(&lc.entries[i-1], &lc.entries[i]) {
			t.Fatalf("entries not sorted after eviction at index %d: %q >= %q",
				i, lc.entries[i-1].begin, lc.entries[i].begin)
		}
	}
}

// TestInvalidateRangePreservesSort verifies entries remain sorted after invalidateRange.
func TestInvalidateRangePreservesSort(t *testing.T) {
	t.Parallel()

	lc := &locationCache{
		maxSize: 1000,
		entries: []locationEntry{
			{tenantId: NoTenantID, begin: []byte("a"), end: []byte("d"), servers: []ServerInfo{{Address: "s1"}}},
			{tenantId: NoTenantID, begin: []byte("d"), end: []byte("g"), servers: []ServerInfo{{Address: "s2"}}},
			{tenantId: NoTenantID, begin: []byte("g"), end: []byte("k"), servers: []ServerInfo{{Address: "s3"}}},
			{tenantId: NoTenantID, begin: []byte("k"), end: []byte("z"), servers: []ServerInfo{{Address: "s4"}}},
			{tenantId: 42, begin: []byte("a"), end: []byte("z"), servers: []ServerInfo{{Address: "t42"}}},
		},
	}

	// Remove middle shards [d,g) and [g,k).
	lc.invalidateRange([]byte("d"), []byte("k"), NoTenantID)

	if len(lc.entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(lc.entries))
	}

	// Verify sorted.
	for i := 1; i < len(lc.entries); i++ {
		if !entryLess(&lc.entries[i-1], &lc.entries[i]) {
			t.Fatalf("entries not sorted at index %d", i)
		}
	}

	// Verify correct entries remain.
	if !bytes.Equal(lc.entries[0].begin, []byte("a")) {
		t.Fatalf("expected first entry begin=a, got %q", lc.entries[0].begin)
	}
	if !bytes.Equal(lc.entries[1].begin, []byte("k")) {
		t.Fatalf("expected second entry begin=k, got %q", lc.entries[1].begin)
	}
	if lc.entries[2].tenantId != 42 {
		t.Fatalf("expected third entry to be tenant 42, got %d", lc.entries[2].tenantId)
	}
}
