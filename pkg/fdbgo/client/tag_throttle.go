package client

import (
	"encoding/binary"
	"math"
	"sync"
	"time"
)

// Tag throttle knobs — matching C++ CLIENT_KNOBS defaults from NativeAPI.actor.cpp.
const (
	// tagThrottleRecheckInterval matches C++ CLIENT_KNOBS->TAG_THROTTLE_RECHECK_INTERVAL.
	tagThrottleRecheckInterval = 7 * time.Second

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

// parseTagThrottleInfo deserializes the TagThrottleInfo bytes from a GRV reply.
// Wire format: FDB standard serialization of unordered_map<StringRef, ClientTagThrottleLimits>:
//   - uint32 count (LE)
//   - For each entry: uint32 tagLen (LE) + tagLen bytes (tag) + float64 tpsRate (LE) + float64 duration (LE)
func parseTagThrottleInfo(data []byte) map[string]clientTagThrottleLimits {
	if len(data) < 4 {
		return nil
	}
	count := binary.LittleEndian.Uint32(data[:4])
	if count == 0 {
		return nil
	}
	off := 4
	// Don't pass `count` as the make() hint: count is wire-controlled, and a
	// hostile or corrupt server can set it to ~4B → make() would reserve tens
	// of GB and freeze the host. C++ uses unordered_map::insert without any
	// pre-reserve (NativeAPI.actor.cpp), and the server hard-caps the set at
	// SERVER_KNOBS->GLOBAL_TAG_THROTTLING_MAX_TAGS_TRACKED = 10 entries. We
	// match: let the map grow naturally; the real safety bound is len(data)
	// via the per-entry length checks below.
	result := make(map[string]clientTagThrottleLimits)
	now := time.Now()
	for i := uint32(0); i < count; i++ {
		if off+4 > len(data) {
			break
		}
		tagLen := binary.LittleEndian.Uint32(data[off : off+4])
		off += 4
		if off+int(tagLen) > len(data) {
			break
		}
		tag := string(data[off : off+int(tagLen)])
		off += int(tagLen)
		if off+16 > len(data) {
			break
		}
		tpsRate := math.Float64frombits(binary.LittleEndian.Uint64(data[off : off+8]))
		off += 8
		duration := math.Float64frombits(binary.LittleEndian.Uint64(data[off : off+8]))
		off += 8
		result[tag] = clientTagThrottleLimits{
			tpsRate:    tpsRate,
			expiration: now.Add(time.Duration(duration * float64(time.Second))),
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
