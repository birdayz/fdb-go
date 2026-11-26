package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// refresh queries commit proxies for the location of a single key. `span` is the
// issuing transaction's span; the getKeyLocation CHILD is derived once here and
// reused across the queryLocations proxy retries — matching C++ getKeyLocation_internal
// (NativeAPI.actor.cpp:3017 `Span("NAPI:getKeyLocation", spanContext)`, built once
// before basicLoadBalance).
func (lc *locationCache) refresh(db *database, ctx context.Context, key []byte, tenantId int64, span types.SpanContext, isBackward bool, replyWait func(context.Context)) (LocationResult, error) {
	child := childSpanContext(span)
	entries, err := lc.queryLocations(db, ctx, tenantId, func(replyToken transport.UID) []byte {
		return buildGetKeyServerLocationsRequest(key, isBackward, tenantId, child, replyToken)
	}, replyWait)
	if err != nil {
		return LocationResult{}, err
	}
	return LocationResult{
		Servers:    entries[0].servers,
		ShardBegin: entries[0].begin,
		ShardEnd:   entries[0].end,
	}, nil
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

// withServers returns the entries a request can be routed to.
func withServers(entries []locationEntry) []locationEntry {
	if !slices.ContainsFunc(entries, func(e locationEntry) bool { return len(e.servers) == 0 }) {
		return entries
	}
	return slices.DeleteFunc(slices.Clone(entries), func(e locationEntry) bool { return len(e.servers) == 0 })
}

// invertedEntry reports an entry with begin >= end: after the parser, only a
// boundary stripTenantPrefix leaves unprefixed produces one.
func invertedEntry(entries []locationEntry) bool {
	for _, e := range entries {
		if e.end != nil && bytes.Compare(e.begin, e.end) >= 0 {
			return true
		}
	}
	return false
}

// queryLocations is the shared load-balance loop for location queries.
// Cycles all commit proxies with exponential backoff until success or ctx
// cancellation. The buildRequest callback constructs the request body given
// a reply token. Used by both refresh (single key) and refreshRange (range).
func (lc *locationCache) queryLocations(db *database, ctx context.Context, tenantId int64, buildRequest func(replyToken transport.UID) []byte, replyWait func(context.Context)) ([]locationEntry, error) {
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
			reset, err := waitLocationBackoff(db, ctx, backoff)
			if err != nil {
				return nil, err
			}
			if reset {
				backoff = 0
			} else {
				backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
			}
			continue
		}

		if backoff > 0 {
			reset, err := waitLocationBackoff(db, ctx, backoff)
			if err != nil {
				return nil, err
			}
			if reset {
				backoff = 0
			}
		}

		// Every attempt that does not return moves on to the next proxy; a full
		// cycle without an answer backs off.
		for _, proxy := range proxies {
			entries, ok, err := lc.queryLocationProxy(db, ctx, tenantId, proxy, buildRequest, replyWait, &replyCleanup, &sendCleanup)
			if err != nil {
				return nil, err
			}
			if ok {
				return entries, nil
			}
		}

		if backoff == 0 {
			backoff = loadBalanceStartBackoff
		} else {
			backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
		}
	}
}

func waitLocationBackoff(db *database, ctx context.Context, backoff time.Duration) (reset bool, err error) {
	timer := time.NewTimer(backoff)
	notifyParked(ctx) // a delay is a wait
	select {
	case <-timer.C:
		return false, nil
	case <-db.waitProxiesChanged():
		timer.Stop()
		return true, nil
	case <-ctx.Done():
		timer.Stop()
		return false, ctx.Err()
	case <-db.ctx.Done():
		timer.Stop()
		return false, db.ctx.Err()
	}
}

