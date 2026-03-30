package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/protocol"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// openDatabaseCoord sends an OpenDatabaseCoordRequest to the coordinator
// and returns the parsed ClientDBInfo with proxy addresses and tokens.
func (c *Cluster) openDatabaseCoord(ctx context.Context, conn *transport.Conn, addr string) (*DBInfo, error) {
	// Allocate reply token first — we need it embedded in the request body.
	replyToken, replyCh := conn.PrepareReply()

	// Build the request with the reply token embedded.
	body := buildOpenDatabaseCoordRequest(c.clusterFile, replyToken)

	// Send to the coordinator's well-known openDatabase endpoint.
	destToken := transport.WellKnownToken(transport.WLTokenClientLeaderRegOpenDatabase)
	if err := conn.SendFrame(destToken, body); err != nil {
		return nil, fmt.Errorf("send OpenDatabaseCoordRequest: %w", err)
	}

	// The server closes the connection after ~12s if we don't reply to PINGs.
	// We consume PINGs without replying (reply format WIP). Our request should
	// get a response within that window if the cluster is configured.
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
	connStr := cf.Description + ":" + cf.ID + "@"
	for i, addr := range cf.Coordinators {
		if i > 0 {
			connStr += ","
		}
		connStr += addr
	}

	// OpenDatabaseCoordRequest vtable from schema:
	// {22, 37, 4, 8, 12, 16, 20, 24, 28, 32, 36}
	// Fields (all wire_size=4 except internal=1):
	//   slot 0: issues (bytes)        offset 4
	//   slot 1: supportedVersions     offset 8
	//   slot 2: traceLogGroup (bytes) offset 12
	//   slot 3: knownClientInfoID     offset 16  ← nested UID struct
	//   slot 4: clusterKey (bytes)    offset 20
	//   slot 5: coordinators (bytes)  offset 24
	//   slot 6: reply                 offset 28  ← nested UID struct (ReplyPromise → token)
	//   slot 7: hostnames (bytes)     offset 32
	//   slot 8: internal (bool)       offset 36
	vt := protocol.OpenDatabaseCoordRequest_VTable
	fileID := protocol.OpenDatabaseCoordRequest_FileIdentifier

	w := wire.NewWriter(nil)
	return w.WriteMessage(fileID, vt, 4, func(obj *wire.ObjectWriter) {
		// slot 0: issues — empty vector (skip, leave as zero = absent)
		// slot 1: supportedVersions — empty (skip)
		// slot 2: traceLogGroup — empty (skip)

		// slot 3: knownClientInfoID — nested UID struct (all zeros = unknown)
		obj.WriteStruct(int(vt[3+2]), protocol.UID_VTable, 8, func(uid *wire.ObjectWriter) {
			uid.WriteUint64(int(protocol.UID_VTable[0+2]), 0)
			uid.WriteUint64(int(protocol.UID_VTable[1+2]), 0)
		})

		// slot 4: clusterKey — the cluster connection string
		obj.WriteBytes(int(vt[4+2]), []byte(connStr))

		// slot 5: coordinators — empty vector (coordinator knows its own topology)
		// slot 6: reply — ReplyPromise uses save/load (not serialize), so in
		// FlatBuffers it's stored as an opaque blob via BinaryWriter. The
		// BinaryWriter for ReplyPromise writes just the endpoint UID token
		// (part[0] + part[1], 16 bytes LE).
		replyBytes := make([]byte, 16)
		binary.LittleEndian.PutUint64(replyBytes[0:], replyToken.First)
		binary.LittleEndian.PutUint64(replyBytes[8:], replyToken.Second)
		obj.WriteBytes(int(vt[6+2]), replyBytes)

		// slot 7: hostnames — empty (skip)
		// slot 8: internal — true
		obj.WriteBool(int(vt[8+2]), true)
	})
}

