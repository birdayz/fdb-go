package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// locationCache maps key ranges to storage server endpoints.
// C++: CoalescedKeyRangeMap<Reference<LocationInfo>>.
//
// Methods receive *database as argument — no stored back-pointer.
// Size-capped to maxSize entries (C++ default: LOCATION_CACHE_EVICTION_SIZE = 600,000).
// Random eviction on overflow, matching C++ setCachedLocation behavior.
//
// Entries are kept sorted by (tenantId, begin) for O(log N) lookups via
// binary search. Most deployments use a single tenant (tenantId=-1), so
// entries within that tenant form a contiguous sorted block.
type locationCache struct {
	mu      sync.RWMutex
	entries []locationEntry
	maxSize int // default 600_000
}

type locationEntry struct {
	tenantId int64
	begin    []byte
	end      []byte
	servers  []ServerInfo
}

// entryLess returns true if a sorts before b by (tenantId, begin).
func entryLess(a, b *locationEntry) bool {
	if a.tenantId != b.tenantId {
		return a.tenantId < b.tenantId
	}
	return bytes.Compare(a.begin, b.begin) < 0
}

// searchIndex returns the index of the first entry where
// (entry.tenantId, entry.begin) >= (tenantId, key) using binary search.
func (lc *locationCache) searchIndex(tenantId int64, key []byte) int {
	return sort.Search(len(lc.entries), func(i int) bool {
		e := &lc.entries[i]
		if e.tenantId != tenantId {
			return e.tenantId > tenantId
		}
		return bytes.Compare(e.begin, key) >= 0
	})
}

// insertSorted inserts entries in sorted order, replacing any existing entry
// with the same (tenantId, begin). Caller must hold lc.mu write lock.
func (lc *locationCache) insertSorted(newEntries []locationEntry) {
	for _, ne := range newEntries {
		idx := lc.searchIndex(ne.tenantId, ne.begin)
		// Check for duplicate: same (tenantId, begin) at idx.
		if idx < len(lc.entries) &&
			lc.entries[idx].tenantId == ne.tenantId &&
			bytes.Equal(lc.entries[idx].begin, ne.begin) {
			// Replace in-place.
			lc.entries[idx] = ne
			continue
		}
		// Insert at idx.
		lc.entries = append(lc.entries, locationEntry{})
		copy(lc.entries[idx+1:], lc.entries[idx:])
		lc.entries[idx] = ne
	}
}

// ServerInfo holds a storage server's address and endpoint token.
type ServerInfo struct {
	Address string
	Token   transport.UID
}

// LocationResult holds the storage servers and shard key range for a locate() result.
// C++ KeyRangeLocationInfo equivalent.
type LocationResult struct {
	Servers    []ServerInfo
	ShardBegin []byte
	ShardEnd   []byte
}

// locate finds the storage servers responsible for a key.
// On cache miss, queries a commit proxy.
// Returns the servers AND the shard boundaries so callers can clamp requests.
// O(log N) via binary search on the sorted entries.
func (lc *locationCache) locate(db *database, ctx context.Context, key []byte, tenantId int64) (LocationResult, error) {
	// System key space (\xff\xff prefix) is handled specially in C++ client.
	// Don't send GetKeyServerLocationsRequest for it — clamp to normal key range.
	if len(key) >= 2 && key[0] == 0xff && key[1] == 0xff {
		// Use the last known storage server for system keys.
		// This matches C++ behavior where system keys are resolved internally.
		key = []byte{0xff}
	}

	// Check cache first. Binary search for the entry where begin <= key.
	lc.mu.RLock()
	if result, ok := lc.lookupLocked(tenantId, key); ok {
		lc.mu.RUnlock()
		return result, nil
	}
	lc.mu.RUnlock()

	// Cache miss — query commit proxy.
	return lc.refresh(db, ctx, key, tenantId)
}

