package client

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// openDatabaseCoord sends an OpenDatabaseCoordRequest to the coordinator
// and returns the parsed ClientDBInfo with proxy addresses and tokens.
func (db *database) openDatabaseCoord(ctx context.Context, conn *transport.Conn, snap *ClusterFile, addr string) (*DBInfo, error) {
	replyToken, replyCh, replyHandle := conn.PrepareReply()
	defer replyHandle.Release()
	body := buildOpenDatabaseCoordRequest(snap, replyToken)

	destToken := transport.WellKnownToken(transport.WLTokenClientLeaderRegOpenDatabase)
	if err := conn.SendFrame(destToken, body); err != nil {
		replyHandle.Cancel()
		return nil, fmt.Errorf("send OpenDatabaseCoordRequest: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, CoordinatorTimeout)
	defer cancel()

	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			return nil, fmt.Errorf("coordinator response: %w", resp.Err)
		}
		return parseCoordinatorResponse(resp.Body)
	case <-reqCtx.Done():
		replyHandle.Cancel()
		return nil, fmt.Errorf("coordinator request timed out: %w", reqCtx.Err())
	}
}

func buildOpenDatabaseCoordRequest(cf *ClusterFile, replyToken transport.UID) []byte {
	req := types.OpenDatabaseCoordRequest{
		ClusterKey: []byte(cf.Description + ":" + cf.ID),
		Reply:      types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		Internal:   true,
	}
	return req.MarshalFDB()
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
	return parseClientDBInfoFromReader(r)
}

// parseStandaloneClientDBInfo parses a plain ClientDBInfo (no ErrorOr wrapper).
func parseStandaloneClientDBInfo(data []byte) (*DBInfo, error) {
	r, err := wire.NewReader(data)
	if err != nil {
		return nil, fmt.Errorf("NewReader: %w", err)
	}
	return parseClientDBInfoFromReader(r)
}

// parseClientDBInfoFromReader extracts proxy info from a ClientDBInfo.
func parseClientDBInfoFromReader(r *wire.Reader) (*DBInfo, error) {
	info := &DBInfo{}

	grvSlot := types.ClientDBInfoSlotGrvProxies
	grvCount, err := r.ReadVectorCount(grvSlot)
	if err != nil {
		return nil, fmt.Errorf("read grvProxies count: %w", err)
	}
	for i := 0; i < grvCount; i++ {
		elemR, err := r.ReadVectorElementReader(grvSlot, i)
		if err != nil {
			return nil, fmt.Errorf("read grvProxy[%d]: %w", i, err)
		}
		proxy, err := parseGrvProxyInterface(elemR)
		if err != nil {
			return nil, fmt.Errorf("parse grvProxy[%d]: %w", i, err)
		}
		info.GRVProxies = append(info.GRVProxies, proxy)
	}

	commitSlot := types.ClientDBInfoSlotCommitProxies
	commitCount, err := r.ReadVectorCount(commitSlot)
	if err != nil {
		return nil, fmt.Errorf("read commitProxies count: %w", err)
	}
	for i := 0; i < commitCount; i++ {
		elemR, err := r.ReadVectorElementReader(commitSlot, i)
		if err != nil {
			return nil, fmt.Errorf("read commitProxy[%d]: %w", i, err)
		}
		proxy, err := parseCommitProxyInterface(elemR)
		if err != nil {
			return nil, fmt.Errorf("parse commitProxy[%d]: %w", i, err)
		}
		info.CommitProxies = append(info.CommitProxies, proxy)
	}

	idSlot := types.ClientDBInfoSlotId
	if r.FieldPresent(idSlot) {
		idR, err := r.ReadNestedReader(idSlot)
		if err == nil {
			info.ID = parseUID(idR)
		}
	}

	// forward (Optional<Value>): when present, the coordinators handed back a new
	// connection string instead of proxies (a `coordinators auto`/`change`
	// rotation). Read exactly as types.ClientDBInfo.UnmarshalFromReader does —
	// slot-3 presence tag + slot-4 value (RFC-111 §3).
	if r.FieldPresent(types.ClientDBInfoSlotForward) && r.ReadUint8(types.ClientDBInfoSlotForward) > 0 {
		info.Forward = string(r.ReadBytes(types.ClientDBInfoSlotForward + 1))
	}

	return info, nil
}

func parseGrvProxyInterface(r *wire.Reader) (ProxyInfo, error) {
	return parseEndpointAsProxy(r, types.GrvProxyInterfaceSlotGetConsistentReadVersion)
}

func parseCommitProxyInterface(r *wire.Reader) (ProxyInfo, error) {
	return parseEndpointAsProxy(r, types.CommitProxyInterfaceSlotCommit)
}

func parseEndpointAsProxy(r *wire.Reader, slot int) (ProxyInfo, error) {
	ep, err := ReadEndpointFromSlot(r, slot)
	if err != nil {
		return ProxyInfo{}, err
	}
	return ProxyInfo{
		Address: endpointAddress(&ep),
		Token:   endpointToken(&ep),
	}, nil
}

func parseUID(r *wire.Reader) transport.UID {
	first, second := r.ReadUIDPair(0)
	return transport.UID{First: first, Second: second}
}
