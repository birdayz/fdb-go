package transport

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// ConnectPacket is exchanged by both sides on TCP connection establishment.
// Packed struct (no padding), 44 bytes total.
//
// Layout:
//
//	Offset  Size  Field
//	0       4     connectPacketLength (uint32 LE) = 40 (excludes itself)
//	4       8     protocolVersion (uint64 LE, with objectSerializerFlag bit 60)
//	12      2     canonicalRemotePort (uint16 LE)
//	14      8     connectionId (uint64 LE)
//	22      4     canonicalRemoteIp4 (uint32 LE)
//	26      2     flags (uint16 LE, bit 0 = FLAG_IPV6)
//	28      16    canonicalRemoteIp6 (16 bytes, only meaningful if FLAG_IPV6)
//
// Total: 4 + 40 = 44 bytes.
const ConnectPacketSize = 44

// Protocol version constants.
const (
	// objectSerializerFlag signals FlatBuffers serialization (always set in modern FDB).
	ObjectSerializerFlag uint64 = 0x1000000000000000

	// compatibleProtocolVersionMask: top 48 bits must match for compatibility.
	CompatibleProtocolVersionMask uint64 = 0xFFFFFFFFFFFF0000

	// FDB 7.3 protocol version (without flags).
	ProtocolVersion73 uint64 = 0x0FDB00B073000000

	// FLAG_IPV6 in ConnectPacket.flags.
	FlagIPv6 uint16 = 1
)

// ConnectPacket represents the FDB connection handshake packet.
type ConnectPacket struct {
	ProtocolVersion     uint64
	CanonicalRemotePort uint16
	ConnectionID        uint64
	CanonicalRemoteIP4  uint32
	Flags               uint16
	CanonicalRemoteIP6  [16]byte
}

// Marshal serializes the ConnectPacket to wire format (44 bytes).
func (p *ConnectPacket) Marshal() []byte {
	buf := make([]byte, ConnectPacketSize)
	binary.LittleEndian.PutUint32(buf[0:], 40) // length excludes itself
	binary.LittleEndian.PutUint64(buf[4:], p.ProtocolVersion|ObjectSerializerFlag)
	binary.LittleEndian.PutUint16(buf[12:], p.CanonicalRemotePort)
	binary.LittleEndian.PutUint64(buf[14:], p.ConnectionID)
	binary.LittleEndian.PutUint32(buf[22:], p.CanonicalRemoteIP4)
	binary.LittleEndian.PutUint16(buf[26:], p.Flags)
	copy(buf[28:], p.CanonicalRemoteIP6[:])
	return buf
}

// Unmarshal deserializes a ConnectPacket from wire format.
func (p *ConnectPacket) Unmarshal(buf []byte) error {
	if len(buf) < ConnectPacketSize {
		return fmt.Errorf("connect packet too short: %d < %d", len(buf), ConnectPacketSize)
	}
	pktLen := binary.LittleEndian.Uint32(buf[0:])
	if pktLen > 40 {
		return fmt.Errorf("connect packet length too large: %d", pktLen)
	}
	p.ProtocolVersion = binary.LittleEndian.Uint64(buf[4:])
	p.CanonicalRemotePort = binary.LittleEndian.Uint16(buf[12:])
	p.ConnectionID = binary.LittleEndian.Uint64(buf[14:])
	p.CanonicalRemoteIP4 = binary.LittleEndian.Uint32(buf[22:])
	p.Flags = binary.LittleEndian.Uint16(buf[26:])
	copy(p.CanonicalRemoteIP6[:], buf[28:44])
	return nil
}

// HasObjectSerializerFlag returns true if the protocol version uses FlatBuffers.
func (p *ConnectPacket) HasObjectSerializerFlag() bool {
	return p.ProtocolVersion&ObjectSerializerFlag != 0
}

// IsCompatible returns true if the peer's protocol version is compatible.
func (p *ConnectPacket) IsCompatible(ourVersion uint64) bool {
	theirs := p.ProtocolVersion & ^ObjectSerializerFlag
	ours := ourVersion & ^ObjectSerializerFlag
	return (theirs & CompatibleProtocolVersionMask) == (ours & CompatibleProtocolVersionMask)
}

// IsIPv6 returns true if the canonical address is IPv6.
func (p *ConnectPacket) IsIPv6() bool {
	return p.Flags&FlagIPv6 != 0
}

// WriteConnectPacket sends a ConnectPacket to the connection.
func WriteConnectPacket(w io.Writer, localAddr net.Addr, connectionID uint64) error {
	pkt := ConnectPacket{
		ProtocolVersion: ProtocolVersion73,
		ConnectionID:    connectionID,
	}

	// Set canonical address from local addr.
	if tcpAddr, ok := localAddr.(*net.TCPAddr); ok {
		pkt.CanonicalRemotePort = uint16(tcpAddr.Port)
		if ip4 := tcpAddr.IP.To4(); ip4 != nil {
			pkt.CanonicalRemoteIP4 = binary.BigEndian.Uint32(ip4)
		} else if ip6 := tcpAddr.IP.To16(); ip6 != nil {
			pkt.Flags = FlagIPv6
			copy(pkt.CanonicalRemoteIP6[:], ip6)
		}
	}

	_, err := w.Write(pkt.Marshal())
	return err
}

// ReadConnectPacket reads a ConnectPacket from the connection.
func ReadConnectPacket(r io.Reader) (*ConnectPacket, error) {
	buf := make([]byte, ConnectPacketSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read connect packet: %w", err)
	}
	var pkt ConnectPacket
	if err := pkt.Unmarshal(buf); err != nil {
		return nil, err
	}
	return &pkt, nil
}
