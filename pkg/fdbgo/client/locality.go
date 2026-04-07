package client

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand"
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
func (lc *locationCache) locate(db *database, ctx context.Context, key []byte, tenantId int64) (LocationResult, error) {
	// System key space (\xff\xff prefix) is handled specially in C++ client.
	// Don't send GetKeyServerLocationsRequest for it — clamp to normal key range.
	if len(key) >= 2 && key[0] == 0xff && key[1] == 0xff {
		// Use the last known storage server for system keys.
		// This matches C++ behavior where system keys are resolved internally.
		key = []byte{0xff}
	}

	// Check cache first. Entries are keyed by (tenantId, key range).
	lc.mu.RLock()
	for _, entry := range lc.entries {
		if entry.tenantId == tenantId &&
			bytes.Compare(key, entry.begin) >= 0 &&
			(entry.end == nil || bytes.Compare(key, entry.end) < 0) {
			result := LocationResult{
				Servers:    entry.servers,
				ShardBegin: entry.begin,
				ShardEnd:   entry.end,
			}
			lc.mu.RUnlock()
			return result, nil
		}
	}
	lc.mu.RUnlock()

	// Cache miss — query commit proxy.
	return lc.refresh(db, ctx, key, tenantId)
}

// invalidate removes cached entries containing the given key for the given tenant.
// Called on wrong_shard_server errors for point lookups.
func (lc *locationCache) invalidate(key []byte, tenantId int64) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	filtered := lc.entries[:0]
	for _, entry := range lc.entries {
		if entry.tenantId == tenantId &&
			bytes.Compare(key, entry.begin) >= 0 &&
			(entry.end == nil || bytes.Compare(key, entry.end) < 0) {
			continue // remove this entry
		}
		filtered = append(filtered, entry)
	}
	lc.entries = filtered
}

// invalidateRange removes all cached entries overlapping [begin, end) for the given tenant.
// C++ DatabaseContext::invalidateCache(KeyRangeRef) uses intersectingRanges to clear
// all stale entries after wrong_shard_server during range reads.
// Note: end must not be nil (callers always pass concrete byte slices).
func (lc *locationCache) invalidateRange(begin, end []byte, tenantId int64) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	filtered := lc.entries[:0]
	for _, entry := range lc.entries {
		// Entry overlaps [begin, end) if entry.begin < end && entry.end > begin.
		if entry.tenantId == tenantId &&
			bytes.Compare(entry.begin, end) < 0 &&
			(entry.end == nil || bytes.Compare(entry.end, begin) > 0) {
			continue // remove overlapping entry
		}
		filtered = append(filtered, entry)
	}
	lc.entries = filtered
}

// refresh queries commit proxies for the location of a key, matching C++
// basicLoadBalance with AtMostOnce::False. Cycles all proxies with backoff.
// Loops until success or ctx cancellation.
func (lc *locationCache) refresh(db *database, ctx context.Context, key []byte, tenantId int64) (LocationResult, error) {
	var backoff time.Duration

	for {
		proxies := db.getCommitProxies()
		if len(proxies) == 0 {
			db.kickTopology()
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			}
			select {
			case <-time.After(backoff):
				backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
				continue
			case <-ctx.Done():
				return LocationResult{}, ctx.Err()
			case <-db.ctx.Done():
				return LocationResult{}, db.ctx.Err()
			}
		}

		if backoff > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return LocationResult{}, ctx.Err()
			case <-db.ctx.Done():
				return LocationResult{}, db.ctx.Err()
			}
		}

		cycledAll := true
		for _, proxy := range proxies {
			conn, err := db.getOrDial(ctx, proxy.Address)
			if err != nil {
				db.handleConnError(proxy.Address)
				continue
			}

			replyToken, replyCh, cancelReply := conn.PrepareReply()
			body := buildGetKeyServerLocationsRequest(key, tenantId, replyToken)
			locToken := getAdjustedEndpoint(proxy.Token, EndpointGetKeyServerLocations)

			if err := conn.SendFrame(locToken, body); err != nil {
				cancelReply()
				db.handleConnError(proxy.Address)
				continue
			}

			rctx, rpcCancel := context.WithTimeout(ctx, DefaultRPCTimeout)
			select {
			case resp := <-replyCh:
				rpcCancel()
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
				lc.mu.Lock()
				lc.entries = append(lc.entries, entries...)
				for len(lc.entries) > lc.maxSize {
					idx := rand.Intn(len(lc.entries))
					lc.entries[idx] = lc.entries[len(lc.entries)-1]
					lc.entries = lc.entries[:len(lc.entries)-1]
				}
				lc.mu.Unlock()
				if len(entries) > 0 {
					return LocationResult{
						Servers:    entries[0].servers,
						ShardBegin: entries[0].begin,
						ShardEnd:   entries[0].end,
					}, nil
				}
			case <-rctx.Done():
				rpcCancel()
				cancelReply()
				if ctx.Err() != nil {
					return LocationResult{}, ctx.Err()
				}
				continue
			}
			cycledAll = false
		}

		// All proxies failed. Kick topology, grow backoff.
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

		// Scan cache for all entries overlapping [curBegin, end).
		lc.mu.RLock()
		for _, entry := range lc.entries {
			// Entry overlaps [curBegin, end) if entry.begin < end && entry.end > curBegin.
			if entry.tenantId == tenantId &&
				(entry.end == nil || bytes.Compare(entry.end, curBegin) > 0) &&
				bytes.Compare(entry.begin, end) < 0 {
				results = append(results, LocationResult{
					Servers:    entry.servers,
					ShardBegin: entry.begin,
					ShardEnd:   entry.end,
				})
			}
		}
		lc.mu.RUnlock()

		// Sort by begin key.
		sortLocationResults(results)

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

