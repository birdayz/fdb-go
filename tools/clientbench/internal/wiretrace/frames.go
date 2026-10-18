package wiretrace

import (
	"encoding/binary"
	"fmt"
)

// Frame is one FDB packet: the destination endpoint token and its body.
type Frame struct {
	TimeNS int64 // capture time of the segment carrying the frame's last byte
	Flow   Flow
	// Segment numbers the chunks of a flow, so frames carried by one TCP
	// segment share it.
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
	segments  int
	emit      func(Frame) error
}

// NewFrameParser parses the flow's data in stream order.
func NewFrameParser(flow Flow, emit func(Frame) error) *FrameParser {
	return &FrameParser{flow: flow, emit: emit}
}

// Add takes the flow's next chunk.
func (p *FrameParser) Add(c Chunk) error {
	p.segments++
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
		f := Frame{TimeNS: c.TimeNS, Flow: p.flow, Segment: p.segments}
		copy(f.Token[:], payload[:frameTokenWidth])
		f.Body = append([]byte(nil), payload[frameTokenWidth:]...)
		p.buf = p.buf[hdr+n:]
		if err := p.emit(f); err != nil {
			return err
		}
	}
}