// lookupLocked finds the entry containing key for the given tenant.
// Caller must hold at least lc.mu.RLock(). O(log N).
func (lc *locationCache) lookupLocked(tenantId int64, key []byte) (LocationResult, bool) {
	// searchIndex returns the first entry with begin >= key for this tenant.
	// The containing entry (begin <= key) is at idx-1, unless idx itself
	// has begin == key (exact match).
	idx := lc.searchIndex(tenantId, key)

	// Check idx (exact match: entry.begin == key).
	if idx < len(lc.entries) {
		e := &lc.entries[idx]
		if e.tenantId == tenantId && bytes.Equal(e.begin, key) {
			if e.end == nil || bytes.Compare(key, e.end) < 0 {
				return LocationResult{
					Servers:    e.servers,
					ShardBegin: e.begin,
					ShardEnd:   e.end,
				}, true
			}
		}
	}

	// Check idx-1 (entry.begin < key, might contain key if key < entry.end).
	if idx > 0 {
		e := &lc.entries[idx-1]
		if e.tenantId == tenantId &&
			bytes.Compare(key, e.begin) >= 0 &&
			(e.end == nil || bytes.Compare(key, e.end) < 0) {
			return LocationResult{
				Servers:    e.servers,
				ShardBegin: e.begin,
				ShardEnd:   e.end,
			}, true
		}
	}

	return LocationResult{}, false
}

// invalidate removes the cached entry containing the given key for the given tenant.
// Called on wrong_shard_server errors for point lookups. O(log N).
func (lc *locationCache) invalidate(key []byte, tenantId int64) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	// Binary search: find the entry that might contain key.
	idx := lc.searchIndex(tenantId, key)

	// Check exact match at idx.
	if idx < len(lc.entries) {
		e := &lc.entries[idx]
		if e.tenantId == tenantId && bytes.Equal(e.begin, key) {
			if e.end == nil || bytes.Compare(key, e.end) < 0 {
				lc.entries = append(lc.entries[:idx], lc.entries[idx+1:]...)
				return
			}
		}
	}

	// Check idx-1 (entry.begin < key, key < entry.end).
	if idx > 0 {
		e := &lc.entries[idx-1]
		if e.tenantId == tenantId &&
			bytes.Compare(key, e.begin) >= 0 &&
			(e.end == nil || bytes.Compare(key, e.end) < 0) {
			lc.entries = append(lc.entries[:idx-1], lc.entries[idx:]...)
			return
		}
	}
}

// invalidateRange removes all cached entries overlapping [begin, end) for the given tenant.
// C++ DatabaseContext::invalidateCache(KeyRangeRef) uses intersectingRanges to clear
// all stale entries after wrong_shard_server during range reads.
// Note: end must not be nil (callers always pass concrete byte slices).
// O(log N + K) where K is the number of overlapping entries.
func (lc *locationCache) invalidateRange(begin, end []byte, tenantId int64) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	if len(lc.entries) == 0 {
		return
	}

	// Find the first entry that could overlap [begin, end).
	// An entry overlaps if entry.begin < end AND entry.end > begin.
	// The earliest possible overlapping entry has begin < end, so we
	// search for the first entry with begin >= end and start checking
	// backwards from there. But we also need entries whose begin < end
	// but end > begin. The simplest correct approach:
	// - Binary search for first entry with (tenantId, begin) >= (tenantId, begin).
	//   Back up one to catch an entry whose begin < begin but end > begin.
	// - Scan forward collecting entries to remove while entry.begin < end.
	startIdx := lc.searchIndex(tenantId, begin)
	// Back up one: the entry at startIdx-1 might have begin < begin but end > begin.
	if startIdx > 0 {
		prev := &lc.entries[startIdx-1]
		if prev.tenantId == tenantId {
			startIdx--
		}
	}

	// Collect indices to remove (scan forward while in tenant and begin < end).
	var toRemove int
	for i := startIdx; i < len(lc.entries); i++ {
		e := &lc.entries[i]
		if e.tenantId != tenantId {
			if e.tenantId > tenantId {
				break // past this tenant's entries
			}
			continue // before this tenant (shouldn't happen given startIdx, but safe)
		}
		if bytes.Compare(e.begin, end) >= 0 {
			break // past the range
		}
		// entry.begin < end. Check entry.end > begin.
		if e.end == nil || bytes.Compare(e.end, begin) > 0 {
			toRemove++
		}
	}

	if toRemove == 0 {
		return
	}

	// Remove overlapping entries in-place (shift left).
	dst := startIdx
	for i := startIdx; i < len(lc.entries); i++ {
		e := &lc.entries[i]
		remove := false
		if e.tenantId == tenantId &&
			bytes.Compare(e.begin, end) < 0 &&
			(e.end == nil || bytes.Compare(e.end, begin) > 0) {
			remove = true
		}
		if !remove {
			if dst != i {
				lc.entries[dst] = lc.entries[i]
			}
			dst++
		}
	}
	lc.entries = lc.entries[:dst]
}

