package types

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// IPAddress — fdbrpc/IPAddress.h
// VTable {8, 9, 8, 4}: isV6 (uint8 at offset 8), addr (RelOff at offset 4)
func ReadIPAddress(r *wire.Reader) string {
	// Field 1 at offset 4 = IPv4 uint32 via RelativeOffset
	ipv4 := r.ReadIPv4(1)
	if ipv4 == 0 {
		return "0.0.0.0"
	}
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, ipv4)
	return ip.String()
}

// NetworkAddress — fdbrpc/Locality.h
// VTable {12, 13, 4, 8, 10, 12}: ip (nested, slot 0), port (uint16, slot 1),
// flags (uint16, slot 2), fromHostname (uint8, slot 3)
func ReadNetworkAddress(r *wire.Reader) string {
	var ipStr string
	var port uint16

	if r.FieldPresent(0) {
		ipR, err := r.ReadNestedReader(0)
		if err == nil {
			ipStr = ReadIPAddress(ipR)
		}
	}
	if r.FieldPresent(1) {
		port = r.ReadUint16(1)
	}

	if ipStr == "" {
		ipStr = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", ipStr, port)
}

// ReadNetworkAddressList reads the primary address from a NetworkAddressList.
// VTable {8, 24, 4, 20} or similar: field 0 = primary NetworkAddress (nested).
func ReadNetworkAddressList(r *wire.Reader) string {
	if !r.FieldPresent(0) {
		return ""
	}
	addrR, err := r.ReadNestedReader(0)
	if err != nil {
		return ""
	}
	return ReadNetworkAddress(addrR)
}

// EndpointInfo holds a parsed Endpoint (address + UID token).
type EndpointInfo struct {
	Address string
	First   uint64
	Second  uint64
}

// ReadEndpoint reads an Endpoint from a nested reader.
// Endpoint inner has 2 fields: UID token (16 bytes inline) and NetworkAddressList (RelOff).
// The UID is always at the field with the LOWER byte offset (closer to soffset).
func ReadEndpoint(r *wire.Reader) (EndpointInfo, error) {
	nf := r.VTableLength() - 2
	if nf < 2 {
		return EndpointInfo{}, fmt.Errorf("endpoint: expected 2+ fields, got %d", nf)
	}

	off0 := r.FieldOffset(0)
	off1 := r.FieldOffset(1)

	// UID is at the lower offset (16 bytes inline, needs more space from soffset)
	uidSlot, addrSlot := 1, 0
	if off0 < off1 {
		uidSlot, addrSlot = 0, 1
	}

	first, second := r.ReadUIDPair(uidSlot)

	var addr string
	if r.FieldPresent(addrSlot) {
		addrListR, err := r.ReadNestedReader(addrSlot)
		if err == nil {
			addr = ReadNetworkAddressList(addrListR)
		}
	}
	if addr == "" {
		addr = "0.0.0.0:0"
	}

	return EndpointInfo{Address: addr, First: first, Second: second}, nil
}

// ReadEndpointFromSlot reads an Endpoint from a proxy/storage interface at the given slot.
// Chain: Interface[slot] → Endpoint wrapper (1 field RelOff) → Endpoint inner (2 fields).
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
	return ReadEndpoint(inner)
}
