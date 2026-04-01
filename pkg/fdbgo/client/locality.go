package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// LocationCache maps key ranges to storage server addresses.
// Populated by GetKeyServerLocationsRequest to commit proxies.
// Invalidated on wrong_shard_server errors.
type LocationCache struct {
	cluster *Cluster
	mu      sync.RWMutex
	entries []locationEntry
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

// NewLocationCache creates a location cache.
func NewLocationCache(cluster *Cluster) *LocationCache {
	return &LocationCache{cluster: cluster}
}

// Locate finds the storage servers responsible for a key.
// On cache miss, queries a commit proxy.
func (lc *LocationCache) Locate(ctx context.Context, key []byte) ([]ServerInfo, error) {
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
	return lc.refresh(ctx, key)
}

// Invalidate removes cached entries containing the given key.
// Called on wrong_shard_server errors.
func (lc *LocationCache) Invalidate(key []byte) {
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

func (lc *LocationCache) refresh(ctx context.Context, key []byte) ([]ServerInfo, error) {
	proxy, err := lc.cluster.GetCommitProxy()
	if err != nil {
		return nil, fmt.Errorf("get commit proxy: %w", err)
	}

	conn, err := lc.cluster.getOrDial(ctx, proxy.Address)
	if err != nil {
		return nil, fmt.Errorf("dial commit proxy: %w", err)
	}

	replyToken, replyCh := conn.PrepareReply()
	body := buildGetKeyServerLocationsRequest(key, replyToken)

	locToken := getAdjustedEndpoint(proxy.Token, EndpointGetKeyServerLocations)

	if err := conn.SendFrame(locToken, body); err != nil {
		return nil, fmt.Errorf("send GetKeyServerLocations: %w", err)
	}

	rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
	defer cancel()

	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			return nil, fmt.Errorf("locations response: %w", resp.Err)
		}
		entries, err := parseGetKeyServerLocationsReply(resp.Body)
		if err != nil {
			return nil, err
		}
		// Cache the returned shard ranges.
		lc.mu.Lock()
		lc.entries = append(lc.entries, entries...)
		lc.mu.Unlock()
		// Return servers for the first entry (covers the queried key).
		if len(entries) > 0 {
			return entries[0].servers, nil
		}
		return nil, fmt.Errorf("no location entries")
	case <-rctx.Done():
		return nil, fmt.Errorf("locations request timed out: %w", rctx.Err())
	}
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
