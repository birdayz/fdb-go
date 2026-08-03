package client

// The read-version instant must name when the version's MVCC window OPENED,
// which is not the same as when this client learned the version.
//
// ReadVersionInstant exists so a layer above can budget against FDB's
// 5-second window (RFC-198). A budget is only safe if the anchor is at or
// BEFORE the true window start: anchor early and the budget expires sooner
// than FDB's window, which costs a retry; anchor late and the budget believes
// there is time left that the cluster has already spent, which is a page that
// starts and dies on transaction_too_old (1007) — the exact failure the
// pre-emption exists to prevent.
//
// Stamping time.Now() after getReadVersion returned got this wrong on two
// paths that are invisible in a fast local test:
//
//   - a delayed proxy reply — the version was assigned when the proxy handled
//     the request, but the stamp landed after the round trip;
//   - a USE_GRV_CACHE hit — the version can be up to maxVersionCacheLag old
//     and the stamp claimed it was brand new.
//
// The anchor therefore travels WITH the version: the pre-RPC request time for
// a proxy round trip, and the original obtain-time for a cache hit. Both are
// lower bounds, which is the safe direction.

import (
	"context"
	"testing"
	"time"
)

// TestGRVCache_TryCacheReportsTheObtainInstantNotNow is the deterministic half:
// no cluster, no timing luck. A version cached 60ms ago must be reported with
// its 60ms-old stamp, because that is when its window opened.
func TestGRVCache_TryCacheReportsTheObtainInstantNotNow(t *testing.T) {
	t.Parallel()
	c := &grvCache{}

	// Inside maxVersionCacheLag (100ms), so this is a HIT — but not fresh.
	obtained := time.Now().Add(-60 * time.Millisecond)
	c.updateFromGRV(obtained, 4242)

	v, at, ok := c.tryCache(grvPriorityDefault)
	if !ok {
		t.Fatalf("cache miss on an entry %v old, well inside maxVersionCacheLag (%v): "+
			"this test can say nothing about the instant if it never gets a hit",
			time.Since(obtained), maxVersionCacheLag)
	}
	if v != 4242 {
		t.Fatalf("cached version = %d, want 4242", v)
	}

	// The reported instant is the OBTAIN time, to the nanosecond the cache
	// stores. Anything near "now" means the cache is re-dating versions it
	// serves, and every USE_GRV_CACHE transaction then budgets as if its
	// window had just opened.
	if !at.Equal(obtained) {
		t.Fatalf("cache reported instant %v for a version obtained at %v (skew %v): a served "+
			"version carries the age it actually has. Reporting the serve time instead "+
			"hands the caller a full 5-second budget for a window that is already up to "+
			"%v spent, and the page that starts on it dies with 1007",
			at, obtained, at.Sub(obtained), maxVersionCacheLag)
	}
	if at.After(time.Now()) {
		t.Fatalf("cache reported a FUTURE instant %v: an anchor ahead of now makes every "+
			"elapsed measurement negative and the budget effectively infinite", at)
	}
}

// TestGRVCache_MissReportsNoInstant pins the other direction: a miss must not
// hand back a stamp at all. A caller that ignored ok and used the instant
// would otherwise anchor on a version it never received.
func TestGRVCache_MissReportsNoInstant(t *testing.T) {
	t.Parallel()

	t.Run("empty cache", func(t *testing.T) {
		t.Parallel()
		c := &grvCache{}
		v, at, ok := c.tryCache(grvPriorityDefault)
		if ok {
			t.Fatalf("empty cache reported a hit (%d)", v)
		}
		if !at.IsZero() {
			t.Fatalf("empty cache returned instant %v, want the zero time", at)
		}
	})

	t.Run("stale entry", func(t *testing.T) {
		t.Parallel()
		c := &grvCache{}
		// Past maxVersionCacheLag: a miss, and the stamp must not leak.
		c.updateFromGRV(time.Now().Add(-time.Second), 777)
		v, at, ok := c.tryCache(grvPriorityDefault)
		if ok {
			t.Fatalf("stale entry reported a hit (%d)", v)
		}
		if !at.IsZero() {
			t.Fatalf("stale miss returned instant %v, want the zero time: a caller that "+
				"trusted it would anchor on a version too old to serve", at)
		}
	})
}

// TestReadVersionInstant_MatchesTheGRVRequestTime pins the proxy-round-trip
// half against a real cluster: the transaction's anchor is the instant the GRV
// was REQUESTED, which is the same instant the shared cache records for that
// version. Equality is the assertion — a locally re-stamped now() would differ
// by the round-trip time, which a bracketing assertion is too coarse to see.
func TestReadVersionInstant_MatchesTheGRVRequestTime(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	if _, err := tx.Get(ctx, []byte(t.Name())); err != nil {
		t.Fatalf("get: %v", err)
	}
	inst, ok := tx.ReadVersionInstant()
	if !ok {
		t.Fatal("ReadVersionInstant ok=false after a read that took a read version")
	}

	// The batcher stamps the cache from the SAME pre-RPC requestTime it hands
	// to the waiters, so a correctly propagated anchor equals it exactly.
	cached := time.Unix(0, db.db.grvCache.lastTime.Load())
	if cached.IsZero() {
		t.Fatal("the GRV cache has no stamp after a GRV — this test cannot compare against it")
	}
	if !inst.Equal(cached) {
		t.Fatalf("transaction anchor %v != the GRV's own request time %v (skew %v): the "+
			"anchor is being re-stamped locally after the reply instead of travelling "+
			"with the version, so it names an instant AFTER the window opened and the "+
			"budget under-counts the transaction's age by the round-trip time",
			inst, cached, inst.Sub(cached))
	}
	if inst.After(time.Now()) {
		t.Fatalf("anchor %v is in the future", inst)
	}
}