func (lc *locationCache) queryLocationProxy(
	db *database,
	ctx context.Context,
	tenantId int64,
	proxy ProxyInfo,
	buildRequest func(replyToken transport.UID) []byte,
	replyWait func(context.Context),
	replyCleanup *replyGuard,
	sendCleanup *sendGuard,
) ([]locationEntry, bool, error) {
	locToken := getAdjustedEndpoint(proxy.Token, EndpointGetKeyServerLocations)
	// basicLoadBalance skips a permanently failed alternative (LoadBalance.actor.h:790).
	if db.failMon.endpointFailed(proxy.Address, locToken) {
		return nil, false, nil
	}
	conn, permit, err := db.getOrDialForSend(ctx, proxy.Address)
	if err != nil {
		db.handleDialError(ctx, proxy.Address)
		return nil, false, nil
	}

	sendCleanup.permit = permit
	replyToken, replyCh, replyHandle := conn.PrepareReplyTo(locToken)
	replyCleanup.handle = replyHandle
	body := buildRequest(replyToken)

	if err := sendReadFrame(ctx, conn, locToken, body, replyHandle, permit, replyWait); err != nil {
		replyHandle.Cancel()
		replyCleanup.release()
		db.handleReadConnError(proxy.Address, conn, err)
		return nil, false, nil
	}
	if h := db.hooks.Load(); h != nil {
		if hook := h.afterLocationSend.Load(); hook != nil {
			(*hook)()
		}
	}

	rctx, rpcCancel := context.WithTimeout(ctx, DefaultRPCTimeout)
	select {
	case resp := <-replyCh:
		rpcCancel()
		replyCleanup.release()
		if resp.Err != nil {
			db.handleReadConnError(proxy.Address, conn, resp.Err)
			return nil, false, nil
		}
		entries, err := parseGetKeyServerLocationsReply(resp.Body)
		if err != nil {
			if db.logger != nil {
				db.logger.Debug("fdbgo: location reply discarded", "proxy", proxy.Address, "error", err)
			}
			return nil, false, nil
		}
		for i := range entries {
			entries[i].tenantId = tenantId
		}
		stripTenantPrefix(entries, tenantId)
		if invertedEntry(entries) {
			if db.logger != nil {
				db.logger.Debug("fdbgo: location reply discarded", "proxy", proxy.Address, "error", "inverted range after the tenant prefix strip")
			}
			return nil, false, nil
		}
		lc.mu.Lock()
		lc.cacheReply(entries)
		lc.evictIfNeeded()
		lc.mu.Unlock()
		if usable := withServers(entries); len(usable) > 0 {
			return usable, true, nil
		}
		return nil, false, nil
	case <-rctx.Done():
		rpcCancel()
		replyHandle.Cancel()
		replyCleanup.release()
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, nil
	}
}