// parseCoordinatorResponse parses the raw response from the coordinator.
// The response is a FlatBuffers message. We need to determine if it's:
// (a) A standalone ClientDBInfo message, or
// (b) An ErrorOr<EnsureTable<ClientDBInfo>> wrapped message
//
// We try both interpretations, starting with ErrorOr-wrapped (more likely).
func parseCoordinatorResponse(data []byte) (*DBInfo, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty coordinator response")
	}

	// Try parsing as ErrorOr<ClientDBInfo> (slot 0 = error_code, slots 1+ = ClientDBInfo).
	info, err := parseErrorOrClientDBInfo(data)
	if err != nil {
		// Fall back to parsing as standalone ClientDBInfo.
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
	r, err := wire.NewReader(data)
	if err != nil {
		return nil, fmt.Errorf("NewReader: %w", err)
	}

	// Slot 0: error_code (uint16). 0xFFFF (invalid_error_code) = success.
	if !r.FieldPresent(0) {
		return nil, fmt.Errorf("error_code field not present")
	}
	errorCode := r.ReadUint16(0)
	if errorCode != 0xFFFF {
		return nil, fmt.Errorf("FDB error from coordinator: code %d", errorCode)
	}

	// Parse ClientDBInfo fields at shifted slots (slot+1 from standalone).
	return parseClientDBInfoFromReader(r, 1)
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
		proxy, err := parseGrvProxyInterface(elemR)
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
		proxy, err := parseCommitProxyInterface(elemR)
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

// parseGrvProxyInterface extracts the getConsistentReadVersion endpoint
// from a GrvProxyInterface FlatBuffers object.
//
// GrvProxyInterface vtable slots:
//
//	slot 0: processId (Optional<Key>)
//	slot 2: provisional (bool, inline)
//	slot 3: getConsistentReadVersion (RequestStream → Endpoint)
//
// But the actual slot numbering in FlatBuffers depends on how save_members
// flattens the fields. We need to find the Endpoint (address + token).
func parseGrvProxyInterface(r *wire.Reader) (ProxyInfo, error) {
	return parseProxyEndpoint(r)
}

// parseCommitProxyInterface extracts the commit endpoint.
// Same structure as GrvProxyInterface but with different endpoint.
func parseCommitProxyInterface(r *wire.Reader) (ProxyInfo, error) {
	return parseProxyEndpoint(r)
}

// parseProxyEndpoint extracts address and token from a proxy interface.
// The proxy interface has processId, provisional, and the main RequestStream (Endpoint).
// We scan through the vtable slots looking for the Endpoint-like structure.
//
// Strategy: try to find two consecutive uint64 values that look like a UID,
// and a NetworkAddress (IPv4 + port) nearby. This is heuristic until we
// validate the exact vtable layout against a real coordinator.
func parseProxyEndpoint(r *wire.Reader) (ProxyInfo, error) {
	// The FlatBuffers layout flattens all nested types. For a proxy interface:
	//
	// From the schema, GrvProxyInterface has 3 fields at slots 0, 2, 3:
	//   slot 0: processId (Optional → 2 vtable entries: type + value)
	//   slot 2: provisional (bool, 1 vtable entry)
	//   slot 3: getConsistentReadVersion (RequestStream → Endpoint)
	//
	// The RequestStream serializes as its Endpoint: serializer(ar, endpoint)
	// Endpoint serializes as: serializer(ar, addresses, token)
	// addresses (NetworkAddressList): serializer(ar, address, secondaryAddress)
	// address (NetworkAddress): serializer(ar, ip, port, flags, fromHostname)
	// token (UID): serializer(ar, part[0], part[1])
	//
	// Flattened, the proxy interface vtable looks like:
	//   Slot 0: processId.type (uint8, Optional tag)
	//   Slot 1: processId.value (RelativeOffset, Optional value)
	//   Slot 2: provisional (bool)
	//   Slot 3: addresses.address.ip (RelativeOffset → IPAddress)
	//   Slot 4: addresses.address.port (uint16)
	//   Slot 5: addresses.address.flags (uint16)
	//   Slot 6: addresses.address.fromHostname (RelativeOffset → struct)
	//   Slot 7: addresses.secondaryAddress (Optional → type)
	//   Slot 8: addresses.secondaryAddress (Optional → value)
	//   Slot 9: token.part[0] (uint64)
	//   Slot 10: token.part[1] (uint64)
	//
	// However, the exact mapping depends on save_members field ordering.
	// We'll try to extract from the expected slots and validate.
	//
	// Actually, the Endpoint, NetworkAddressList, etc. are expect_serialize_member
	// types, so they become NESTED structs (not flattened). Each gets its own
	// vtable and object. So the proxy interface at the top level has:
	//   Slot 0: processId (Optional)
	//   Slot 1: processId value
	//   Slot 2: provisional
	//   Slot 3: getConsistentReadVersion → RelativeOffset to Endpoint nested struct
	//
	// And the Endpoint has its own sub-structure.

	// Try slot 3 as the RequestStream/Endpoint.
	// RequestStream serializes as its Endpoint. Check if it's a nested struct.
	endpointSlot := 3
	if !r.FieldPresent(endpointSlot) {
		// Try scanning for any present nested struct
		for s := 0; s < r.VTableLength()-2; s++ {
			if r.FieldPresent(s) {
				endpointSlot = s
			}
		}
	}

	endpointR, err := r.ReadNestedReader(endpointSlot)
	if err != nil {
		// Fallback: try to read address/token from this reader directly
		return extractProxyInfoDirect(r)
	}

	return parseEndpoint(endpointR)
}

// parseEndpoint extracts address and token from an Endpoint FlatBuffers object.
//
// Endpoint::serialize for FlatBuffers:
//
//	serializer(ar, addresses, token)
//
// addresses is NetworkAddressList (nested struct at slot 0)
// token is UID (nested struct at slot 1)
func parseEndpoint(r *wire.Reader) (ProxyInfo, error) {
	var info ProxyInfo

	// Token at slot 1 (nested UID struct)
	if r.FieldPresent(1) {
		tokenR, err := r.ReadNestedReader(1)
		if err == nil {
			uid := parseUID(tokenR)
			info.Token = uid
		}
	}

	// Address at slot 0 (nested NetworkAddressList struct)
	if r.FieldPresent(0) {
		addrR, err := r.ReadNestedReader(0)
		if err == nil {
			info.Address = parseNetworkAddressList(addrR)
		}
	}

	if info.Address == "" {
		return info, fmt.Errorf("no address in endpoint")
	}
	return info, nil
}

// parseNetworkAddressList extracts "host:port" from a NetworkAddressList.
// NetworkAddressList: serializer(ar, address, secondaryAddress)
// address is NetworkAddress (nested at slot 0)
func parseNetworkAddressList(r *wire.Reader) string {
	if !r.FieldPresent(0) {
		return ""
	}
	addrR, err := r.ReadNestedReader(0)
	if err != nil {
		return ""
	}
	return parseNetworkAddress(addrR)
}

// parseNetworkAddress extracts "host:port" from a NetworkAddress.
// NetworkAddress for modern protocol (hasIPv6):
//
//	serializer(ar, ip, port, flags, fromHostname)
//
// IPAddress: serializer(ar, isV6, addr_bytes_or_v4)
// In FlatBuffers, IPAddress is also a nested struct or flattened.
//
// The schema shows IPAddress at slot 0 as nested struct, port at slots 1 (inline uint16),
// flags at slot 2 (inline uint16).
func parseNetworkAddress(r *wire.Reader) string {
	// Read port (uint16 at slot 1 or 4 depending on flattening)
	// Read IPv4 (uint32 at slot 4 in the multi-version schema)
	//
	// The NetworkAddress schema has multiple serialization versions.
	// For the modern protocol (7.3), try:
	//   Slot 0: ip (nested IPAddress struct)
	//   Slot 1: port (uint16)
	//   Slot 2: flags (uint16)
	//   Slot 3: fromHostname (nested)

	var port uint16
	var ipStr string

	// Try reading port from slot 1 (modern layout)
	if r.FieldPresent(1) {
		port = r.ReadUint16(1)
	}

	// Try reading IP from slot 0 as nested struct
	if r.FieldPresent(0) {
		ipR, err := r.ReadNestedReader(0)
		if err == nil {
			ipStr = parseIPAddress(ipR)
		}
	}

	// Fallback: try slot 4 as direct IPv4 uint32 (old layout from schema)
	if ipStr == "" && r.FieldPresent(4) {
		ipv4 := r.ReadUint32(4)
		ip := make(net.IP, 4)
		binary.LittleEndian.PutUint32(ip, ipv4)
		ipStr = ip.String()
	}

	if ipStr == "" || port == 0 {
		// Last resort: scan for plausible port+ip values
		for s := 0; s < r.VTableLength()-2; s++ {
			if r.FieldPresent(s) {
				v := r.ReadUint16(s)
				if v > 1024 && v < 65535 && port == 0 {
					port = v
				}
			}
		}
	}

	if ipStr == "" {
		ipStr = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", ipStr, port)
}

// parseIPAddress extracts an IP string from an IPAddress nested struct.
// IPAddress: serializer(ar, isV6, addr)
//
//	slot 0: isV6 (bool)
//	slot 1: IPv4 uint32 or IPv6 bytes
func parseIPAddress(r *wire.Reader) string {
	isV6 := false
	if r.FieldPresent(0) {
		isV6 = r.ReadBool(0)
	}

	if isV6 {
		// IPv6: 16 bytes at slot 1
		if r.FieldPresent(1) {
			// Read as bytes
			data := r.ReadBytes(1)
			if len(data) == 16 {
				ip := net.IP(data)
				return ip.String()
			}
		}
		return ""
	}

	// IPv4: uint32 at slot 1
	if r.FieldPresent(1) {
		ipv4 := r.ReadUint32(1)
		ip := make(net.IP, 4)
		binary.LittleEndian.PutUint32(ip, ipv4)
		return ip.String()
	}
	return ""
}

// parseUID reads a UID from a nested UID reader.
func parseUID(r *wire.Reader) transport.UID {
	var uid transport.UID
	if r.FieldPresent(0) {
		uid.First = r.ReadUint64(0)
	}
	if r.FieldPresent(1) {
		uid.Second = r.ReadUint64(1)
	}
	return uid
}

// extractProxyInfoDirect attempts to extract address and token directly
// from the proxy interface reader when nested struct navigation fails.
func extractProxyInfoDirect(r *wire.Reader) (ProxyInfo, error) {
	return ProxyInfo{}, fmt.Errorf("could not extract proxy endpoint (need real FDB to validate layout)")
}
