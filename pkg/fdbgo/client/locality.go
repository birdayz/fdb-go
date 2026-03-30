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

	// The getKeyServerLocations endpoint is at commit.token + 2
	// (adjacent batch registration: commit=0, getConsistentReadVersion=1,
	// getKeyServerLocations=2, etc.)
	locToken := transport.UID{
		First:  proxy.Token.First,
		Second: proxy.Token.Second + 2,
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
	// Use the generated MarshalFDB with the reply token embedded as a
	// nested struct. The generated code uses WriteBytes for the reply
	// which is wrong — we override with WriteStruct.
	vt := protocol.GetKeyServerLocationsRequest_VTable
	fileID := protocol.GetKeyServerLocationsRequest_FileIdentifier

	w := wire.NewWriter(nil)
	return w.WriteMessage(fileID, vt, 8, func(obj *wire.ObjectWriter) {
		// slot 0: Begin key
		obj.WriteBytes(int(vt[0+2]), key)
		// slot 3: Limit
		obj.WriteInt32(int(vt[3+2]), 100)
		// slot 5: Reply (nested ReplyPromise struct)
		replyVT := wire.VTable{6, 20, 4}
		obj.WriteStruct(int(vt[5+2]), replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})
		// slot 8: MinTenantVersion
		obj.WriteInt64(int(vt[8+2]), -1)
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
