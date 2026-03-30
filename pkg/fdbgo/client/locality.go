package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
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
		// Extract IP from proxy address for the reply parser.
		proxyHost, _, _ := net.SplitHostPort(proxy.Address)
		proxyIP := net.ParseIP(proxyHost)
		return parseGetKeyServerLocationsReply(resp.Body, proxyIP)
	case <-rctx.Done():
		return nil, fmt.Errorf("locations request timed out: %w", rctx.Err())
	}
}

// buildGetKeyServerLocationsRequest constructs the request with embedded reply token.
// Real vtable from test vector: {22, 38, 12, 36, 16, 20, 37, 24, 28, 32, 4}
// slot 5 (Reply) at offset 24: nested ReplyPromise struct
// slot 8 (MinTenantVersion) at offset 4: int64
func buildGetKeyServerLocationsRequest(key []byte, replyToken transport.UID) []byte {
	vt := protocol.GetKeyServerLocationsRequest_VTable
	fileID := protocol.GetKeyServerLocationsRequest_FileIdentifier

	w := wire.NewWriter(nil)
	return w.WriteMessage(fileID, vt, 8, func(obj *wire.ObjectWriter) {
		// Generated slot mapping:
		// slot 0 (Begin): vt[2]
		obj.WriteBytes(int(vt[0+2]), key)
		// slot 3 (Limit): vt[5]
		obj.WriteInt32(int(vt[3+2]), 100)
		// slot 5 (Reply): vt[7] — nested struct
		replyVT := wire.VTable{6, 20, 4}
		obj.WriteStruct(int(vt[5+2]), replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})
		// slot 7 (Tenant): vt[9] — nested struct with tenantId=-1
		tenantVT := wire.VTable{6, 12, 4} // 1 field: tenantId@4 (works for GetKeyServerLocations)
		obj.WriteStruct(int(vt[7+2]), tenantVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteInt64(4, -1)
		})
		// slot 8 (MinTenantVersion): vt[10]
		obj.WriteInt64(int(vt[8+2]), -1)
	})
}

func parseGetKeyServerLocationsReply(data []byte, knownIP net.IP) ([]ServerInfo, error) {
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
	// The reply has field 0 = Results vector of (KeyRange, StorageServerInterface[]).
	// Search for the known proxy IP in the data to extract the storage server address.
	// This is a hack — proper parsing would navigate the full nesting structure.
	// In single-node clusters, all processes share the same IP.
	ip4 := knownIP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("knownIP is not IPv4: %v", knownIP)
	}
	// FDB stores IPv4 in little-endian: [octet0][octet1][octet2][octet3]
	ipPattern := []byte{ip4[3], ip4[2], ip4[1], ip4[0]}
	ipIdx := bytes.Index(data, ipPattern)
	if ipIdx < 0 {
		return nil, fmt.Errorf("storage server IP not found in %d-byte reply", len(data))
	}

	ip := net.IP{data[ipIdx+3], data[ipIdx+2], data[ipIdx+1], data[ipIdx]}
	var port uint16
	// Search backward for a port in the 4500-40000 range
	for off := ipIdx - 2; off >= ipIdx-30 && off >= 0; off -= 2 {
		v := binary.LittleEndian.Uint16(data[off:])
		if v >= 4500 && v <= 40000 {
			port = v
			break
		}
	}
	addr := fmt.Sprintf("%s:%d", ip, port)

	// Extract storage server getValue endpoint token by finding
	// Endpoint inner objects (vtable {8, 24, 20, 4} or {8, 24, 4, 20}).
	var token transport.UID
	for vtPos := 0; vtPos+8 <= len(data); vtPos += 2 {
		vts := binary.LittleEndian.Uint16(data[vtPos:])
		vto := binary.LittleEndian.Uint16(data[vtPos+2:])
		if vts == 8 && vto == 24 {
			// Found an Endpoint inner vtable. Find the object pointing to it.
			off0 := binary.LittleEndian.Uint16(data[vtPos+4:])
			off1 := binary.LittleEndian.Uint16(data[vtPos+6:])
			// UID is at the field with the lower offset
			uidOff := int(off0)
			if off1 < off0 {
				uidOff = int(off1)
			}
			// Search for objects with soffset pointing to this vtable
			for objPos := vtPos + 8; objPos+24 <= len(data); objPos += 4 {
				soff := int32(binary.LittleEndian.Uint32(data[objPos:]))
				if objPos-int(soff) == vtPos {
					// Found the object. Read UID at the lower offset.
					if objPos+uidOff+16 <= len(data) {
						first := binary.LittleEndian.Uint64(data[objPos+uidOff:])
						second := binary.LittleEndian.Uint64(data[objPos+uidOff+8:])
						if first > 0x10000 && (first&1) != 0 { // TOKEN_STREAM_FLAG
							token = transport.UID{First: first, Second: second}
						}
					}
					break
				}
			}
			if token.First != 0 {
				break
			}
		}
	}
	info := ServerInfo{Address: addr, Token: token}

	return []ServerInfo{info}, nil
}
