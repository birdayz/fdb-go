package client

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func TestParseTagThrottleInfoEmpty(t *testing.T) {
	t.Parallel()
	// nil data
	if got := parseTagThrottleInfo(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	// too short
	if got := parseTagThrottleInfo([]byte{1, 2}); got != nil {
		t.Fatalf("expected nil for short data, got %v", got)
	}
	// count=0
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, 0)
	if got := parseTagThrottleInfo(data); got != nil {
		t.Fatalf("expected nil for count=0, got %v", got)
	}
}

func TestParseTagThrottleInfoSingleTag(t *testing.T) {
	t.Parallel()
	tag := "myTag"
	tpsRate := 100.0
	duration := 5.0 // 5 seconds

	// Build wire data: count(4) + tagLen(4) + tag(5) + tpsRate(8) + duration(8)
	data := make([]byte, 4+4+len(tag)+8+8)
	off := 0
	binary.LittleEndian.PutUint32(data[off:], 1) // count
	off += 4
	binary.LittleEndian.PutUint32(data[off:], uint32(len(tag))) // tagLen
	off += 4
	copy(data[off:], tag) // tag bytes
	off += len(tag)
	binary.LittleEndian.PutUint64(data[off:], math.Float64bits(tpsRate)) // tpsRate
	off += 8
	binary.LittleEndian.PutUint64(data[off:], math.Float64bits(duration)) // duration
	// off += 8

	before := time.Now()
	result := parseTagThrottleInfo(data)
	after := time.Now()

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	entry, ok := result["myTag"]
	if !ok {
		t.Fatal("missing 'myTag' entry")
	}
	if entry.tpsRate != tpsRate {
		t.Fatalf("expected tpsRate=%f, got %f", tpsRate, entry.tpsRate)
	}
	// Expiration should be ~5 seconds from now (between before+5s and after+5s).
	expectedMin := before.Add(5 * time.Second)
	expectedMax := after.Add(5 * time.Second)
	if entry.expiration.Before(expectedMin) || entry.expiration.After(expectedMax) {
		t.Fatalf("expiration %v not in expected range [%v, %v]", entry.expiration, expectedMin, expectedMax)
	}
}

func TestParseTagThrottleInfoMultipleTags(t *testing.T) {
	t.Parallel()
	tags := []struct {
		name     string
		tpsRate  float64
		duration float64
	}{
		{"tag_a", 50.0, 3.0},
		{"tag_b", 0.0, 10.0},
		{"x", 200.0, 0.5},
	}

	// Calculate total size.
	size := 4 // count
	for _, tg := range tags {
		size += 4 + len(tg.name) + 8 + 8
	}
	data := make([]byte, size)
	off := 0
	binary.LittleEndian.PutUint32(data[off:], uint32(len(tags)))
	off += 4
	for _, tg := range tags {
		binary.LittleEndian.PutUint32(data[off:], uint32(len(tg.name)))
		off += 4
		copy(data[off:], tg.name)
		off += len(tg.name)
		binary.LittleEndian.PutUint64(data[off:], math.Float64bits(tg.tpsRate))
		off += 8
		binary.LittleEndian.PutUint64(data[off:], math.Float64bits(tg.duration))
		off += 8
	}

	result := parseTagThrottleInfo(data)
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}
	for _, tg := range tags {
		entry, ok := result[tg.name]
		if !ok {
			t.Fatalf("missing tag %q", tg.name)
		}
		if entry.tpsRate != tg.tpsRate {
			t.Fatalf("tag %q: expected tpsRate=%f, got %f", tg.name, tg.tpsRate, entry.tpsRate)
		}
	}
}

func TestParseTagThrottleInfoTruncated(t *testing.T) {
	t.Parallel()
	// count=2 but only 1 entry's worth of data.
	tag := "abc"
	data := make([]byte, 4+4+len(tag)+8+8)
	off := 0
	binary.LittleEndian.PutUint32(data[off:], 2) // claim 2 entries
	off += 4
	binary.LittleEndian.PutUint32(data[off:], uint32(len(tag)))
	off += 4
	copy(data[off:], tag)
	off += len(tag)
	binary.LittleEndian.PutUint64(data[off:], math.Float64bits(10.0))
	off += 8
	binary.LittleEndian.PutUint64(data[off:], math.Float64bits(1.0))

	// Should parse the first entry and stop gracefully.
	result := parseTagThrottleInfo(data)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry from truncated data, got %d", len(result))
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
		lim := &clientTagThrottleLimits{
			tpsRate:    50,
			expiration: time.Now().Add(2 * time.Second),
		}
		d := lim.throttleDuration()
		if d < 1*time.Second || d > 3*time.Second {
			t.Fatalf("expected ~2s for nonzero rate, got %v", d)
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
	state.update(PriorityDefault, []string{"tag1", "tag2"}, info)

	// Query tag1 — should have ~5s duration.
	d1 := state.maxDuration(PriorityDefault, []string{"tag1"})
	if d1 < 4*time.Second || d1 > 6*time.Second {
		t.Fatalf("expected ~5s for tag1, got %v", d1)
	}

	// Query both — max should be ~5s (tag1 > tag2).
	dBoth := state.maxDuration(PriorityDefault, []string{"tag1", "tag2"})
	if dBoth < 4*time.Second || dBoth > 6*time.Second {
		t.Fatalf("expected ~5s for max(tag1,tag2), got %v", dBoth)
	}

	// Query at different priority — should return 0.
	if d := state.maxDuration(PriorityBatch, []string{"tag1"}); d != 0 {
		t.Fatalf("expected 0 for different priority, got %v", d)
	}

	// Update to remove tag1 (absent from info).
	info2 := map[string]clientTagThrottleLimits{
		"tag2": {tpsRate: 0, expiration: time.Now().Add(10 * time.Second)},
	}
	state.update(PriorityDefault, []string{"tag1", "tag2"}, info2)

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
	db.tagThrottles.update(PriorityDefault, []string{"slow"}, info)

	tx := &Transaction{
		db:       db,
		priority: PriorityDefault,
		tags:     []string{"slow"},
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

func TestNextBackoffNoTagsNormalBackoff(t *testing.T) {
	t.Parallel()

	tx := &Transaction{
		db:       &database{},
		priority: PriorityDefault,
		// No tags set.
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
		db:       &database{},
		priority: PriorityDefault,
		tags:     []string{"fast"},
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
		priority:                  PriorityDefault,
		tags:                      []string{"mytag"},
		proxyTagThrottledDuration: 15.0,
	}

	tx.reset()

	if tx.proxyTagThrottledDuration != 0 {
		t.Fatalf("expected 0 after reset, got %f", tx.proxyTagThrottledDuration)
	}
	// Tags should be preserved.
	if len(tx.tags) != 1 || tx.tags[0] != "mytag" {
		t.Fatalf("expected tags preserved after reset, got %v", tx.tags)
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
