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

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
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
	// TLS records whether the SS address carried the TLS flag. Address itself stays a clean
	// host:port for dialing; GetAddressesForKey appends ":tls" to its output when this is set,
	// matching C++ NetworkAddress::toString (flow/network.cpp:215).
	TLS bool
}

// LocationResult holds the storage servers and shard key range for a locate() result.
// C++ KeyRangeLocationInfo equivalent.
type LocationResult struct {
	Servers    []ServerInfo
	ShardBegin []byte
	ShardEnd   []byte
}

// locate finds the storage servers responsible for a key (O(log N) binary search on the sorted entries;
// on cache miss it queries a commit proxy). Returns the servers AND the shard boundaries so callers can
// clamp requests. isBackward selects the shard for a BACKWARD key selector (offset<=0 && !orEqual, C++
// FDBTypes.h:659-661): on a shard boundary it resolves to the shard ENDING at key (holding keyBefore(key)),
// matching C++ getKey's Reverse{k.isBackward()} threading (NativeAPI.actor.cpp:3786-3788 →
// getCachedLocation:1944-1955). Forward callers pass false.
func (lc *locationCache) locate(db *database, ctx context.Context, key []byte, tenantId int64, span types.SpanContext, isBackward bool) (LocationResult, error) {
	// System key space (\xff\xff prefix) is handled specially in C++ client.
	// Don't send GetKeyServerLocationsRequest for it — clamp to normal key range.
	if len(key) >= 2 && key[0] == 0xff && key[1] == 0xff {
		// Use the last known storage server for system keys.
		// This matches C++ behavior where system keys are resolved internally.
		key = []byte{0xff}
		// The clamp collapses the whole \xff\xff system keyspace to the single 0xff sentinel, so a
		// BACKWARD selector's keyBefore precision is already gone — force a FORWARD lookup of the sentinel
		// (the pre-#10 routing). Applying a backward lookup here would find the last USER shard ending at
		// 0xff instead of the system shard containing 0xff, wrong-shard/retry-ing on a multi-SS cluster
		// where user and system shards differ. System-key routing is C++-internal anyway.
		isBackward = false
	}

	// Check cache first. Binary search for the entry where begin <= key.
	lc.mu.RLock()
	if result, ok := lc.lookupLocked(tenantId, key, isBackward); ok {
		lc.mu.RUnlock()
		return result, nil
	}
	lc.mu.RUnlock()

	// Cache miss — query commit proxy.
	return lc.refresh(db, ctx, key, tenantId, span, isBackward)
}