// refresh queries commit proxies for the location of a single key.
func (lc *locationCache) refresh(db *database, ctx context.Context, key []byte, tenantId int64) (LocationResult, error) {
	entries, err := lc.queryLocations(db, ctx, tenantId, func(replyToken transport.UID) []byte {
		return buildGetKeyServerLocationsRequest(key, tenantId, replyToken)
	})
	if err != nil {
		return LocationResult{}, err
	}
	return LocationResult{
		Servers:    entries[0].servers,
		ShardBegin: entries[0].begin,
		ShardEnd:   entries[0].end,
	}, nil
}

// evictIfNeeded removes random entries when the cache exceeds maxSize.
// Caller must hold lc.mu write lock. After eviction the slice is re-sorted
// because swap-with-last during random eviction breaks sort order.
func (lc *locationCache) evictIfNeeded() {
	if len(lc.entries) <= lc.maxSize {
		return
	}
	for len(lc.entries) > lc.maxSize {
		idx := rand.Intn(len(lc.entries))
		lc.entries[idx] = lc.entries[len(lc.entries)-1]
		lc.entries = lc.entries[:len(lc.entries)-1]
	}
	// Re-sort after random eviction broke ordering.
	sort.Slice(lc.entries, func(i, j int) bool {
		return entryLess(&lc.entries[i], &lc.entries[j])
	})
}

// locateRange returns all cached location entries overlapping [begin, end).
// On cache miss for any sub-range, queries a commit proxy for the missing range.
// C++ getKeyRangeLocations equivalent. The reverse parameter is forwarded to the
// commit proxy so it returns shards in the right order for the scan direction.
func (lc *locationCache) locateRange(db *database, ctx context.Context, begin, end []byte, limit int, reverse bool, tenantId int64) ([]LocationResult, error) {
	// System key space (\xff\xff prefix) is handled like locate().
	if len(begin) >= 2 && begin[0] == 0xff && begin[1] == 0xff {
		begin = []byte{0xff}
	}
	if len(end) >= 2 && end[0] == 0xff && end[1] == 0xff {
		end = []byte{0xff, 0x00} // just past \xff
	}

	curBegin := begin
	for {
		var results []LocationResult

		// Binary search for overlapping entries. O(log N + K).
		lc.mu.RLock()
		results = lc.collectOverlapping(tenantId, curBegin, end)
		lc.mu.RUnlock()

		// Results are already sorted (entries are sorted, we scan forward).

		// Check for gaps in [curBegin, end).
		gapBegin := curBegin
		hasGap := false
		for _, r := range results {
			if bytes.Compare(r.ShardBegin, gapBegin) > 0 {
				// There's a gap: [gapBegin, r.ShardBegin) is uncached.
				hasGap = true
				break
			}
			if r.ShardEnd == nil {
				gapBegin = end // shard covers to infinity, no more gaps
				break
			} else if bytes.Compare(r.ShardEnd, gapBegin) > 0 {
				gapBegin = r.ShardEnd
			}
		}
		if !hasGap && bytes.Compare(gapBegin, end) < 0 {
			// Gap at the tail: [gapBegin, end) is uncached.
			hasGap = true
		}

		if !hasGap {
			// Return in scan order: C++ getKeyRangeLocations returns shards
			// end→begin for reverse scans so locations[0] is nearest end.
			// Our cache always sorts ascending, so reverse first, then clamp.
			// Reversing before limit ensures we keep the shards nearest the
			// scan start (nearest end for reverse, nearest begin for forward).
			if reverse {
				for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
					results[i], results[j] = results[j], results[i]
				}
			}
			if limit > 0 && len(results) > limit {
				results = results[:limit]
			}
			return results, nil
		}

		// Cache miss — refresh the missing sub-range.
		_, err := lc.refreshRange(db, ctx, gapBegin, end, limit, reverse, tenantId)
		if err != nil {
			return nil, err
		}
		// Loop back to re-scan cache with the new entries.
	}
}