func (lc *locationCache) refreshRange(db *database, ctx context.Context, begin, end []byte, limit int, reverse bool, tenantId int64, span types.SpanContext) ([]locationEntry, error) {
	child := childSpanContext(span) // getKeyRangeLocations child, reused across proxy retries (:3184)
	return lc.queryLocations(db, ctx, tenantId, func(replyToken transport.UID) []byte {
		return buildGetKeyServerLocationsRangeRequest(begin, end, limit, reverse, tenantId, child, replyToken)
	}, nil)
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

// locationsDefect names why a GetKeyServerLocations reply was not used.
type locationsDefect int

const (
	locationsUnreadable    locationsDefect = iota + 1 // out of bounds: no defined C++ result
	locationsInvertedRange                            // C++ throws or asserts (FDBTypes.h:384-399)
	locationsNoShards                                 // no non-empty range
)

func (d locationsDefect) String() string {
	switch d {
	case locationsUnreadable:
		return "unreadable"
	case locationsInvertedRange:
		return "inverted range"
	default:
		return "no shards"
	}
}

// locationsReplyError reports a reply nothing is cached from; C++ never delivers
// a reply that fails to deserialize (FlowTransport.actor.cpp:1195-1207).
type locationsReplyError struct {
	defect locationsDefect
	detail string
	err    error
}

func (e *locationsReplyError) Error() string {
	msg := "locations reply " + e.defect.String() + ": " + e.detail
	if e.err != nil {
		return msg + ": " + e.err.Error()
	}
	return msg
}

func (e *locationsReplyError) Unwrap() error { return e.err }

// parseGetKeyServerLocationsReply skips an absent or empty range, which C++ does not
// cache (KeyRangeMap.h:209-210); a shard without servers is returned without them.
func parseGetKeyServerLocationsReply(data []byte) ([]locationEntry, error) {
	r, err := wire.ReadErrorOr(data)
	if err != nil {
		return nil, fmt.Errorf("locations reply: %w", err)
	}

	// Parse vector of pair<KeyRangeRef, vector<StorageServerInterface>> using
	// generated slot constants and types.
	// One budget covers both vector levels, so aliased offsets cannot multiply the walk.
	budget := wire.NewDecodeBudget(data)
	pairCount, err := r.ReadVectorCount(types.GetKeyServerLocationsReplySlotResults)
	if err == nil {
		err = budget.Take(pairCount)
	}
	if err != nil {
		return nil, &locationsReplyError{defect: locationsUnreadable, detail: "results", err: err}
	}

	entries := make([]locationEntry, 0, pairCount)
	for i := 0; i < pairCount; i++ {
		pairR, err := r.ReadVectorElementReader(types.GetKeyServerLocationsReplySlotResults, i)
		if err != nil {
			return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d", i), err: err}
		}

		// Pair slot 0: KeyRangeRef (nested struct).
		if !pairR.FieldPresent(types.LocationPairSlotKeyRange) {
			if err := pairR.Err(); err != nil {
				return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d", i), err: err}
			}
			continue
		}
		krR, err := pairR.ReadNestedReader(types.LocationPairSlotKeyRange)
		if err != nil {
			return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d range", i), err: err}
		}
		var kr types.KeyRangeRef
		if err := kr.UnmarshalFromReader(krR); err != nil {
			return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d range", i), err: err}
		}
		// A present key with a zero RelativeOffset reads as empty, as in C++, and would
		// widen the range.
		for _, slot := range [...]int{types.KeyRangeRefSlotBegin, types.KeyRangeRefSlotEnd} {
			if krR.FieldPresent(slot) && krR.ReadBytes(slot) == nil {
				return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d range key", i)}
			}
		}
		// An absent end is the empty key, as C++ decodes it: ["a", absent) is inverted.
		switch c := bytes.Compare(kr.Begin, kr.End); {
		case c > 0:
			return nil, &locationsReplyError{defect: locationsInvertedRange, detail: fmt.Sprintf("result %d range [%q, %q)", i, kr.Begin, kr.End)}
		case c == 0:
			continue
		}

		// Pair slot 1: vector<StorageServerInterface>.
		ssCount, err := pairR.ReadVectorCount(types.LocationPairSlotServers)
		if err == nil {
			err = budget.Take(ssCount)
		}
		if err != nil {
			return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d servers", i), err: err}
		}
		var servers []ServerInfo
		for j := 0; j < ssCount; j++ {
			ssR, err := pairR.ReadVectorElementReader(types.LocationPairSlotServers, j)
			if err != nil {
				return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d server %d", i, j), err: err}
			}
			// Slot 2 = getKeyValues RequestStream in StorageServerInterface.
			// We read any endpoint to get the base token; slot 2 is the most
			// reliably present in all FDB versions.
			ep, err := ReadEndpointFromSlot(ssR, types.StorageServerInterfaceSlotField_2)
			// A present but malformed endpoint fails the reply; the scan below probes
			// slots that may hold no endpoint at all, so it skips their errors.
			var absent *endpointAbsentError
			if err != nil && !errors.As(err, &absent) {
				return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d server %d", i, j), err: err}
			}
			if err != nil || !endpointValid(&ep) {
				// Scan only the slots this schema knows; the peer chooses the vtable length.
				nf := min(ssR.VTableLength(), len(types.StorageServerInterfaceVTable)) - 2
				for s := 0; s < nf; s++ {
					ep, err = ReadEndpointFromSlot(ssR, s)
					if err == nil && endpointValid(&ep) {
						break
					}
				}
			}
			if err := ssR.Err(); err != nil {
				return nil, &locationsReplyError{defect: locationsUnreadable, detail: fmt.Sprintf("result %d server %d", i, j), err: err}
			}
			if endpointValid(&ep) {
				servers = append(servers, ServerInfo{
					Address: endpointAddress(&ep),
					Token:   endpointToken(&ep),
					TLS:     endpointIsTLS(&ep),
				})
			}
		}
		entries = append(entries, locationEntry{
			begin:   kr.Begin,
			end:     kr.End,
			servers: servers,
		})
	}

	if len(entries) == 0 {
		return nil, &locationsReplyError{defect: locationsNoShards, detail: "no shard"}
	}
	return entries, nil
}