// sortLocationResults sorts by ShardBegin ascending.
func sortLocationResults(results []LocationResult) {
	// Simple insertion sort — typically very few entries (< 100 shards).
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && bytes.Compare(results[j].ShardBegin, results[j-1].ShardBegin) < 0; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}

// refreshRange queries commit proxies for locations overlapping [begin, end).
// Returns all location entries from the response.
func (lc *locationCache) refreshRange(db *database, ctx context.Context, begin, end []byte, limit int, reverse bool, tenantId int64) ([]locationEntry, error) {
	var backoff time.Duration

	for {
		proxies := db.getCommitProxies()
		if len(proxies) == 0 {
			db.kickTopology()
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			}
			select {
			case <-time.After(backoff):
				backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-db.ctx.Done():
				return nil, db.ctx.Err()
			}
		}

		if backoff > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-db.ctx.Done():
				return nil, db.ctx.Err()
			}
		}

		cycledAll := true
		for _, proxy := range proxies {
			conn, err := db.getOrDial(ctx, proxy.Address)
			if err != nil {
				db.handleConnError(proxy.Address)
				continue
			}

			replyToken, replyCh, cancelReply := conn.PrepareReply()
			body := buildGetKeyServerLocationsRangeRequest(begin, end, limit, reverse, tenantId, replyToken)
			locToken := getAdjustedEndpoint(proxy.Token, EndpointGetKeyServerLocations)

			if err := conn.SendFrame(locToken, body); err != nil {
				cancelReply()
				db.handleConnError(proxy.Address)
				continue
			}

			rctx, rpcCancel := context.WithTimeout(ctx, DefaultRPCTimeout)
			select {
			case resp := <-replyCh:
				rpcCancel()
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
				lc.mu.Lock()
				lc.entries = append(lc.entries, entries...)
				for len(lc.entries) > lc.maxSize {
					idx := rand.Intn(len(lc.entries))
					lc.entries[idx] = lc.entries[len(lc.entries)-1]
					lc.entries = lc.entries[:len(lc.entries)-1]
				}
				lc.mu.Unlock()
				if len(entries) > 0 {
					return entries, nil
				}
			case <-rctx.Done():
				rpcCancel()
				cancelReply()
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			cycledAll = false
		}

		// All proxies failed. Kick topology, grow backoff.
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

// buildGetKeyServerLocationsRequest constructs the request with embedded reply token.
// Single-key lookup: no End field set.
func buildGetKeyServerLocationsRequest(key []byte, tenantId int64, replyToken transport.UID) []byte {
	req := types.GetKeyServerLocationsRequest{
		Begin:            key,
		Limit:            100,
		Reply:            types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		Tenant:           types.TenantInfo{TenantId: tenantId},
		MinTenantVersion: -2, // C++ latestVersion = -2 (default for GetKeyServerLocationsRequest)
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
		MinTenantVersion: -2,
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
			ep, err := ReadEndpointFromSlot(ssR, 2)
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
