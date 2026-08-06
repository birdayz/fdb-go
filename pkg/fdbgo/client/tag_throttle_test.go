package client

import (
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// The GRV reply's tagThrottleInfo is TransactionTagMap<ClientTagThrottleLimits>
// — an unordered_map, which flat_buffers.h serializes as a VECTOR OF PAIR
// OBJECTS, not a length-prefixed byte blob. These tests therefore exercise the
// decoded entries; the byte framing itself is pinned against real C++ output by
// the ground-truth tests in pkg/fdbgo/wire/types.
//
// The tests these replaced hand-built a `count | tagLen | tag | f64 | f64` byte
// stream and asserted the old parser round-tripped it. That layout is not what a
// proxy sends, so the suite was green against an encoding the server never uses
// — self-consistency, not coverage.

func TestParseTagThrottleInfoEmpty(t *testing.T) {
	t.Parallel()
	if got := parseTagThrottleInfo(nil); got != nil {
		t.Fatalf("expected nil for nil entries, got %v", got)
	}
	if got := parseTagThrottleInfo([]types.TransactionTagThrottle{}); got != nil {
		t.Fatalf("expected nil for empty entries, got %v", got)
	}
}

func TestParseTagThrottleInfoSingleTag(t *testing.T) {
	t.Parallel()
	before := time.Now()
	result := parseTagThrottleInfo([]types.TransactionTagThrottle{
		{Tag: []byte("myTag"), Limits: types.ClientTagThrottleLimits{TpsRate: 100.0, Duration: 5.0}},
	})
	after := time.Now()

	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	entry, ok := result["myTag"]
	if !ok {
		t.Fatal("missing 'myTag' entry")
	}
	if entry.tpsRate != 100.0 {
		t.Errorf("tpsRate = %v, want 100.0", entry.tpsRate)
	}
	// ClientTagThrottleLimits carries a RELATIVE duration on the wire; the
	// parser re-anchors it to the local clock, so the expiration must land
	// 5s after the instant the call was made.
	if entry.expiration.Before(before.Add(5*time.Second)) || entry.expiration.After(after.Add(5*time.Second)) {
		t.Errorf("expiration %v not within [%v, %v]", entry.expiration,
			before.Add(5*time.Second), after.Add(5*time.Second))
	}
}

func TestParseTagThrottleInfoMultipleTags(t *testing.T) {
	t.Parallel()
	result := parseTagThrottleInfo([]types.TransactionTagThrottle{
		{Tag: []byte("alpha"), Limits: types.ClientTagThrottleLimits{TpsRate: 1.5, Duration: 1.0}},
		{Tag: []byte("beta"), Limits: types.ClientTagThrottleLimits{TpsRate: 2.5, Duration: 2.0}},
		{Tag: []byte("gamma"), Limits: types.ClientTagThrottleLimits{TpsRate: 3.5, Duration: 3.0}},
	})
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}
	for tag, wantRate := range map[string]float64{"alpha": 1.5, "beta": 2.5, "gamma": 3.5} {
		entry, ok := result[tag]
		if !ok {
			t.Fatalf("missing %q entry", tag)
		}
		if entry.tpsRate != wantRate {
			t.Errorf("%s tpsRate = %v, want %v", tag, entry.tpsRate, wantRate)
		}
	}
}

// An empty tag is a legal StringRef; it must not be confused with "no entry".
func TestParseTagThrottleInfoEmptyTagName(t *testing.T) {
	t.Parallel()
	result := parseTagThrottleInfo([]types.TransactionTagThrottle{
		{Tag: nil, Limits: types.ClientTagThrottleLimits{TpsRate: 7.0, Duration: 1.0}},
	})
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if entry, ok := result[""]; !ok || entry.tpsRate != 7.0 {
		t.Fatalf("empty-named tag not preserved: %#v", result)
	}
}

