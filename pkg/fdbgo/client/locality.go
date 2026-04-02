package client

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sync"

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
	begin   []byte
	end     []byte
	servers []ServerInfo
}

// ServerInfo holds a storage server's address and endpoint token.
type ServerInfo struct {
	Address string
	Token   transport.UID
}

// locate finds the storage servers responsible for a key.
// On cache miss, queries a commit proxy.
func (lc *locationCache) locate(db *database, ctx context.Context, key []byte) ([]ServerInfo, error) {
	// Check cache first.
	lc.mu.RLock()
	for _, entry := range lc.entries {
		if bytes.Compare(key, entry.begin) >= 0 &&
			(entry.end == nil || bytes.Compare(key, entry.end) < 0) {
			servers := entry.servers
			lc.mu.RUnlock()
			return servers, nil
		}
	}
	lc.mu.RUnlock()

	// Cache miss — query commit proxy.
	return lc.refresh(db, ctx, key)
}

// invalidate removes cached entries containing the given key.
// Called on wrong_shard_server errors.
func (lc *locationCache) invalidate(key []byte) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	filtered := lc.entries[:0]
	for _, entry := range lc.entries {
		if bytes.Compare(key, entry.begin) >= 0 &&
			(entry.end == nil || bytes.Compare(key, entry.end) < 0) {
			continue // remove this entry
		}
		filtered = append(filtered, entry)
	}
	lc.entries = filtered
}

func (lc *locationCache) refresh(db *database, ctx context.Context, key []byte) ([]ServerInfo, error) {
	proxies := db.getCommitProxies()
	if len(proxies) == 0 {
		return nil, &wire.FDBError{Code: ErrAllProxiesUnreachable}
	}

	for _, proxy := range proxies {
		conn, err := db.getOrDial(ctx, proxy.Address)
		if err != nil {
			db.handleConnError(proxy.Address)
			continue
		}

		replyToken, replyCh, cancelReply := conn.PrepareReply()
		body := buildGetKeyServerLocationsRequest(key, replyToken)
		locToken := getAdjustedEndpoint(proxy.Token, EndpointGetKeyServerLocations)

		if err := conn.SendFrame(locToken, body); err != nil {
			cancelReply()
			db.handleConnError(proxy.Address)
			continue
		}

		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			if resp.Err != nil {
				db.handleConnError(proxy.Address)
				continue
			}
			entries, err := parseGetKeyServerLocationsReply(resp.Body)
			if err != nil {
				continue
			}
			// Cache the returned shard ranges with size cap.
			lc.mu.Lock()
			lc.entries = append(lc.entries, entries...)
			for len(lc.entries) > lc.maxSize {
				idx := rand.Intn(len(lc.entries))
				lc.entries[idx] = lc.entries[len(lc.entries)-1]
				lc.entries = lc.entries[:len(lc.entries)-1]
			}
			lc.mu.Unlock()
			if len(entries) > 0 {
				return entries[0].servers, nil
			}
		case <-rctx.Done():
			cancel()
			cancelReply()
			continue
		}
	}

	db.kickTopology()
	return nil, &wire.FDBError{Code: ErrAllProxiesUnreachable}
}

// buildGetKeyServerLocationsRequest constructs the request with embedded reply token.
func buildGetKeyServerLocationsRequest(key []byte, replyToken transport.UID) []byte {
	req := types.GetKeyServerLocationsRequest{
		Begin:            key,
		Limit:            100,
		Reply:            types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		Tenant:           types.TenantInfo{TenantId: NoTenantID},
		MinTenantVersion: NoTenantID,
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
