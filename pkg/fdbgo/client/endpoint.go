package client

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"

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
	// net.JoinHostPort brackets an IPv6 host ([::1]:4500), matching C++ formatIpPort
	// (flow/network.cpp:242, `ip.isV6() ? "[%s]:%d" : "%s:%d"`). The old "%s:%d" produced
	// an unparseable "::1:4500" for IPv6 — broken for both dialing and GetAddressesForKey.
	return net.JoinHostPort(ipAddressString(&na.Ip), strconv.Itoa(int(na.Port)))
}

// networkAddressFlagTLS is C++ NetworkAddress::FLAG_TLS (flow/include/flow/network.h).
const networkAddressFlagTLS uint16 = 2

// endpointIsTLS reports whether the endpoint's primary address has the TLS flag set. C++
// NetworkAddress::toString appends ":tls" for these (flow/network.cpp:215); GetAddressesForKey
// echoes that suffix to the caller (NativeAPI.actor.cpp:5747 returns address().toString()).
func endpointIsTLS(ep *types.Endpoint) bool {
	return ep.Addresses.Address.Flags&networkAddressFlagTLS != 0
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