func TestThrottleDuration(t *testing.T) {
	t.Parallel()

	t.Run("expired", func(t *testing.T) {
		t.Parallel()
		lim := &clientTagThrottleLimits{
			tpsRate:    100,
			expiration: time.Now().Add(-1 * time.Second),
		}
		if d := lim.throttleDuration(); d != 0 {
			t.Fatalf("expected 0 for expired, got %v", d)
		}
	})

	t.Run("zero_rate", func(t *testing.T) {
		t.Parallel()
		lim := &clientTagThrottleLimits{
			tpsRate:    0,
			expiration: time.Now().Add(3 * time.Second),
		}
		d := lim.throttleDuration()
		if d < 2*time.Second || d > 4*time.Second {
			t.Fatalf("expected ~3s for zero tpsRate, got %v", d)
		}
	})

	t.Run("nonzero_rate", func(t *testing.T) {
		t.Parallel()
		// At 50 TPS, one slot = 1/50 = 20ms.
		lim := &clientTagThrottleLimits{
			tpsRate:    50,
			expiration: time.Now().Add(2 * time.Second),
		}
		d := lim.throttleDuration()
		if d < 15*time.Millisecond || d > 25*time.Millisecond {
			t.Fatalf("expected ~20ms (1/50 TPS slot), got %v", d)
		}
	})

	t.Run("low_rate_capped_by_remaining", func(t *testing.T) {
		t.Parallel()
		// At 0.1 TPS, one slot = 10s, but only 2s remaining → capped at 2s.
		lim := &clientTagThrottleLimits{
			tpsRate:    0.1,
			expiration: time.Now().Add(2 * time.Second),
		}
		d := lim.throttleDuration()
		if d < 1500*time.Millisecond || d > 2500*time.Millisecond {
			t.Fatalf("expected ~2s (capped by remaining), got %v", d)
		}
	})

	t.Run("high_rate", func(t *testing.T) {
		t.Parallel()
		// At 1000 TPS, one slot = 1ms.
		lim := &clientTagThrottleLimits{
			tpsRate:    1000,
			expiration: time.Now().Add(5 * time.Second),
		}
		d := lim.throttleDuration()
		if d < 500*time.Microsecond || d > 2*time.Millisecond {
			t.Fatalf("expected ~1ms (1/1000 TPS slot), got %v", d)
		}
	})
}

func TestTagThrottleStateUpdateAndQuery(t *testing.T) {
	t.Parallel()

	var state tagThrottleState

	// No state yet — should return 0.
	if d := state.maxDuration(PriorityDefault, []string{"tag1"}); d != 0 {
		t.Fatalf("expected 0 for empty state, got %v", d)
	}

	// Update with a throttled tag.
	info := map[string]clientTagThrottleLimits{
		"tag1": {tpsRate: 10, expiration: time.Now().Add(5 * time.Second)},
		"tag2": {tpsRate: 0, expiration: time.Now().Add(2 * time.Second)},
	}
	state.replace(PriorityDefault, info)

	// Query tag1 — at 10 TPS, one slot = 100ms.
	d1 := state.maxDuration(PriorityDefault, []string{"tag1"})
	if d1 < 50*time.Millisecond || d1 > 150*time.Millisecond {
		t.Fatalf("expected ~100ms for tag1 (1/10 TPS slot), got %v", d1)
	}

	// Query both — max should be ~2s (tag2 has tpsRate=0, returns full remaining).
	dBoth := state.maxDuration(PriorityDefault, []string{"tag1", "tag2"})
	if dBoth < 1500*time.Millisecond || dBoth > 2500*time.Millisecond {
		t.Fatalf("expected ~2s for max(tag1,tag2) where tag2 is zero-rate, got %v", dBoth)
	}

	// Query at different priority — should return 0.
	if d := state.maxDuration(PriorityBatch, []string{"tag1"}); d != 0 {
		t.Fatalf("expected 0 for different priority, got %v", d)
	}

	// Replace with only tag2 — tag1 should be gone (server stopped throttling it).
	info2 := map[string]clientTagThrottleLimits{
		"tag2": {tpsRate: 0, expiration: time.Now().Add(10 * time.Second)},
	}
	state.replace(PriorityDefault, info2)

	// tag1 should be gone.
	if d := state.maxDuration(PriorityDefault, []string{"tag1"}); d != 0 {
		t.Fatalf("expected 0 for removed tag1, got %v", d)
	}
	// tag2 should be updated.
	d2 := state.maxDuration(PriorityDefault, []string{"tag2"})
	if d2 < 8*time.Second || d2 > 11*time.Second {
		t.Fatalf("expected ~10s for updated tag2, got %v", d2)
	}
}

func TestNextBackoffTagThrottled(t *testing.T) {
	t.Parallel()

	db := &database{}
	// Populate tag throttle state: tag "slow" throttled for 3s.
	info := map[string]clientTagThrottleLimits{
		"slow": {tpsRate: 0, expiration: time.Now().Add(3 * time.Second)},
	}
	db.tagThrottles.replace(PriorityDefault, info)

	tx := &Transaction{
		db:        db,
		txOptions: txOptions{priority: PriorityDefault, tags: []string{"slow"}},
	}

	delay := tx.nextBackoff(ErrTagThrottled)
	// Should be at least ~2s (the tag throttle duration), capped at 3s (not at 7s recheck).
	if delay < 2*time.Second {
		t.Fatalf("expected delay >= 2s from tag throttle, got %v", delay)
	}
	if delay > tagThrottleRecheckInterval {
		t.Fatalf("expected delay <= %v (TAG_THROTTLE_RECHECK_INTERVAL), got %v", tagThrottleRecheckInterval, delay)
	}
}

