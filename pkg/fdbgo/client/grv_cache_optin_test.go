package client

import (
	"context"
	"testing"
	"time"
)

// TestFDB_GRVCache_OptInOnly pins RFC-104: the GRV cache is opt-in
// (USE_GRV_CACHE, default off). A DEFAULT transaction never serves a cached read
// version — each issues its own real proxy GRV — and the background refresher
// never starts. A SetUseGrvCache() transaction reading a warm cache serves the
// cached version (a cache HIT) and starts the refresher.
//
// Deterministic via the grvCacheHits + transactionReadVersionsCompleted counters
// and the refresherStarted seam — no goroutine-count or timer-vs-reply flakiness
// (RFC-104 test plan, Torvalds). The grvCacheHits==0 + exactly-N-real-GRVs pair
// proves the cache is GATED OFF, not merely that it happened to be stale.
//
// openTestDB returns a FRESH database handle (its own grvCache, metrics, and
// refresher), so these per-handle counters are isolated from other parallel
// tests sharing the container.
func TestFDB_GRVCache_OptInOnly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	batcher := db.db.grvBatchers[grvBatcherDefault]

	// --- DEFAULT transactions: zero cache hits, exactly-N real GRVs ---
	// Issued strictly serially (each read version fetched before the next
	// starts) so the batcher cannot coalesce them into one GRV reply, making the
	// real-GRV count exactly N (no "~N" tolerance).
	before := db.db.metrics.Snapshot()
	const n = 5
	for i := 0; i < n; i++ {
		tx := db.CreateTransaction()
		if err := tx.ensureReadVersion(ctx); err != nil {
			t.Fatalf("default read %d: ensureReadVersion: %v", i, err)
		}
	}
	after := db.db.metrics.Snapshot()
	if got := after.GRVCacheHits - before.GRVCacheHits; got != 0 {
		t.Fatalf("default transactions: GRVCacheHits advanced by %d, want 0 — the cache must be OFF by default", got)
	}
	if got := after.TransactionReadVersionsCompleted - before.TransactionReadVersionsCompleted; got != n {
		t.Fatalf("default transactions: real GRVs = %d, want exactly %d — each default tx must fresh-fetch", got, n)
	}
	if batcher.refresherStarted.Load() {
		t.Fatal("background GRV refresher started for default-only transactions — it must be opt-in")
	}

	// --- OPT-IN transaction: serves a cached version (fail-open on the cached path) ---
	// Warm the cache deterministically: a real GRV populates it (population is
	// unconditional, RFC-104), then refresh its freshness clock to now() so the
	// opt-in read is guaranteed inside maxVersionCacheLag — no timing race.
	warm := db.CreateTransaction()
	if err := warm.ensureReadVersion(ctx); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	v := db.db.grvCache.version.Load()
	if v == 0 {
		t.Fatal("GRV cache not populated by a real reply")
	}
	db.db.grvCache.updateFromGRV(time.Now(), v) // refresh freshness → deterministic hit window

	hitsBefore := db.db.metrics.Snapshot().GRVCacheHits
	optIn := db.CreateTransaction()
	optIn.SetUseGrvCache()
	if err := optIn.ensureReadVersion(ctx); err != nil {
		t.Fatalf("opt-in read: ensureReadVersion: %v", err)
	}
	if got := db.db.metrics.Snapshot().GRVCacheHits - hitsBefore; got != 1 {
		t.Fatalf("opt-in transaction: GRVCacheHits advanced by %d, want 1 — it must serve from the warm cache", got)
	}
	if !batcher.refresherStarted.Load() {
		t.Fatal("background refresher did not start after an opt-in cache hit")
	}
	// The opt-in read adopted the cached version with no real GRV.
	if rv := optIn.readVersion; rv != v {
		t.Fatalf("opt-in read version = %d, want the cached %d", rv, v)
	}
}

// TestFDB_GRVCache_RefresherStartsOnOptInMiss pins the codex PR #291 fix: the background
// GRV refresher starts on the FIRST opt-in request even when the cached version is
// STALE (a cache MISS), not only on a hit — matching C++ getReadVersion, which launches
// backgroundGrvUpdater inside the opt-in gate BEFORE the freshness check
// (NativeAPI.actor.cpp:7507-7509). Starting it only on a hit left a cold/stale cache
// un-warmed: every sparse opt-in read fell through to a real GRV and the cache never
// caught up, defeating the opt-in entirely.
func TestFDB_GRVCache_RefresherStartsOnOptInMiss(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	batcher := db.db.grvBatchers[grvBatcherDefault]

	// Force a guaranteed cache MISS: invalidate() zeroes both the cached version and the
	// freshness clock, so tryCache returns false regardless of any bootstrap warming.
	// (updateFromGRV with a past timestamp won't work — updateTime is monotonic and
	// ignores an older clock.)
	db.db.grvCache.invalidate()
	if batcher.refresherStarted.Load() {
		t.Fatal("refresher started before any opt-in transaction — must be opt-in driven")
	}

	hitsBefore := db.db.metrics.Snapshot().GRVCacheHits
	tx := db.CreateTransaction()
	tx.SetUseGrvCache()
	if err := tx.ensureReadVersion(ctx); err != nil {
		t.Fatalf("opt-in read on cold cache: %v", err)
	}
	if got := db.db.metrics.Snapshot().GRVCacheHits - hitsBefore; got != 0 {
		t.Fatalf("GRVCacheHits advanced by %d on a cold-cache opt-in, want 0 — this must be the MISS path", got)
	}
	if !batcher.refresherStarted.Load() {
		t.Fatal("background refresher did not start on an opt-in cache MISS — a cold/stale cache stays un-warmed (codex PR #291)")
	}
}

