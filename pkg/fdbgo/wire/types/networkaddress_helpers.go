package types

// Endpoint reader chain — uses generated types for navigation.
// EndpointInfo is a Go convenience type (flattened address + UID).
// TODO: fully generate once IPv6 vector_like alternative is supported (#3).

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// EndpointInfo holds a parsed Endpoint (address string + UID token parts).
type EndpointInfo struct {
	Address string
	First   uint64
	Second  uint64
}

// ReadEndpointFromSlot reads an Endpoint from a proxy/storage interface at the given slot.
// Chain: Interface[slot] → RequestStream wrapper (1 field RelOff) → Endpoint inner.
func ReadEndpointFromSlot(r *wire.Reader, slot int) (EndpointInfo, error) {
	if !r.FieldPresent(slot) {
		return EndpointInfo{}, fmt.Errorf("endpoint slot %d not present", slot)
	}
	wrapper, err := r.ReadNestedReader(slot)
	if err != nil {
		return EndpointInfo{}, fmt.Errorf("read endpoint wrapper: %w", err)
	}
	if !wrapper.FieldPresent(0) {
		return EndpointInfo{}, fmt.Errorf("endpoint wrapper field 0 absent")
	}
	inner, err := wrapper.ReadNestedReader(0)
	if err != nil {
		return EndpointInfo{}, fmt.Errorf("read endpoint inner: %w", err)
	}

	// Read Endpoint using generated type.
	var ep Endpoint
	ep.UnmarshalFromReader(inner)

	// Extract address string from the nested NetworkAddressList → NetworkAddress → IPAddress chain.
	addr := formatNetworkAddress(&ep.Addresses.Address)
	first := binary.LittleEndian.Uint64(ep.Token[:8])
	second := binary.LittleEndian.Uint64(ep.Token[8:])

	if addr == "" {
		addr = "0.0.0.0:0"
	}
	return EndpointInfo{Address: addr, First: first, Second: second}, nil
}

// formatNetworkAddress extracts "ip:port" from a generated NetworkAddress.
func formatNetworkAddress(na *NetworkAddress) string {
	ipStr := formatIPAddress(&na.Ip)
	if ipStr == "" {
		ipStr = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", ipStr, na.Port)
}

// formatIPAddress extracts an IP string from a generated IPAddress.
func formatIPAddress(ip *IPAddress) string {
	switch ip.Field_0Tag {
	case 1: // IPv4: uint32
		if ip.Field_0Alt0 == 0 {
			return "0.0.0.0"
		}
		b := make(net.IP, 4)
		binary.BigEndian.PutUint32(b, ip.Field_0Alt0)
		return b.String()
	case 2: // IPv6: array<uint8_t, 16> (vector_like, raw bytes at RelOff)
		// TODO: read 16 bytes from vector_like format [count=16][16 bytes]
		return "::1" // placeholder
	default:
		return "0.0.0.0"
	}
}