// stripTenantPrefix removes the 8-byte tenant prefix from shard boundaries.
// The FDB proxy returns absolute shard ranges (with tenant prefix prepended).
// Our cache and lookup use tenant-relative keys, so we strip the prefix.
// C++ caches absolute and looks up absolute; we cache relative and look up relative.
func stripTenantPrefix(entries []locationEntry, tenantId int64) {
	if tenantId < 0 {
		return // no tenant, boundaries are already in normal key space
	}
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(tenantId))
	for i := range entries {
		if bytes.HasPrefix(entries[i].begin, prefix[:]) {
			entries[i].begin = entries[i].begin[8:]
		}
		if entries[i].end != nil && bytes.HasPrefix(entries[i].end, prefix[:]) {
			entries[i].end = entries[i].end[8:]
		}
	}
}

// collectOverlapping returns all entries overlapping [begin, end) for the given
// tenant, in sorted order. Caller must hold at least lc.mu.RLock(). O(log N + K).
func (lc *locationCache) collectOverlapping(tenantId int64, begin, end []byte) []LocationResult {
	if len(lc.entries) == 0 {
		return nil
	}

	// Find starting position: first entry with (tenantId, begin) >= (tenantId, begin).
	// Back up one to catch entry whose begin < begin but end > begin.
	startIdx := lc.searchIndex(tenantId, begin)
	if startIdx > 0 {
		prev := &lc.entries[startIdx-1]
		if prev.tenantId == tenantId {
			startIdx--
		}
	}

	var results []LocationResult
	for i := startIdx; i < len(lc.entries); i++ {
		e := &lc.entries[i]
		if e.tenantId != tenantId {
			if e.tenantId > tenantId {
				break
			}
			continue
		}
		if bytes.Compare(e.begin, end) >= 0 {
			break // past the range
		}
		// entry.begin < end. Check entry.end > begin.
		if e.end == nil || bytes.Compare(e.end, begin) > 0 {
			results = append(results, LocationResult{
				Servers:    e.servers,
				ShardBegin: e.begin,
				ShardEnd:   e.end,
			})
		}
	}
	return results
}

// queryLocations is the shared load-balance loop for location queries.
// Cycles all commit proxies with exponential backoff until success or ctx
// cancellation. The buildRequest callback constructs the request body given
// a reply token. Used by both refresh (single key) and refreshRange (range).
func (lc *locationCache) queryLocations(db *database, ctx context.Context, tenantId int64, buildRequest func(replyToken transport.UID) []byte) ([]locationEntry, error) {
	var backoff time.Duration

	for {
		proxies := db.getCommitProxies()
		if len(proxies) == 0 {
			db.kickTopology()
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			}
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
				backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
				continue
			case <-db.waitProxiesChanged():
				timer.Stop()
				backoff = 0
				continue
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-db.ctx.Done():
				timer.Stop()
				return nil, db.ctx.Err()
			}
		}

		if backoff > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-db.waitProxiesChanged():
				timer.Stop()
				backoff = 0
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-db.ctx.Done():
				timer.Stop()
				return nil, db.ctx.Err()
			}
		}

		cycledAll := true
		for _, proxy := range proxies {
			conn, err := db.getOrDial(ctx, proxy.Address)
			if err != nil {
				db.handleDialError(ctx, proxy.Address)
				continue
			}

			replyToken, replyCh, replyHandle := conn.PrepareReply()
			body := buildRequest(replyToken)
			locToken := getAdjustedEndpoint(proxy.Token, EndpointGetKeyServerLocations)

			if err := conn.SendFrame(locToken, body); err != nil {
				replyHandle.Cancel()
				replyHandle.Release()
				db.handleConnError(proxy.Address)
				continue
			}

			rctx, rpcCancel := context.WithTimeout(ctx, DefaultRPCTimeout)
			select {
			case resp := <-replyCh:
				rpcCancel()
				replyHandle.Release()
				if resp.Err != nil {
					db.handleConnError(proxy.Address)
					continue
				}
				entries, err := parseGetKeyServerLocationsReply(resp.Body)
				if err != nil {
					continue
				}
				for i := range entries {
					entries[i].tenantId = tenantId
				}
				stripTenantPrefix(entries, tenantId)
				lc.mu.Lock()
				lc.insertSorted(entries)
				lc.evictIfNeeded()
				lc.mu.Unlock()
				if len(entries) > 0 {
					return entries, nil
				}
			case <-rctx.Done():
				rpcCancel()
				replyHandle.Cancel()
				replyHandle.Release()
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			cycledAll = false
		}

		db.kickTopology()
		if cycledAll {
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			} else {
				backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
			}
		}
	}
}