// TestFDB_GRVCache_SkipOverridesUse pins that SKIP_GRV_CACHE (1102) wins over
// USE_GRV_CACHE (1101): a transaction that sets both must NOT consult the cache,
// matching the C++ gate `useGrvCache && !skipGrvCache` (NativeAPI.actor.cpp:7505).
func TestFDB_GRVCache_SkipOverridesUse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Warm the cache deterministically (as above).
	warm := db.CreateTransaction()
	if err := warm.ensureReadVersion(ctx); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	v := db.db.grvCache.version.Load()
	if v == 0 {
		t.Fatal("GRV cache not populated")
	}
	db.db.grvCache.updateFromGRV(time.Now(), v)

	hitsBefore := db.db.metrics.Snapshot().GRVCacheHits
	tx := db.CreateTransaction()
	tx.SetUseGrvCache()
	tx.SetSkipGrvCache() // skip wins
	if err := tx.ensureReadVersion(ctx); err != nil {
		t.Fatalf("ensureReadVersion: %v", err)
	}
	if got := db.db.metrics.Snapshot().GRVCacheHits - hitsBefore; got != 0 {
		t.Fatalf("SKIP_GRV_CACHE did not override USE_GRV_CACHE: GRVCacheHits advanced by %d, want 0", got)
	}
}

// TestFDB_GRVCache_ImmediateServesButStartsNoRefresher pins the #16 codex P2: an opted-in
// SYSTEM_IMMEDIATE read SERVES the warm shared cache (the #16 fix) but must NOT start the IMMEDIATE
// batcher's background refresher. Go's refresher is per-batcher and issues its periodic GRVs at the
// batcher's priority, so starting the IMMEDIATE refresher would emit a long-lived stream of
// ratekeeper-bypassing PRIORITY_SYSTEM_IMMEDIATE GRVs from a single opted-in read. C++ instead runs one
// cx-level updater at DEFAULT priority (backgroundGrvUpdater, NativeAPI.actor.cpp:1283), never IMMEDIATE.
// Revert-proof: removing the `b.priority != grvPrioritySystemImmediate` guard in getReadVersion starts
// the IMMEDIATE refresher and reds the refresherStarted assertion.
func TestFDB_GRVCache_ImmediateServesButStartsNoRefresher(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	immBatcher := db.db.grvBatchers[grvBatcherSystemImmediate]

	// Warm the shared cache with a real GRV (population is unconditional), then set its freshness clock
	// to now() so the IMMEDIATE opt-in read is a deterministic hit inside maxVersionCacheLag.
	warm := db.CreateTransaction()
	if err := warm.ensureReadVersion(ctx); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	v := db.db.grvCache.version.Load()
	if v == 0 {
		t.Fatal("GRV cache not populated")
	}
	db.db.grvCache.updateFromGRV(time.Now(), v)
	if immBatcher.refresherStarted.Load() {
		t.Fatal("IMMEDIATE refresher started before any IMMEDIATE opt-in read")
	}

	hitsBefore := db.db.metrics.Snapshot().GRVCacheHits
	tx := db.CreateTransaction()
	tx.SetUseGrvCache()
	tx.SetPriority(PrioritySystemImmediate)
	if err := tx.ensureReadVersion(ctx); err != nil {
		t.Fatalf("IMMEDIATE opt-in read: %v", err)
	}
	// #16: IMMEDIATE serves the warm cache.
	if got := db.db.metrics.Snapshot().GRVCacheHits - hitsBefore; got != 1 {
		t.Fatalf("IMMEDIATE opt-in must serve the warm cache (#16); GRVCacheHits advanced by %d, want 1", got)
	}
	if rv := tx.readVersion; rv != v {
		t.Fatalf("IMMEDIATE opt-in read version = %d, want the cached %d", rv, v)
	}
	// codex #16 P2: but it must NOT start the IMMEDIATE batcher's refresher.
	if immBatcher.refresherStarted.Load() {
		t.Fatal("IMMEDIATE opt-in started the IMMEDIATE refresher — a stream of ratekeeper-bypassing IMMEDIATE GRVs (codex #16 P2)")
	}
}
