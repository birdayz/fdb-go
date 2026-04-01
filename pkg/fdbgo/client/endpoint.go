package client

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// ReadEndpointFromSlot reads an Endpoint from a proxy/storage interface at the given slot.
// Chain: Interface[slot] → RequestStream wrapper (1 field RelOff) → Endpoint inner.
func ReadEndpointFromSlot(r *wire.Reader, slot int) (types.Endpoint, error) {
	var ep types.Endpoint
	if !r.FieldPresent(slot) {
		return ep, fmt.Errorf("endpoint slot %d not present", slot)
	}
	wrapper, err := r.ReadNestedReader(slot)
	if err != nil {
		return ep, fmt.Errorf("read endpoint wrapper: %w", err)
	}
	if !wrapper.FieldPresent(0) {
		return ep, fmt.Errorf("endpoint wrapper field 0 absent")
	}
	inner, err := wrapper.ReadNestedReader(0)
	if err != nil {
		return ep, fmt.Errorf("read endpoint inner: %w", err)
	}
	ep.UnmarshalFromReader(inner)
	return ep, nil
}

// endpointAddress returns "ip:port" for the endpoint's primary address.
func endpointAddress(ep *types.Endpoint) string {
	return networkAddressString(&ep.Addresses.Address)
}

// endpointToken returns the UID from the endpoint's 16-byte token.
func endpointToken(ep *types.Endpoint) transport.UID {
	return transport.UID{
		First:  binary.LittleEndian.Uint64(ep.Token[:8]),
		Second: binary.LittleEndian.Uint64(ep.Token[8:]),
	}
}

// endpointValid returns true if the endpoint has a non-zero token.
func endpointValid(ep *types.Endpoint) bool {
	return binary.LittleEndian.Uint64(ep.Token[:8]) != 0
}

func networkAddressString(na *types.NetworkAddress) string {
	ip := ipAddressString(&na.Ip)
	return fmt.Sprintf("%s:%d", ip, na.Port)
}

func ipAddressString(ip *types.IPAddress) string {
	switch ip.AddrTag {
	case 1:
		if ip.AddrAlt0 == 0 {
			return "0.0.0.0"
		}
		b := make(net.IP, 4)
		binary.BigEndian.PutUint32(b, ip.AddrAlt0)
		return b.String()
	case 2:
		if len(ip.AddrAlt1) >= 16 {
			return net.IP(ip.AddrAlt1[:16]).String()
		}
		return "::0"
	default:
		return "0.0.0.0"
	}
}