// TestNextBackoff_TagThrottleCapAtRecheckInterval pins the tag-throttle backoff CAP at
// TAG_THROTTLE_RECHECK_INTERVAL = 5s (C++ ClientKnobs.cpp:296; getBackoff NativeAPI.actor.cpp:6100),
// using a throttle duration that EXCEEDS the cap so the cap (not the duration) dominates. Revert-proof:
// with the prior erroneous 7s constant a 20s throttle backs off 7s and this fails.
func TestNextBackoff_TagThrottleCapAtRecheckInterval(t *testing.T) {
	t.Parallel()

	db := &database{}
	// Throttle "slow" for ~20s — well above the 5s recheck cap.
	info := map[string]clientTagThrottleLimits{
		"slow": {tpsRate: 0, expiration: time.Now().Add(20 * time.Second)},
	}
	db.tagThrottles.replace(PriorityDefault, info)

	tx := &Transaction{db: db, txOptions: txOptions{priority: PriorityDefault, tags: []string{"slow"}}}
	delay := tx.nextBackoff(ErrTagThrottled)
	// min(TAG_THROTTLE_RECHECK_INTERVAL=5s, throttleDuration~20s) = 5s exactly.
	if delay != 5*time.Second {
		t.Fatalf("tag-throttle backoff must cap at 5s (TAG_THROTTLE_RECHECK_INTERVAL), got %v", delay)
	}
}

func TestNextBackoffNoTagsNormalBackoff(t *testing.T) {
	t.Parallel()

	tx := &Transaction{
		db:        &database{},
		txOptions: txOptions{priority: PriorityDefault}, // no tags set
	}

	delay := tx.nextBackoff(ErrTagThrottled)
	// Without tags, should use normal exponential backoff (starts at 10ms, jittered).
	if delay > 20*time.Millisecond {
		t.Fatalf("expected small delay without tags, got %v", delay)
	}
}

func TestNextBackoffProxyTagThrottledAccumulates(t *testing.T) {
	t.Parallel()

	tx := &Transaction{
		db:        &database{},
		txOptions: txOptions{priority: PriorityDefault, tags: []string{"fast"}},
	}

	if tx.proxyTagThrottledDuration != 0 {
		t.Fatal("expected 0 initial proxyTagThrottledDuration")
	}

	tx.nextBackoff(ErrProxyTagThrottled)
	if tx.proxyTagThrottledDuration != proxyMaxTagThrottleDuration.Seconds() {
		t.Fatalf("expected %f, got %f", proxyMaxTagThrottleDuration.Seconds(), tx.proxyTagThrottledDuration)
	}

	tx.nextBackoff(ErrProxyTagThrottled)
	if tx.proxyTagThrottledDuration != 2*proxyMaxTagThrottleDuration.Seconds() {
		t.Fatalf("expected %f, got %f", 2*proxyMaxTagThrottleDuration.Seconds(), tx.proxyTagThrottledDuration)
	}
}

func TestResetClearsProxyThrottleDuration(t *testing.T) {
	t.Parallel()

	tx := &Transaction{
		db:                        &database{},
		txOptions:                 txOptions{priority: PriorityDefault, tags: []string{"mytag"}},
		proxyTagThrottledDuration: 15.0,
	}

	tx.reset(false)

	if tx.proxyTagThrottledDuration != 0 {
		t.Fatalf("expected 0 after reset, got %f", tx.proxyTagThrottledDuration)
	}
	// Tags are NON-persistent — cleared on reset (RFC-171 / #9,#14; C++ TransactionOptions::clear sets
	// tags = TagSet{}, NativeAPI.actor.cpp:6131-6144). Pre-RFC-171 Go wrongly preserved them.
	if len(tx.tags) != 0 {
		t.Fatalf("expected tags CLEARED after reset (non-persistent), got %v", tx.tags)
	}
}

func TestSetTag(t *testing.T) {
	t.Parallel()

	tx := &Transaction{}
	tx.SetTag("foo")
	tx.SetTag("bar")
	if len(tx.tags) != 2 || tx.tags[0] != "foo" || tx.tags[1] != "bar" {
		t.Fatalf("expected [foo, bar], got %v", tx.tags)
	}
}