// lookupLocked finds the entry containing key for the given tenant.
// Caller must hold at least lc.mu.RLock(). O(log N).
func (lc *locationCache) lookupLocked(tenantId int64, key []byte, isBackward bool) (LocationResult, bool) {
	// searchIndex returns the first entry with begin >= key for this tenant.
	// The containing entry (begin <= key) is at idx-1, unless idx itself
	// has begin == key (exact match).
	idx := lc.searchIndex(tenantId, key)

	if isBackward {
		// Backward selector (offset<=0 && !orEqual): resolve keyBefore(key) = the largest key' < key. It
		// lives in the shard [begin, end) with begin < key && key <= end — on a shard boundary (key == a
		// shard's begin) that is the PREVIOUS shard, whose end == key, NOT the shard beginning at key.
		// searchIndex returns the first entry with begin >= key, so that previous shard is at idx-1.
		// Mirrors C++ getCachedLocation's rangeContainingKeyBefore (NativeAPI.actor.cpp:1944-1955).
		if idx > 0 {
			e := &lc.entries[idx-1]
			if e.tenantId == tenantId && bytes.Compare(key, e.begin) > 0 &&
				(e.end == nil || bytes.Compare(key, e.end) <= 0) {
				return LocationResult{Servers: e.servers, ShardBegin: e.begin, ShardEnd: e.end}, true
			}
		}
		return LocationResult{}, false
	}

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
func (lc *locationCache) invalidate(key []byte, tenantId int64, isBackward bool) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	// Binary search: find the entry that might contain key.
	idx := lc.searchIndex(tenantId, key)

	if isBackward {
		// Invalidate the shard a BACKWARD selector resolved to (whose end == key on a shard boundary; see
		// lookupLocked's backward branch). It is at idx-1 with begin < key && key <= end.
		if idx > 0 {
			e := &lc.entries[idx-1]
			if e.tenantId == tenantId && bytes.Compare(key, e.begin) > 0 &&
				(e.end == nil || bytes.Compare(key, e.end) <= 0) {
				lc.entries = append(lc.entries[:idx-1], lc.entries[idx:]...)
			}
		}
		return
	}

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

// refresh queries commit proxies for the location of a single key. `span` is the
// issuing transaction's span; the getKeyLocation CHILD is derived once here and
// reused across the queryLocations proxy retries — matching C++ getKeyLocation_internal
// (NativeAPI.actor.cpp:3017 `Span("NAPI:getKeyLocation", spanContext)`, built once
// before basicLoadBalance).
func (lc *locationCache) refresh(db *database, ctx context.Context, key []byte, tenantId int64, span types.SpanContext, isBackward bool) (LocationResult, error) {
	child := childSpanContext(span)
	entries, err := lc.queryLocations(db, ctx, tenantId, func(replyToken transport.UID) []byte {
		return buildGetKeyServerLocationsRequest(key, isBackward, tenantId, child, replyToken)
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

// locateRange returns up to limit shards overlapping [begin, end) in scan order: nearest
// begin first, or nearest end first when reverse; limit <= 0 asks for all of them, as C++
// callers pass TOO_MANY. As C++ getKeyRangeLocations (NativeAPI.actor.cpp:3244-3262), it
// answers from the cache only when the cache holds those shards contiguously from the scan
// origin (getCachedLocations :1963-1999), and otherwise asks a commit proxy for them
// (:3194-3221).
func (lc *locationCache) locateRange(db *database, ctx context.Context, begin, end []byte, limit int, reverse bool, tenantId int64, span types.SpanContext) ([]LocationResult, error) {
	// System key space (\xff\xff prefix) is handled like locate().
	if len(begin) >= 2 && begin[0] == 0xff && begin[1] == 0xff {
		begin = []byte{0xff}
	}
	if len(end) >= 2 && end[0] == 0xff && end[1] == 0xff {
		end = []byte{0xff, 0x00} // just past \xff
	}
	if bytes.Compare(begin, end) >= 0 {
		return nil, nil
	}
	if limit <= 0 || limit > math.MaxInt32 {
		limit = math.MaxInt32
	}
	return lc.locateRangeWith(ctx, begin, end, limit, reverse, tenantId, func() ([]locationEntry, error) {
		return lc.refreshRange(db, ctx, begin, end, limit, reverse, tenantId, span)
	})
}

// locateRangeWith is locateRange after argument normalisation, with the proxy request
// supplied by the caller.
func (lc *locationCache) locateRangeWith(ctx context.Context, begin, end []byte, limit int, reverse bool, tenantId int64, refresh func() ([]locationEntry, error)) ([]LocationResult, error) {
	for {
		lc.mu.RLock()
		results, ok := lc.cachedLocationsLocked(tenantId, begin, end, limit, reverse)
		lc.mu.RUnlock()
		if ok {
			return results, nil
		}
		entries, err := refresh()
		if err != nil {
			return nil, err
		}
		// The reply parser drops shards without storage servers. A reply with a hole
		// would make callers skip its keys, so it is asked for again instead.
		if results := contiguousLocations(entries, begin, end, limit, reverse); results != nil {
			return results, nil
		}
		if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
			return nil, err
		}
	}
}

// contiguousLocations converts a proxy reply covering the range from the scan origin
// without a gap, or returns nil. The proxy stops only at limit or at the range edge
// (CommitProxyServer.actor.cpp:2713-2741), so a shorter reply ending earlier lost a shard.
func contiguousLocations(entries []locationEntry, begin, end []byte, limit int, reverse bool) []LocationResult {
	results := make([]LocationResult, 0, len(entries))
	for i, e := range entries {
		switch {
		case i == 0 && !reverse:
			if bytes.Compare(e.begin, begin) > 0 || (e.end != nil && bytes.Compare(e.end, begin) <= 0) {
				return nil
			}
		case i == 0:
			if bytes.Compare(e.begin, end) >= 0 || (e.end != nil && bytes.Compare(e.end, end) < 0) {
				return nil
			}
		case !reverse:
			if !bytes.Equal(e.begin, entries[i-1].end) {
				return nil
			}
		default:
			if !bytes.Equal(e.end, entries[i-1].begin) {
				return nil
			}
		}
		results = append(results, LocationResult{Servers: e.servers, ShardBegin: e.begin, ShardEnd: e.end})
	}
	if len(results) == 0 {
		return nil
	}
	if last := results[len(results)-1]; len(results) < limit &&
		((!reverse && last.ShardEnd != nil && bytes.Compare(last.ShardEnd, end) < 0) || (reverse && bytes.Compare(last.ShardBegin, begin) > 0)) {
		return nil
	}
	return results
}

// cachedLocationsLocked walks the cache from the scan origin like getCachedLocations,
// reporting a miss at the first uncached key. Caller must hold at least lc.mu.RLock().
func (lc *locationCache) cachedLocationsLocked(tenantId int64, begin, end []byte, limit int, reverse bool) ([]LocationResult, bool) {
	var results []LocationResult
	key := begin
	if reverse {
		key = end
	}
	for {
		loc, ok := lc.lookupLocked(tenantId, key, reverse)
		if !ok {
			return nil, false
		}
		results = append(results, loc)
		if len(results) == limit {
			return results, true
		}
		if reverse {
			if bytes.Compare(loc.ShardBegin, begin) <= 0 {
				return results, true
			}
			key = loc.ShardBegin
		} else {
			if loc.ShardEnd == nil || bytes.Compare(loc.ShardEnd, end) >= 0 {
				return results, true
			}
			key = loc.ShardEnd
		}
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
	// A non-prefixed boundary is left unchanged here; C++ toPrefixRelativeRange (FDBTypes.cpp:26-34)
	// instead clamps it to allKeys ("" .. "\xff\xff"). The difference is unreachable on a correct
	// cluster: the commit proxy already clamps every returned shard range to the tenant via
	// TenantAPI::clampRangeToTenant (CommitProxyServer.actor.cpp), so both boundaries always carry
	// the prefix. The else-branch only differs against a malformed proxy reply (tenant audit D3).
	for i := range entries {
		if bytes.HasPrefix(entries[i].begin, prefix[:]) {
			entries[i].begin = entries[i].begin[8:]
		}
		if entries[i].end != nil && bytes.HasPrefix(entries[i].end, prefix[:]) {
			entries[i].end = entries[i].end[8:]
		}
	}
}

// queryLocations is the shared load-balance loop for location queries.
// Cycles all commit proxies with exponential backoff until success or ctx
// cancellation. The buildRequest callback constructs the request body given
// a reply token. Used by both refresh (single key) and refreshRange (range).
func (lc *locationCache) queryLocations(db *database, ctx context.Context, tenantId int64, buildRequest func(replyToken transport.UID) []byte) ([]locationEntry, error) {
	var backoff time.Duration
	var replyCleanup replyGuard
	defer replyCleanup.close()
	var sendCleanup sendGuard
	defer sendCleanup.close()

	for {
		proxies := db.getCommitProxies()
		if len(proxies) == 0 {
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			}
			timer := time.NewTimer(backoff)
			notifyParked(ctx) // a delay is a wait
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
			notifyParked(ctx) // a delay is a wait
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
			locToken := getAdjustedEndpoint(proxy.Token, EndpointGetKeyServerLocations)
			// basicLoadBalance skips a permanently failed alternative (LoadBalance.actor.h:790).
			if db.failMon.endpointFailed(proxy.Address, locToken) {
				continue
			}
			conn, permit, err := db.getOrDialForSend(ctx, proxy.Address)
			if err != nil {
				db.handleDialError(ctx, proxy.Address)
				continue
			}

			sendCleanup.permit = permit
			replyToken, replyCh, replyHandle := conn.PrepareReplyTo(locToken)
			replyCleanup.handle = replyHandle
			body := buildRequest(replyToken)

			if err := sendReadFrame(ctx, conn, locToken, body, replyHandle, permit); err != nil {
				replyHandle.Cancel()
				replyCleanup.release()
				db.handleReadConnError(proxy.Address, err)
				continue
			}
			if hook := db.afterLocationSend.Load(); hook != nil {
				(*hook)()
			}

			rctx, rpcCancel := context.WithTimeout(ctx, DefaultRPCTimeout)
			select {
			case resp := <-replyCh:
				rpcCancel()
				replyCleanup.release()
				if resp.Err != nil {
					db.handleReadConnError(proxy.Address, resp.Err)
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
				replyCleanup.release()
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			cycledAll = false
		}

		if cycledAll {
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			} else {
				backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
			}
		}
	}
}

func (lc *locationCache) refreshRange(db *database, ctx context.Context, begin, end []byte, limit int, reverse bool, tenantId int64, span types.SpanContext) ([]locationEntry, error) {
	child := childSpanContext(span) // getKeyRangeLocations child, reused across proxy retries (:3184)
	return lc.queryLocations(db, ctx, tenantId, func(replyToken transport.UID) []byte {
		return buildGetKeyServerLocationsRangeRequest(begin, end, limit, reverse, tenantId, child, replyToken)
	})
}

// buildGetKeyServerLocationsRequest constructs the request with embedded reply token.
// Single-key lookup: no End field set. `span` is the getKeyLocation child span
// (derived once in refresh, reused across proxy retries) — stamped verbatim, matching
// C++ GetKeyServerLocationsRequest(span.context,…) (NativeAPI.actor.cpp:3037).
// buildGetKeyServerLocationsRequest builds a single-key location request. reverse mirrors C++
// getKeyLocation_internal's Reverse{k.isBackward()} (NativeAPI.actor.cpp:3008-3037): for a BACKWARD key
// selector (offset<=0, !orEqual) anchored exactly on a shard boundary the proxy must return the shard
// ENDING at the key (the one holding keyBefore(key)), not the shard beginning at it.
func buildGetKeyServerLocationsRequest(key []byte, reverse bool, tenantId int64, span types.SpanContext, replyToken transport.UID) []byte {
	req := types.GetKeyServerLocationsRequest{
		Begin:            key,
		Reverse:          reverse,
		Limit:            100,
		SpanContext:      span,
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
func buildGetKeyServerLocationsRangeRequest(begin, end []byte, limit int, reverse bool, tenantId int64, span types.SpanContext, replyToken transport.UID) []byte {
	req := types.GetKeyServerLocationsRequest{
		Begin:            begin,
		HasEnd:           true,
		End:              end,
		Limit:            int32(limit),
		Reverse:          reverse,
		SpanContext:      span, // getKeyRangeLocations child (NativeAPI.actor.cpp:3197)
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
					TLS:     endpointIsTLS(&ep),
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
