package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
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

	// REAL vtable from C++ ground truth test vector (OpenDatabaseCoordRequest.json):
	// {22, 49, 20, 24, 28, 4, 32, 36, 40, 44, 48}
	// UID (slot 3) is 16 bytes INLINE at offset 4.
	// ReplyPromise (slot 6) is 4-byte RelativeOffset to nested struct at offset 40.
	vt := wire.VTable{22, 49, 20, 24, 28, 4, 32, 36, 40, 44, 48}
	fileID := types.OpenDatabaseCoordRequestFileID

	w := wire.NewWriter(nil)
	return w.WriteMessage(fileID, vt, 8, func(obj *wire.ObjectWriter) {
		// slot 3: knownClientInfoID — UID INLINE at offset 4 (16 bytes zeros)
		obj.WriteUint64(4, 0)
		obj.WriteUint64(12, 0)

		// slot 6: reply — ReplyPromise is a NESTED struct (4-byte RelativeOffset)
		// The nested struct contains the UID (vtable {6, 20, 4}: 16 bytes inline)
		replyVT := wire.VTable{6, 20, 4}
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
	// The response is ErrorOr<EnsureTable<ClientDBInfo>> with flattened FakeRoot.
	// The FakeRoot uses the ErrorOr union vtable: field0(type)@8, field1(value)@4.
	// NewReader navigates FakeRoot field0 (at hardcoded offset 4) which follows the
	// VALUE RelativeOffset, landing directly on the inner struct (Error or ClientDBInfo).
	//
	// To distinguish Error from ClientDBInfo, check the vtable field count:
	// - Error has 1 field (error_code int32)
	// - ClientDBInfo has 10+ fields
	r, err := wire.NewReader(data)
	if err != nil {
		return nil, fmt.Errorf("NewReader: %w", err)
	}

	nfields := r.VTableLength() - 2
	if nfields <= 1 {
		// This is the Error struct (1 field = error_code)
		if r.FieldPresent(0) {
			errCode := r.ReadInt32(0)
			return nil, fmt.Errorf("FDB coordinator error: code %d", errCode)
		}
		return nil, fmt.Errorf("FDB coordinator error (empty Error struct)")
	}

	// This is the ClientDBInfo struct (10+ fields).
	// Parse directly — no additional slot offset needed.
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

// parseGrvProxyInterface extracts the getConsistentReadVersion endpoint.
// Proxy vtable {12, 14, 12, 4, 13, 8}: 4 fields.
// Field 3 (offset 8) = Endpoint RelativeOffset.
func parseGrvProxyInterface(r *wire.Reader) (ProxyInfo, error) {
	return parseProxyEndpointFromSlot(r, 3)
}

// parseCommitProxyInterface extracts the commit endpoint.
// Same vtable layout as GrvProxyInterface.
func parseCommitProxyInterface(r *wire.Reader) (ProxyInfo, error) {
	return parseProxyEndpointFromSlot(r, 3)
}

// parseProxyEndpointFromSlot extracts address and token from a proxy interface
// at the specified endpoint slot.
//
// Chain: Proxy[slot] → Endpoint wrapper (1 field, RelOff) → Endpoint inner (2 fields)
//
//	inner field 0 (offset 20): NetworkAddressList RelOff
//	inner field 1 (offset 4): UID token INLINE (16 bytes)
//
// parseProxyEndpointFromSlot extracts address and token by following the
// exact nesting chain verified from live FDB 7.3.75 response data:
//
//	Proxy[endpointSlot] → Endpoint wrapper (1 field) → Endpoint inner (2 fields)
//	  inner field 1 (larger offset): UID token INLINE (16 bytes)
//	  inner field 0 (smaller offset): NetworkAddressList (RelOff)
//	    → NetworkAddress (4 fields): ip(RelOff), port(u16), flags(u16), fromHostname(u8)
//	      → IPAddress (2 fields): isV6?(u8), ipv4(RelOff to uint32)
func parseProxyEndpointFromSlot(r *wire.Reader, endpointSlot int) (ProxyInfo, error) {
	if !r.FieldPresent(endpointSlot) {
		return ProxyInfo{}, fmt.Errorf("endpoint slot %d not present", endpointSlot)
	}

	// Level 1: Proxy → Endpoint wrapper (serializable_traits wrapper, 1 field RelOff)
	epWrapper, err := r.ReadNestedReader(endpointSlot)
	if err != nil {
		return ProxyInfo{}, fmt.Errorf("read endpoint wrapper: %w", err)
	}

	if !epWrapper.FieldPresent(0) {
		return ProxyInfo{}, fmt.Errorf("endpoint wrapper field 0 absent")
	}

	// Level 2: Endpoint wrapper → Endpoint inner (2 fields)
	epInner, err := epWrapper.ReadNestedReader(0)
	if err != nil {
		return ProxyInfo{}, fmt.Errorf("read endpoint inner: %w", err)
	}

	var info ProxyInfo

	// Endpoint inner has 2 fields. The UID token is INLINE at the field with
	// the LARGER byte span (16 bytes). The NetworkAddressList is at the other
	// field (4-byte RelOffset).
	//
	// From live data: field 1 at offset 4 = UID (16 bytes), field 0 at offset 20 = addr RelOff.
	// But vtable sort may vary. Read UID from the field at the LOWER offset.
	nf := epInner.VTableLength() - 2
	if nf >= 2 {
		// Find the two fields and read UID from the one with more space
		off0 := epInner.FieldOffset(0)
		off1 := epInner.FieldOffset(1)

		// The UID field has the lower offset (closer to soffset, more room)
		uidOff := off1
		addrFieldSlot := 0
		if off0 < off1 {
			uidOff = off0
			addrFieldSlot = 1
		}

		// Read UID inline (16 bytes at uidOff)
		obj := epInner.ObjectBytes()
		if uidOff > 0 && uidOff+16 <= len(obj) {
			info.Token = transport.UID{
				First:  binary.LittleEndian.Uint64(obj[uidOff:]),
				Second: binary.LittleEndian.Uint64(obj[uidOff+8:]),
			}
		}

		// Read NetworkAddressList from the other field
		if epInner.FieldPresent(addrFieldSlot) {
			addrListR, err := epInner.ReadNestedReader(addrFieldSlot)
			if err == nil {
				info.Address = parseNetworkAddressList(addrListR)
			}
		}
	}

	if info.Address == "" {
		info.Address = "0.0.0.0:0"
	}
	return info, nil
}

// parseNetworkAddressList extracts "host:port" from a NetworkAddressList.
// NetworkAddressList has field 0 = NetworkAddress (RelOff to nested struct).
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
// Live data vtable {12, 13, 4, 8, 10, 12}: 4 fields.
//
//	field 0 at offset 4: IPAddress (RelOff to nested struct, 4 bytes)
//	field 1 at offset 8: port (uint16)
//	field 2 at offset 10: flags (uint16)
//	field 3 at offset 12: fromHostname (uint8)
func parseNetworkAddress(r *wire.Reader) string {
	var port uint16
	var ipStr string

	// port at field 1
	if r.FieldPresent(1) {
		port = r.ReadUint16(1)
	}

	// IP at field 0 (nested IPAddress struct)
	if r.FieldPresent(0) {
		ipR, err := r.ReadNestedReader(0)
		if err == nil {
			ipStr = parseIPAddress(ipR)
		}
	}

	if ipStr == "" {
		ipStr = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", ipStr, port)
}

// parseIPAddress extracts an IP string from an IPAddress nested struct.
// Live data vtable {8, 9, 8, 4}: 2 fields.
//
//	field 0 at offset 8: isV6 flag (uint8) — but may be inverted
//	field 1 at offset 4: IPv4 uint32 as RelOff, OR inline data
//
// The IPv4 address is found by following field 1 as RelOff → uint32 at target,
// or by scanning for recognizable IP bytes in the struct.
func parseIPAddress(r *wire.Reader) string {
	// IPAddress vtable {8, 9, 8, 4}: 2 fields.
	// field 0 at offset 8: isV6 flag (uint8) — 0=IPv4, 1=IPv6
	// field 1 at offset 4: IP data (RelOff to nested struct containing uint32 IPv4)
	//
	// The IPv4 uint32 is inside a nested struct pointed to by field 1.
	// That nested struct has field 0 = the uint32 IPv4.
	if !r.FieldPresent(1) {
		return ""
	}

	// Field 1 is a RelativeOffset to the raw IPv4 uint32 data.
	// Read the RelOff, follow it, and read the uint32 at the target.
	off := r.FieldOffset(1)
	obj := r.ObjectBytes()
	rawData := r.RawData()
	if off < 4 || off+4 > len(obj) {
		return ""
	}
	relOff := binary.LittleEndian.Uint32(obj[off:])
	target := r.ObjectPos() + int(off) + int(relOff)
	if target+4 > len(rawData) {
		return ""
	}
	ipv4 := binary.LittleEndian.Uint32(rawData[target:])
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, ipv4) // IPv4 → network byte order for net.IP
	return ip.String()
}

// parseUID reads a UID from a nested struct. UID fields are inline at offsets 4 and 12.
func parseUID(r *wire.Reader) transport.UID {
	obj := r.ObjectBytes()
	if r.FieldPresent(0) {
		off := r.FieldOffset(0)
		if off+16 <= len(obj) {
			return transport.UID{
				First:  binary.LittleEndian.Uint64(obj[off:]),
				Second: binary.LittleEndian.Uint64(obj[off+8:]),
			}
		}
	}
	return transport.UID{}
}
