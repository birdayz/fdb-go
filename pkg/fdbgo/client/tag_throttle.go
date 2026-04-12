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
// Simplified vs C++ (no Smoother). Conservative: may over-throttle slightly
// but never under-throttle.
func (t *clientTagThrottleLimits) throttleDuration() time.Duration {
	remaining := time.Until(t.expiration)
	if remaining <= 0 {
		return 0
	}
	if t.tpsRate == 0 {
		return remaining // throttled indefinitely until expiry
	}
	// C++ uses Smoother-based capacity calculation. We simplify:
	// use remaining time as duration. Conservative.
	return remaining
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
	result := make(map[string]clientTagThrottleLimits, count)
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

// update merges parsed tag throttle info for a given priority.
// For each transaction tag: if the tag is present in info, update its limits;
// if absent, remove its entry (throttle expired/cleared by server).
func (s *tagThrottleState) update(priority TransactionPriority, txTags []string, info map[string]clientTagThrottleLimits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tags == nil {
		s.tags = make(map[TransactionPriority]map[string]*clientTagThrottleLimits)
	}
	priorityMap, ok := s.tags[priority]
	if !ok {
		priorityMap = make(map[string]*clientTagThrottleLimits)
		s.tags[priority] = priorityMap
	}
	for _, tag := range txTags {
		if limits, found := info[tag]; found {
			copied := limits
			priorityMap[tag] = &copied
		} else {
			delete(priorityMap, tag)
		}
	}
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
