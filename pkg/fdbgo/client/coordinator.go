package client

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// errLongPollExpired reports a long poll the coordinator held past
// coordinatorLongPollCeiling; the caller re-sends it rather than failing.
var errLongPollExpired = errors.New("coordinator long poll expired")

// openDatabaseCoord sends an OpenDatabaseCoordRequest and returns the parsed
// ClientDBInfo. The coordinator holds the request while its info id equals
// known (Coordination.actor.cpp:234-249), so a non-zero known is a long poll;
// timeout bounds the wait.
func (db *database) openDatabaseCoord(ctx context.Context, conn *transport.Conn, snap *ClusterFile, known transport.UID, timeout time.Duration, permit *sendqueue.Permit) (*DBInfo, error) {
	replyToken, replyCh, replyHandle := conn.PrepareReply()
	replyCleanup := replyGuard{handle: replyHandle}
	defer replyCleanup.close()
	body, err := buildOpenDatabaseCoordRequest(snap, db.tlsConfig != nil, known, replyToken)
	if err != nil {
		replyHandle.Cancel()
		return nil, err
	}

	destToken := transport.WellKnownToken(transport.WLTokenClientLeaderRegOpenDatabase)
	if err := sendReadFrame(ctx, conn, destToken, body, replyHandle, permit, nil); err != nil {
		replyHandle.Cancel()
		return nil, fmt.Errorf("send OpenDatabaseCoordRequest: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp := <-replyCh:
		replyCleanup.release()
		if resp.Err != nil {
			return nil, fmt.Errorf("coordinator response: %w", resp.Err)
		}
		return parseCoordinatorResponse(resp.Body)
	case <-timer.C:
		replyHandle.Cancel()
		if known != (transport.UID{}) {
			return nil, errLongPollExpired
		}
		return nil, fmt.Errorf("coordinator request timed out: %w", context.DeadlineExceeded)
	case <-ctx.Done():
		replyHandle.Cancel()
		return nil, fmt.Errorf("coordinator request: %w", ctx.Err())
	}
}

// buildOpenDatabaseCoordRequest fills the fields C++ monitorProxiesOneGeneration
// sets (MonitorLeader.actor.cpp:873-881). The coordinator seeds its leader
// monitor from the first request's coordinators (Coordination.actor.cpp:331-333),
// so they must be the full set. supportedVersions, traceLogGroup, issues and
// internal only feed status reporting (Coordination.actor.cpp:228-231).
func buildOpenDatabaseCoordRequest(cf *ClusterFile, useTLS bool, known transport.UID, replyToken transport.UID) ([]byte, error) {
	coords, hostnames, err := coordinatorAddresses(cf, useTLS)
	if err != nil {
		return nil, err
	}
	req := types.CoordOpenDatabaseRequest{
		KnownClientInfoID: wire.UIDFromParts(known.First, known.Second),
		ClusterKey:        []byte(cf.Description + ":" + cf.ID),
		Coordinators:      coords,
		Reply:             types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		Hostnames:         hostnames,
		Internal:          true,
	}
	return req.MarshalFDB(), nil
}

// coordinatorAddresses splits the coordinators into C++ ClusterConnectionString's
// coords and hostnames, in order (MonitorLeader.actor.cpp:100-121). Addresses
// parse as NetworkAddress::parse does (flow/network.cpp:151-187): public, and
// IPv6 only when the address is (a bracketed IPv4 stays IPv4, :134-141). The TLS
// flag follows the transport the client dials, which WithTLSConfig can force: a
// coordinator compares it when matching its peers (FlowTransport.actor.cpp:1732).
func coordinatorAddresses(cf *ClusterFile, useTLS bool) ([]types.NetworkAddress, []types.Hostname, error) {
	var flags uint16
	if useTLS {
		flags = types.FlagTLS
	}
	var coords []types.NetworkAddress
	var hostnames []types.Hostname
	for _, addr := range cf.Coordinators {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, nil, fmt.Errorf("coordinator %q: %w", addr, err)
		}
		if isHostnameToken(addr) {
			hostnames = append(hostnames, types.Hostname{Host: []byte(host), Service: []byte(port), IsTLS: useTLS})
			continue
		}
		ip := net.ParseIP(host)
		p, err := strconv.ParseUint(port, 10, 16)
		if ip == nil || err != nil {
			return nil, nil, fmt.Errorf("coordinator %q is neither a hostname nor an IP address", addr)
		}
		na := types.NetworkAddress{Port: uint16(p), Flags: flags}
		if v4 := ip.To4(); v4 != nil && !strings.Contains(host, ":") {
			na.Ip = types.IPAddress{AddrTag: 1, AddrAlt0: binary.BigEndian.Uint32(v4)}
		} else {
			na.Ip = types.IPAddress{AddrTag: 2, AddrAlt1: ip.To16()}
		}
		coords = append(coords, na)
	}
	if len(coords)+len(hostnames) == 0 {
		// An empty list is what wedges a restarted coordinator; never send one.
		return nil, nil, errors.New("no coordinators to send")
	}
	return coords, hostnames, nil
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

	// The id is an inline UID struct, not a nested table. The long poll sends it
	// back as knownClientInfoID, so a zero id would make every poll return at once.
	if r.FieldPresent(types.ClientDBInfoSlotId) {
		first, second := r.ReadUIDPair(types.ClientDBInfoSlotId)
		info.ID = transport.UID{First: first, Second: second}
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
