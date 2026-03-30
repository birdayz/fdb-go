package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/protocol"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
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

	// getKeyServerLocations is at getAdjustedEndpoint(2) from commit:
	//   first = commit.first + (2 << 32)
	//   second = (commit.second & 0xffffffff00000000) | (commit_index + 2)
	// where commit_index = commit.second & 0xffffffff
	commitIndex := uint32(proxy.Token.Second)
	locToken := transport.UID{
		First:  proxy.Token.First + (2 << 32),
		Second: (proxy.Token.Second & 0xFFFFFFFF00000000) | uint64(commitIndex+2),
	}

	if err := conn.SendFrame(locToken, body); err != nil {
		return nil, fmt.Errorf("send GetKeyServerLocations: %w", err)
	}

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			return nil, fmt.Errorf("locations response: %w", resp.Err)
		}
		return parseGetKeyServerLocationsReply(resp.Body)
	case <-rctx.Done():
		return nil, fmt.Errorf("locations request timed out: %w", rctx.Err())
	}
}

// buildGetKeyServerLocationsRequest constructs the request with embedded reply token.
// Real vtable from test vector: {22, 38, 12, 36, 16, 20, 37, 24, 28, 32, 4}
// slot 5 (Reply) at offset 24: nested ReplyPromise struct
// slot 8 (MinTenantVersion) at offset 4: int64
func buildGetKeyServerLocationsRequest(key []byte, replyToken transport.UID) []byte {
	// Use the real vtable from C++ test vector.
	// Real vtable: {22, 38, 12, 36, 16, 20, 37, 24, 28, 32, 4}
	// Serialize order: arena(0), spanContext(0), tenant(1), begin(2),
	//   end(3=type,4=value), limit(5), reverse(6), reply(7), minTenantVersion(8)
	//
	// But the slot-to-field mapping needs matching the C++ serialize order.
	// slot 0 (offset 12): spanContext (RelOff, leave as 0 = empty)
	// slot 1 (offset 36): end.type (uint8, 0 = absent)
	// slot 2 (offset 16): tenant (RelOff, 0 = empty)
	// slot 3 (offset 20): begin (RelOff to key data)
	// slot 4 (offset 37): reverse (bool, false)
	// slot 5 (offset 24): end.value (RelOff, 0 = absent)
	// slot 6 (offset 28): limit (int32)
	// slot 7 (offset 32): reply (RelOff to nested ReplyPromise)
	// slot 8 (offset 4): minTenantVersion (int64)
	vt := wire.VTable{22, 38, 12, 36, 16, 20, 37, 24, 28, 32, 4}
	fileID := protocol.GetKeyServerLocationsRequest_FileIdentifier

	w := wire.NewWriter(nil)
	return w.WriteMessage(fileID, vt, 8, func(obj *wire.ObjectWriter) {
		// slot 8: minTenantVersion at offset 4 (int64, -2 = latestVersion)
		obj.WriteInt64(4, -2)

		// begin at offset 20 (slot 3), tenant at offset 16 (slot 2)
		obj.WriteBytes(20, key)
		tenantVT := wire.VTable{10, 17, 4, 16, 12}
		obj.WriteStruct(16, tenantVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteInt64(4, -1) // tenantId = INVALID_TENANT
			// token type (uint8 at 16) and value (RelOff at 12) left as 0 (absent)
		})

		// reply at offset 24
		replyVT := wire.VTable{6, 20, 4}
		obj.WriteStruct(24, replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})

		// limit at offset 28
		obj.WriteInt32(28, 100)
	})
}

// parseGetKeyServerLocationsReply parses the ErrorOr-wrapped response.
// The response contains a vector of (KeyRange, StorageServerInterface[]) pairs.
func parseGetKeyServerLocationsReply(data []byte) ([]ServerInfo, error) {
	r, err := wire.NewReader(data)
	if err != nil {
		return nil, fmt.Errorf("parse locations reply: %w", err)
	}

	// Check for ErrorOr error
	nfields := r.VTableLength() - 2
	if nfields <= 1 {
		if r.FieldPresent(0) {
			errCode := r.ReadInt32(0)
			return nil, fmt.Errorf("FDB locations error: code %d", errCode)
		}
		return nil, fmt.Errorf("empty locations response")
	}

	// The reply has field 0 = results (vector of nested structs).
	// Each result contains a KeyRangeRef and a vector of StorageServerInterface.
	// For now, just extract the FIRST storage server's address.
	// Full parsing would extract all ranges and servers.

	// TODO: Full result parsing. For now, return the commit proxy as a fallback
	// server (single-node clusters have all roles on the same address).
	return nil, fmt.Errorf("location reply parsing not yet implemented (got %d fields in %d bytes)", nfields, len(data))
}
