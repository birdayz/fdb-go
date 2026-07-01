package client

import (
	"encoding/binary"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// ============================================================================
// grvCache.tryCache / update / invalidate — fresh-version cache state machine.
// ============================================================================

func TestGRVCache_TryCacheBeforeUpdate(t *testing.T) {
	t.Parallel()
	c := &grvCache{}
	if v, ok := c.tryCache(grvPriorityDefault); ok || v != 0 {
		t.Errorf("got (%d, %v), want (0, false)", v, ok)
	}
}

func TestGRVCache_UpdateThenTryCacheReturnsValue(t *testing.T) {
	t.Parallel()
	c := &grvCache{}
	c.updateFromGRV(time.Now(), 9999)
	v, ok := c.tryCache(grvPriorityDefault)
	if !ok || v != 9999 {
		t.Errorf("got (%d, %v), want (9999, true)", v, ok)
	}
}

func TestGRVCache_TryCacheStaleVersion(t *testing.T) {
	t.Parallel()
	c := &grvCache{}
	// Stamp the cache as updated 1 second ago (way past maxVersionCacheLag = 100ms).
	c.version.Store(1234)
	c.lastTime.Store(time.Now().Add(-time.Second).UnixNano())
	if v, ok := c.tryCache(grvPriorityDefault); ok {
		t.Errorf("stale entry returned: got (%d, true), want stale-miss", v)
	}
}

func TestGRVCache_SystemImmediateServesCache(t *testing.T) {
	t.Parallel()
	// #16: C++'s cache gate does NOT exclude SYSTEM_IMMEDIATE — rkThrottlingCooledDown(IMMEDIATE)
	// returns true (NativeAPI.actor.cpp:7485-7486, IMMEDIATE bypasses ratekeeper), so an opted-in
	// IMMEDIATE read is served from a fresh cache exactly like DEFAULT. (Was TestGRVCache_SystemImmediateBypass,
	// which pinned the pre-#16 divergence where Go excluded IMMEDIATE.)
	c := &grvCache{}
	c.updateFromGRV(time.Now(), 9999)
	if v, ok := c.tryCache(grvPrioritySystemImmediate); !ok || v != 9999 {
		t.Errorf("SYSTEM_IMMEDIATE must serve a fresh cache (#16); got (%d, %v), want (9999, true)", v, ok)
	}
}

func TestGRVCache_UpdateMonotonicNoBackwards(t *testing.T) {
	t.Parallel()
	c := &grvCache{}
	c.updateFromGRV(time.Now(), 100)
	c.updateFromGRV(time.Now(), 50) // older — must be rejected
	v, _ := c.tryCache(grvPriorityDefault)
	if v != 100 {
		t.Errorf("got %d, want 100 (older update must be ignored)", v)
	}
}

func TestGRVCache_Invalidate(t *testing.T) {
	t.Parallel()
	c := &grvCache{}
	c.updateFromGRV(time.Now(), 100)
	c.invalidate()
	if v, ok := c.tryCache(grvPriorityDefault); ok || v != 0 {
		t.Errorf("got (%d, %v), want (0, false) after invalidate", v, ok)
	}
}

func TestGRVCache_BatchPriorityRkThrottle(t *testing.T) {
	t.Parallel()
	c := &grvCache{}
	c.updateFromGRV(time.Now(), 100)
	// Mark BATCH priority as throttled less than grvCacheRKCooldown ago.
	c.lastRkBatch.Store(time.Now().UnixNano())
	if _, ok := c.tryCache(grvPriorityBatch); ok {
		t.Error("BATCH priority must miss cache while ratekeeper throttled")
	}
	// DEFAULT priority should NOT be affected by BATCH throttle.
	if _, ok := c.tryCache(grvPriorityDefault); !ok {
		t.Error("DEFAULT priority should not be affected by lastRkBatch")
	}
}

// ============================================================================
// updateMinAcceptable — ratchets atomically upward only.
// ============================================================================

// updateMinAcceptable tracks the SMALLEST version seen (std::min; RFC-104), with
// 0 = unset: the first version sets the floor, a smaller one lowers it, a larger
// one is ignored (the floor must not rise past a pinned version).
func TestUpdateMinAcceptable_TracksMinimum(t *testing.T) {
	t.Parallel()
	var min atomic.Int64
	updateMinAcceptable(&min, 100) // first → sets the floor
	if v := min.Load(); v != 100 {
		t.Errorf("got %d, want 100", v)
	}
	updateMinAcceptable(&min, 200) // larger → ignored
	if v := min.Load(); v != 100 {
		t.Errorf("got %d, want 100 (a larger version must not raise the floor)", v)
	}
	updateMinAcceptable(&min, 50) // smaller → lowers the floor
	if v := min.Load(); v != 50 {
		t.Errorf("got %d, want 50 (a smaller version lowers the floor)", v)
	}
	updateMinAcceptable(&min, 0) // unset/invalid → ignored
	if v := min.Load(); v != 50 {
		t.Errorf("got %d, want 50 (v<=0 ignored)", v)
	}
}

// ============================================================================
// validateVersion — rejects too-old + absurd-future versions client-side.
// ============================================================================

// A user-set read version BELOW the smallest-seen floor (minAcceptableReadVersion)
// is rejected as transaction_too_old — a genuinely-ancient pinned version. The
// floor is the SMALLEST version seen (std::min; RFC-104), so this fires only for
// versions below anything the client has observed, never for a recent pin.
func TestValidateVersion_BelowMinReturnsTooOld(t *testing.T) {
	t.Parallel()
	db := &database{}
	db.minAcceptableReadVersion.Store(1000)
	err := db.validateVersion(500)
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionTooOld {
		t.Errorf("got %v, want FDBError ErrTransactionTooOld (1007)", err)
	}
}

func TestValidateVersion_AtMinIsAccepted(t *testing.T) {
	t.Parallel()
	db := &database{}
	db.minAcceptableReadVersion.Store(1000)
	if err := db.validateVersion(1000); err != nil {
		t.Errorf("got %v, want nil at exactly min", err)
	}
}

func TestValidateVersion_NoMinAcceptsAnyReasonable(t *testing.T) {
	t.Parallel()
	db := &database{} // minAcceptableReadVersion = 0 → no floor check
	if err := db.validateVersion(1); err != nil {
		t.Errorf("got %v, want nil when no min set", err)
	}
}

func TestValidateVersion_AbsurdFutureRejected(t *testing.T) {
	t.Parallel()
	db := &database{}
	err := db.validateVersion(1_000_000_000_000_001)
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrFutureVersion {
		t.Errorf("got %v, want FDBError ErrFutureVersion (1009)", err)
	}
}

// ============================================================================
// grvBatcherIndex / grvPriorityToPriority — priority encoding round-trip.
// ============================================================================

func TestGRVBatcherIndex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		flags uint32
		want  int
	}{
		{"batch", grvPriorityBatch, grvBatcherBatch},
		{"default", grvPriorityDefault, grvBatcherDefault},
		{"system immediate", grvPrioritySystemImmediate, grvBatcherSystemImmediate},
		{"batch with extra flag", grvPriorityBatch | grvFlagCausalReadRisky, grvBatcherBatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := grvBatcherIndex(tt.flags); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGRVPriorityToPriority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		flags uint32
		want  TransactionPriority
	}{
		{"batch", grvPriorityBatch, PriorityBatch},
		{"default", grvPriorityDefault, PriorityDefault},
		{"system immediate", grvPrioritySystemImmediate, PrioritySystemImmediate},
		{"unknown bits → default", 0xDEAD0000 & grvPriorityMask, PriorityDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := grvPriorityToPriority(tt.flags); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// ============================================================================
// buildGetReadVersionRequest — wire-construction round-trip.
// ============================================================================

func TestBuildGetReadVersionRequest_RoundTrip(t *testing.T) {
	t.Parallel()
	const (
		flags    uint32 = grvPriorityBatch | grvFlagCausalReadRisky
		txnCount uint32 = 17
	)
	replyToken := transport.UID{First: 0xCAFE, Second: 0xBABE}
	span := types.SpanContext{
		TraceID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:  0x1234,
		Flags:   traceFlagSampled,
	}

	body := buildGetReadVersionRequest(replyToken, flags, txnCount, span)

	var req types.GetReadVersionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if req.Flags != flags {
		t.Errorf("Flags: got %#x, want %#x", req.Flags, flags)
	}
	// The GetReadVersionRequest must carry the span it is given (C++
	// GetReadVersionRequest req(span.context,…), NativeAPI.actor.cpp:7245).
	// Revert-prove: drop the SpanContext field in buildGetReadVersionRequest → zero → red.
	if req.SpanContext != span {
		t.Errorf("SpanContext: got %+v, want %+v", req.SpanContext, span)
	}
	if req.TransactionCount != txnCount {
		t.Errorf("TransactionCount: got %d, want %d", req.TransactionCount, txnCount)
	}
	if req.MaxVersion != InvalidVersion {
		t.Errorf("MaxVersion: got %d, want %d (InvalidVersion)", req.MaxVersion, InvalidVersion)
	}
	first := binary.LittleEndian.Uint64(req.Reply.Token[0:8])
	second := binary.LittleEndian.Uint64(req.Reply.Token[8:16])
	if first != replyToken.First || second != replyToken.Second {
		t.Errorf("reply token: got {%x,%x}, want {%x,%x}",
			first, second, replyToken.First, replyToken.Second)
	}
}

// TestGRVCache_CommitUpdateAdvancesFreshness pins RFC-104: a successful commit
// advances BOTH the cached version and the freshness clock, matching C++
// updateCachedReadVersion at the commit site (NativeAPI.actor.cpp:6657,
// t=now()). This REVERSES the RFC-096 "commit must not extend freshness"
// behavior, which existed only to compensate for the previous always-on,
// enforcement-carrying cache. With the cache now opt-in/default-off, every
// default transaction takes the fresh-GRV path and hits the real locked check
// at the consumption site, so the divergence is gone. Population is
// UNCONDITIONAL — update() runs for every committing transaction regardless of
// USE_GRV_CACHE; only cache READS are opt-in.
func TestGRVCache_CommitUpdateAdvancesFreshness(t *testing.T) {
	t.Parallel()
	var c grvCache
	// A real GRV reply older than maxVersionCacheLag — the cache is stale.
	c.updateFromGRV(time.Now().Add(-2*maxVersionCacheLag), 100)
	if _, ok := c.tryCache(grvPriorityDefault); ok {
		t.Fatal("precondition: the cache should be stale before the commit")
	}
	// A commit "now" advances the version AND renews freshness (C++ :6657).
	c.update(200)
	v, ok := c.tryCache(grvPriorityDefault)
	if !ok || v != 200 {
		t.Fatalf("tryCache = (%d, %v), want (200, true): a commit must advance version AND freshness", v, ok)
	}
	// Monotonicity of the commit path: a backwards version is rejected.
	c.update(150)
	if got := c.version.Load(); got != 200 {
		t.Fatalf("version = %d after backwards commit update, want 200", got)
	}
}