func (lc *locationCache) refreshRange(db *database, ctx context.Context, begin, end []byte, limit int, reverse bool, tenantId int64) ([]locationEntry, error) {
	return lc.queryLocations(db, ctx, tenantId, func(replyToken transport.UID) []byte {
		return buildGetKeyServerLocationsRangeRequest(begin, end, limit, reverse, tenantId, replyToken)
	})
}

// buildGetKeyServerLocationsRequest constructs the request with embedded reply token.
// Single-key lookup: no End field set.
func buildGetKeyServerLocationsRequest(key []byte, tenantId int64, replyToken transport.UID) []byte {
	req := types.GetKeyServerLocationsRequest{
		Begin:            key,
		Limit:            100,
		Reply:            types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		Tenant:           types.TenantInfo{TenantId: tenantId},
		MinTenantVersion: LatestVersion,
	}
	return req.MarshalFDB()
}

// buildGetKeyServerLocationsRangeRequest constructs the request with Begin, End, Limit, and Reverse.
// C++ getKeyRangeLocations sends both begin and end to get all overlapping shards.
// The reverse flag tells the commit proxy to return shards from end→begin order,
// matching C++ NativeAPI.actor.cpp:2241.
func buildGetKeyServerLocationsRangeRequest(begin, end []byte, limit int, reverse bool, tenantId int64, replyToken transport.UID) []byte {
	req := types.GetKeyServerLocationsRequest{
		Begin:            begin,
		HasEnd:           true,
		End:              end,
		Limit:            int32(limit),
		Reverse:          reverse,
		Reply:            types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		Tenant:           types.TenantInfo{TenantId: tenantId},
		MinTenantVersion: LatestVersion,
	}
	return req.MarshalFDB()
}

func parseGetKeyServerLocationsReply(data []byte) ([]locationEntry, error) {
	r, err := wire.ReadErrorOr(data)
	if err != nil {
		return nil, fmt.Errorf("locations reply: %w", err)
	}

	// Parse vector of pair<KeyRangeRef, vector<StorageServerInterface>> using
	// generated slot constants and types.
	pairCount, err := r.ReadVectorCount(types.GetKeyServerLocationsReplySlotResults)
	if err != nil || pairCount == 0 {
		return nil, fmt.Errorf("no location results")
	}

	entries := make([]locationEntry, 0, pairCount)
	for i := 0; i < pairCount; i++ {
		pairR, err := r.ReadVectorElementReader(types.GetKeyServerLocationsReplySlotResults, i)
		if err != nil {
			continue
		}

		// Pair slot 0: KeyRangeRef (nested struct).
		var kr types.KeyRangeRef
		if krR, err := pairR.ReadNestedReader(types.LocationPairSlotKeyRange); err == nil {
			kr.UnmarshalFromReader(krR)
		}

		// Pair slot 1: vector<StorageServerInterface>.
		var servers []ServerInfo
		ssCount, err := pairR.ReadVectorCount(types.LocationPairSlotServers)
		if err != nil || ssCount == 0 {
			continue
		}
		for j := 0; j < ssCount; j++ {
			ssR, err := pairR.ReadVectorElementReader(types.LocationPairSlotServers, j)
			if err != nil {
				continue
			}
			// Slot 2 = getKeyValues RequestStream in StorageServerInterface.
			// We read any endpoint to get the base token; slot 2 is the most
			// reliably present in all FDB versions.
			ep, err := ReadEndpointFromSlot(ssR, types.StorageServerInterfaceSlotField_2)
			if err != nil || !endpointValid(&ep) {
				nf := ssR.VTableLength() - 2
				for s := 0; s < nf; s++ {
					ep, err = ReadEndpointFromSlot(ssR, s)
					if err == nil && endpointValid(&ep) {
						break
					}
				}
			}
			if endpointValid(&ep) {
				servers = append(servers, ServerInfo{
					Address: endpointAddress(&ep),
					Token:   endpointToken(&ep),
				})
			}
		}
		if len(servers) > 0 {
			entries = append(entries, locationEntry{
				begin:   kr.Begin,
				end:     kr.End,
				servers: servers,
			})
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no storage servers in locations reply")
	}
	return entries, nil
}
