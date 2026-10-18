package wiretrace

import (
	"encoding/binary"
	"fmt"

	"github.com/zeebo/xxh3"
)

// Frame is one FDB packet: the destination endpoint token and its body.
type Frame struct {
	TimeNS int64 // capture time of the segment that made the frame's last byte readable
	Flow   Flow
	// Segment is that segment's capture index, so frames completed by one
	// captured segment share it.
	Segment int
	Token   [16]byte
	Body    []byte
}

const (
	frameLenWidth      = 4
	frameChecksumWidth = 8 // absent on TLS connections, which a capture cannot read
	frameTokenWidth    = 16
	maxFrameLen        = 1 << 30
)

// FrameParser splits one flow of a plaintext FDB connection into frames,
// after the length-prefixed ConnectPacket that opens each direction
// (FlowTransport.actor.cpp, ConnectPacket and scanPackets).
type FrameParser struct {
	flow      Flow
	buf       []byte
	connected bool
	emit      func(Frame) error
}

// NewFrameParser parses the flow's data in stream order.
func NewFrameParser(flow Flow, emit func(Frame) error) *FrameParser {
	return &FrameParser{flow: flow, emit: emit}
}

// Add takes the flow's next chunk.
func (p *FrameParser) Add(c Chunk) error {
	p.buf = append(p.buf, c.Data...)
	for {
		if !p.connected {
			if len(p.buf) < 4 {
				return nil
			}
			n := 4 + int(binary.LittleEndian.Uint32(p.buf))
			if n > 4096 {
				return fmt.Errorf("%v: connect packet of %d bytes", p.flow, n)
			}
			if len(p.buf) < n {
				return nil
			}
			p.buf = p.buf[n:]
			p.connected = true
			continue
		}
		const hdr = frameLenWidth + frameChecksumWidth
		if len(p.buf) < hdr {
			return nil
		}
		n := int(binary.LittleEndian.Uint32(p.buf))
		if n < frameTokenWidth || n > maxFrameLen {
			return fmt.Errorf("%v: frame payload of %d bytes", p.flow, n)
		}
		if len(p.buf) < hdr+n {
			return nil
		}
		payload := p.buf[hdr : hdr+n]
		// The checksum is XXH3-64 of the payload (FlowTransport.actor.cpp:1256-1273).
		if sum := binary.LittleEndian.Uint64(p.buf[frameLenWidth:hdr]); sum != xxh3.Hash(payload) {
			return fmt.Errorf("%v: frame checksum mismatch", p.flow)
		}
		f := Frame{TimeNS: c.TimeNS, Flow: p.flow, Segment: c.Packet}
		copy(f.Token[:], payload[:frameTokenWidth])
		f.Body = append([]byte(nil), payload[frameTokenWidth:]...)
		p.buf = p.buf[hdr+n:]
		if err := p.emit(f); err != nil {
			return err
		}
	}
}

// Finish reports a ConnectPacket or frame cut off by the end of the stream.
func (p *FrameParser) Finish() error {
	switch {
	case len(p.buf) == 0:
		return nil
	case !p.connected:
		return fmt.Errorf("%v: stream ended inside its connect packet (%d bytes)", p.flow, len(p.buf))
	default:
		return fmt.Errorf("%v: stream ended inside a frame (%d bytes)", p.flow, len(p.buf))
	}
}
