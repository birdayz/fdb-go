package client

import (
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// Tag throttle knobs — matching C++ CLIENT_KNOBS defaults from NativeAPI.actor.cpp.
const (
	// tagThrottleRecheckInterval matches C++ CLIENT_KNOBS->TAG_THROTTLE_RECHECK_INTERVAL = 5.0
	// (ClientKnobs.cpp:296). It caps the tag-throttle retry backoff (getBackoff,
	// NativeAPI.actor.cpp:6100: min(TAG_THROTTLE_RECHECK_INTERVAL, throttleDuration)). Was erroneously 7s.
	tagThrottleRecheckInterval = 5 * time.Second

	// proxyMaxTagThrottleDuration matches C++ CLIENT_KNOBS->PROXY_MAX_TAG_THROTTLE_DURATION.
	proxyMaxTagThrottleDuration = 5 * time.Second
)

// clientTagThrottleLimits stores per-tag throttle info from the GRV reply.
// Matches C++ struct ClientTagThrottleLimits in fdbclient/TagThrottle.h.
type clientTagThrottleLimits struct {
	tpsRate    float64
	expiration time.Time // now() + duration from wire
}

// throttleDuration returns how long to wait for this tag's throttle.
// Matches C++ TransactionTag throttle: wait 1/tpsRate seconds (one TPS slot),
// capped by the remaining time until expiry.
func (t *clientTagThrottleLimits) throttleDuration() time.Duration {
	remaining := time.Until(t.expiration)
	if remaining <= 0 {
		return 0
	}
	if t.tpsRate == 0 {
		return remaining // throttled indefinitely until expiry
	}
	// Wait one TPS slot: the time for one transaction at the allowed rate.
	// At 100 TPS → 10ms, at 1 TPS → 1s, at 0.1 TPS → 10s (capped by remaining).
	delay := time.Duration(float64(time.Second) / t.tpsRate)
	if delay > remaining {
		return remaining
	}
	return delay
}

// parseTagThrottleInfo converts the GRV reply's tagThrottleInfo entries into the
// client-side throttle map.
//
// The field is TransactionTagMap<ClientTagThrottleLimits>
// (CommitProxyInterface.h:272) — std::unordered_map, which flat_buffers.h gives
// vector_like_traits with value_type std::pair<Key,T>. On the wire it is
// therefore a vector of two-slot objects, NOT a length-prefixed byte blob, and
// the wire layer decodes it as such. A hand-rolled byte parser here previously
// read the vector's leading element COUNT as if it were a byte LENGTH; that only
// ever agreed with C++ for the empty map, where both encodings degenerate to a
// bare four-byte zero.
//
// ClientTagThrottleLimits::serialize sends the expiration as a relative duration
// (expiration - now(), TagThrottle.actor.h:215-227) so client and proxy need no
// clock agreement; we re-anchor it against the local clock on arrival.
func parseTagThrottleInfo(entries []types.TransactionTagThrottle) map[string]clientTagThrottleLimits {
	if len(entries) == 0 {
		return nil
	}
	result := make(map[string]clientTagThrottleLimits, len(entries))
	now := time.Now()
	for _, e := range entries {
		result[string(e.Tag)] = clientTagThrottleLimits{
			tpsRate:    e.Limits.TpsRate,
			expiration: now.Add(time.Duration(e.Limits.Duration * float64(time.Second))),
		}
	}
	return result
}

// tagThrottleState holds the per-database tag throttle tracking.
// Maps priority -> (tag -> throttle limits). Matches C++ cx->throttledTags.
type tagThrottleState struct {
	mu   sync.RWMutex
	tags map[TransactionPriority]map[string]*clientTagThrottleLimits
}

// replace sets the tag throttle info for a given priority, replacing all
// previous entries. Tags the server no longer reports are automatically
// removed (the entire map is replaced, not merged).
func (s *tagThrottleState) replace(priority TransactionPriority, info map[string]clientTagThrottleLimits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tags == nil {
		s.tags = make(map[TransactionPriority]map[string]*clientTagThrottleLimits)
	}
	priorityMap := make(map[string]*clientTagThrottleLimits, len(info))
	for tag, limits := range info {
		copied := limits
		priorityMap[tag] = &copied
	}
	s.tags[priority] = priorityMap
}

// maxDuration returns the maximum throttle duration across all given tags
// at the specified priority. Returns 0 if no tags are throttled.
func (s *tagThrottleState) maxDuration(priority TransactionPriority, tags []string) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	priorityMap := s.tags[priority]
	if priorityMap == nil {
		return 0
	}
	var maxDur time.Duration
	for _, tag := range tags {
		if data, found := priorityMap[tag]; found {
			d := data.throttleDuration()
			if d > maxDur {
				maxDur = d
			}
		}
	}
	return maxDur
}
