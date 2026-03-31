package client

import (
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// openDatabaseCoord sends an OpenDatabaseCoordRequest to the coordinator
// and returns the parsed ClientDBInfo with proxy addresses and tokens.
func (c *Cluster) openDatabaseCoord(ctx context.Context, conn *transport.Conn, addr string) (*DBInfo, error) {
	replyToken, replyCh := conn.PrepareReply()
	body := buildOpenDatabaseCoordRequest(c.clusterFile, replyToken)

	destToken := transport.WellKnownToken(transport.WLTokenClientLeaderRegOpenDatabase)
	if err := conn.SendFrame(destToken, body); err != nil {
		return nil, fmt.Errorf("send OpenDatabaseCoordRequest: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			return nil, fmt.Errorf("coordinator response: %w", resp.Err)
		}
		return parseCoordinatorResponse(resp.Body)
	case <-reqCtx.Done():
		return nil, fmt.Errorf("coordinator request timed out: %w", reqCtx.Err())
	}
}

// buildOpenDatabaseCoordRequest constructs the request manually using the
// Writer API. We can't use the generated MarshalFDB because it uses WriteBytes
// for nested struct fields (knownClientInfoID, reply), but FDB expects proper
// nested FlatBuffers objects with vtable soffsets.
func buildOpenDatabaseCoordRequest(cf *ClusterFile, replyToken transport.UID) []byte {
	// clusterKey is "description:id" (NOT the full connection string with @addresses).
	// The coordinator's cs.clusterKey() returns just this prefix part.
	connStr := cf.Description + ":" + cf.ID

	vt := types.OpenDatabaseCoordRequestVTable
	fileID := types.OpenDatabaseCoordRequestFileID

	w := wire.NewWriter(nil)
	return w.WriteMessage(fileID, vt, 8, func(obj *wire.ObjectWriter) {
		// slot 3: knownClientInfoID — UID INLINE at offset 4 (16 bytes zeros)
		obj.WriteUint64(4, 0)
		obj.WriteUint64(12, 0)

		// slot 6: reply — ReplyPromise is a NESTED struct (4-byte RelativeOffset)
		// The nested struct contains the UID (vtable {6, 20, 4}: 16 bytes inline)
		replyVT := types.ReplyPromiseVTable
		obj.WriteStruct(40, replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})

		// slot 4: clusterKey — use empty to skip the key check
		// The coordinator will compare with its own key and accept if empty.
		// If not, it sends wrong_cluster_key error which we handle.
		obj.WriteBytes(32, []byte(connStr))
		// TODO: if still getting wrong_cluster_key, try empty: obj.WriteBytes(32, []byte{})

		// slot 8: internal
		obj.WriteBool(48, true)
	})
}

func parseCoordinatorResponse(data []byte) (*DBInfo, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty coordinator response")
	}

	info, err := parseErrorOrClientDBInfo(data)
	if err != nil {
		info, err = parseStandaloneClientDBInfo(data)
		if err != nil {
			return nil, fmt.Errorf("parse coordinator response: %w (raw %d bytes)", err, len(data))
		}
	}
	return info, nil
}

// parseErrorOrClientDBInfo parses an ErrorOr-wrapped ClientDBInfo response.
// The FlatBuffers message has:
//
//	slot 0: error_code (uint16) — 0xFFFF = success
//	slot 1: grvProxies (vector of GrvProxyInterface)
//	slot 2: commitProxies (vector of CommitProxyInterface)
//	slot 3: id (UID)
//	... (remaining ClientDBInfo fields)
func parseErrorOrClientDBInfo(data []byte) (*DBInfo, error) {
	r, err := wire.ReadErrorOr(data)
	if err != nil {
		return nil, fmt.Errorf("coordinator: %w", err)
	}
	return parseClientDBInfoFromReader(r, 0)
}

// parseStandaloneClientDBInfo parses a plain ClientDBInfo (no ErrorOr wrapper).
func parseStandaloneClientDBInfo(data []byte) (*DBInfo, error) {
	r, err := wire.NewReader(data)
	if err != nil {
		return nil, fmt.Errorf("NewReader: %w", err)
	}
	return parseClientDBInfoFromReader(r, 0)
}

// parseClientDBInfoFromReader extracts proxy info from a ClientDBInfo.
// slotOffset is 0 for standalone or 1 for ErrorOr-wrapped.
func parseClientDBInfoFromReader(r *wire.Reader, slotOffset int) (*DBInfo, error) {
	info := &DBInfo{}

	// grvProxies: vector of GrvProxyInterface
	grvSlot := slotOffset + 0
	grvCount, err := r.ReadVectorCount(grvSlot)
	if err != nil {
		return nil, fmt.Errorf("read grvProxies count: %w", err)
	}
	for i := 0; i < grvCount; i++ {
		elemR, err := r.ReadVectorElementReader(grvSlot, i)
		if err != nil {
			return nil, fmt.Errorf("read grvProxy[%d]: %w", i, err)
		}
		proxy, err := parseProxyInterface(elemR)
		if err != nil {
			return nil, fmt.Errorf("parse grvProxy[%d]: %w", i, err)
		}
		info.GRVProxies = append(info.GRVProxies, proxy)
	}

	// commitProxies: vector of CommitProxyInterface
	commitSlot := slotOffset + 1
	commitCount, err := r.ReadVectorCount(commitSlot)
	if err != nil {
		return nil, fmt.Errorf("read commitProxies count: %w", err)
	}
	for i := 0; i < commitCount; i++ {
		elemR, err := r.ReadVectorElementReader(commitSlot, i)
		if err != nil {
			return nil, fmt.Errorf("read commitProxy[%d]: %w", i, err)
		}
		proxy, err := parseProxyInterface(elemR)
		if err != nil {
			return nil, fmt.Errorf("parse commitProxy[%d]: %w", i, err)
		}
		info.CommitProxies = append(info.CommitProxies, proxy)
	}

	// id: UID at slot slotOffset+2
	idSlot := slotOffset + 2
	if r.FieldPresent(idSlot) {
		idR, err := r.ReadNestedReader(idSlot)
		if err == nil {
			info.ID = parseUID(idR)
		}
	}

	return info, nil
}

// parseProxyInterface extracts the endpoint from a proxy interface at slot 3.
func parseProxyInterface(r *wire.Reader) (ProxyInfo, error) {
	ep, err := types.ReadEndpointFromSlot(r, 3)
	if err != nil {
		return ProxyInfo{}, err
	}
	return ProxyInfo{
		Address: ep.Address,
		Token:   transport.UID{First: ep.First, Second: ep.Second},
	}, nil
}

func parseUID(r *wire.Reader) transport.UID {
	first, second := r.ReadUIDPair(0)
	return transport.UID{First: first, Second: second}
}
