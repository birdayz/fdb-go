package client

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func TestGRVBatcherInitialWindow(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := openTestDB(t, ctx)
	for priority, batcher := range db.db.grvBatchers {
		batcher.mu.Lock()
		window := batcher.batchTime
		batcher.mu.Unlock()
		// NativeAPI.actor.cpp readVersionBatcher starts batchTime at zero.
		if window != 0 {
			t.Errorf("priority %d: initial GRV batch window=%v, want zero", priority, window)
		}
	}
}

func newDeadlineRecoveryGRVDatabase(t *testing.T, ctx context.Context) (*Database, *atomic.Bool, <-chan struct{}) {
	t.Helper()
	sd := newSimDialer()
	db := newTestDatabase(t, ctx, sharedClusterFile, sd.dial)
	t.Cleanup(func() { db.Close() })
	if _, err := db.CreateTransaction().GetReadVersion(ctx); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	recovering := &atomic.Bool{}
	recovering.Store(true)
	injected := make(chan struct{}, 1)
	brokenPromise := (&types.ErrorOrError{ErrorCode: 1100}).MarshalFDB()
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		if len(body) >= 8 && binary.LittleEndian.Uint32(body[4:8]) == grvReplyEnvelopeFileID && recovering.Load() {
			select {
			case injected <- struct{}{}:
			default:
			}
			return brokenPromise, false
		}
		return body, false
	})
	proxies, _ := db.db.getGRVProxies()
	if len(proxies) == 0 {
		t.Fatal("no GRV proxies after warmup")
	}
	for _, proxy := range proxies {
		sd.armAddr(proxy.Address)
	}
	return db, recovering, injected
}

func TestGRVBatcherRecoveryOutlivesCoordinatorTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, recovering, injected := newDeadlineRecoveryGRVDatabase(t, ctx)
	done := make(chan grvResult, 1)
	go func() {
		rv, err := db.CreateTransaction().GetReadVersion(ctx)
		done <- grvResult{version: rv, err: err}
	}()
	select {
	case <-injected:
	case <-time.After(10 * time.Second):
		t.Fatal("no GRV reply was faulted")
	}

	// Cross the actual former deadline, not a shortened test-only surrogate.
	timer := time.NewTimer(31 * time.Second)
	defer timer.Stop()
	select {
	case result := <-done:
		t.Fatalf("GRV ended during recovery: version=%d error=%v; the caller is still live", result.version, result.err)
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	recovering.Store(false)
	select {
	case result := <-done:
		if result.err != nil || result.version <= 0 {
			t.Fatalf("GRV after recovery: version=%d error=%v", result.version, result.err)
		}
	case <-ctx.Done():
		t.Fatal("GRV did not recover:", ctx.Err())
	}
	if db.db.metrics.Snapshot().GRVInBandMaybeDelivered == 0 {
		t.Fatal("injected failures never reached the GRV retry loop")
	}
}

// TestNextGRVRefreshDelay covers the C++-equivalent adaptive delay formula.
// See nextGRVRefreshDelay doc for the porting reference.
func TestNextGRVRefreshDelay(t *testing.T) {
	t.Parallel()

	// Anchor "now" so all subtests use deterministic durations.
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		lastProxy time.Time
		lastTime  time.Time
		grvDelay  time.Duration
		want      time.Duration
		// wantApprox: tolerance for non-exact comparisons (e.g. EMA paths).
	}{
		{
			// Fresh cache, fresh proxy contact, low latency.
			// proxyBudget = 200ms - 0 = 200ms
			// cacheBudget = (100ms - 1ms) - 0 = 99ms
			// min = 99ms (cache is binding)
			name:      "fresh cache, low latency",
			lastProxy: now,
			lastTime:  now,
			grvDelay:  1 * time.Millisecond,
			want:      99 * time.Millisecond,
		},
		{
			// High observed latency eats most of the cache budget.
			// cacheBudget = (100ms - 50ms) - 0 = 50ms
			// proxyBudget = 200ms - 0 = 200ms
			// min = 50ms
			name:      "fresh cache, high latency",
			lastProxy: now,
			lastTime:  now,
			grvDelay:  50 * time.Millisecond,
			want:      50 * time.Millisecond,
		},
		{
			// Cache aged 80ms — only 19ms (= 99-80) remain before staleness.
			name:      "aged cache, low latency",
			lastProxy: now,
			lastTime:  now.Add(-80 * time.Millisecond),
			grvDelay:  1 * time.Millisecond,
			want:      19 * time.Millisecond,
		},
		{
			// Cache already expired — clamp to grvRefreshMin.
			name:      "cache stale",
			lastProxy: now,
			lastTime:  now.Add(-200 * time.Millisecond),
			grvDelay:  1 * time.Millisecond,
			want:      grvRefreshMin,
		},
		{
			// Proxy contact is the binding deadline.
			// proxyBudget = 200ms - 150ms = 50ms
			// cacheBudget = (100ms - 1ms) - 0 = 99ms
			// min = 50ms (proxy binding)
			name:      "proxy contact aging",
			lastProxy: now.Add(-150 * time.Millisecond),
			lastTime:  now,
			grvDelay:  1 * time.Millisecond,
			want:      50 * time.Millisecond,
		},
		{
			// Both budgets negative → clamp to grvRefreshMin.
			name:      "both budgets exhausted",
			lastProxy: now.Add(-1 * time.Second),
			lastTime:  now.Add(-1 * time.Second),
			grvDelay:  1 * time.Millisecond,
			want:      grvRefreshMin,
		},
		{
			// Pathological: latency exceeds cache lag entirely.
			// cacheBudget = (100ms - 200ms) - 0 = -100ms → clamp to min.
			name:      "latency exceeds cache lag",
			lastProxy: now,
			lastTime:  now,
			grvDelay:  200 * time.Millisecond,
			want:      grvRefreshMin,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := nextGRVRefreshDelay(now, tt.lastProxy, tt.lastTime, tt.grvDelay)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// BenchmarkNextGRVRefreshDelay measures the per-iteration math overhead
// (excluding the timer alloc). The old fixed-tick refresher did ~no math
// per tick; we now do this on every iteration. Should be a few ns at most.
func BenchmarkNextGRVRefreshDelay(b *testing.B) {
	now := time.Now()
	lastProxy := now.Add(-10 * time.Millisecond)
	lastTime := now.Add(-30 * time.Millisecond)
	grvDelay := 5 * time.Millisecond
	b.ResetTimer()
	for b.Loop() {
		_ = nextGRVRefreshDelay(now, lastProxy, lastTime, grvDelay)
	}
}
