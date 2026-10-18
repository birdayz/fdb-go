// Package wiretrace decodes FoundationDB client traffic from a loopback packet
// capture: it reassembles each TCP connection, splits it into FDB frames, and
// matches GRV and commit requests with their replies.
package wiretrace

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Endpoint is an IPv4 address and TCP port.
type Endpoint struct {
	IP   [4]byte
	Port uint16
}

func (e Endpoint) String() string {
	return fmt.Sprintf("%d.%d.%d.%d:%d", e.IP[0], e.IP[1], e.IP[2], e.IP[3], e.Port)
}

// Flow is one direction of a TCP connection.
type Flow struct{ Src, Dst Endpoint }

// Segment is one captured TCP segment.
type Segment struct {
	TimeNS  int64
	Flow    Flow
	Seq     uint32
	SYN     bool
	Payload []byte
}

const (
	pcapMagicMicro = 0xa1b2c3d4
	pcapMagicNano  = 0xa1b23c4d
	linkEthernet   = 1
	linkRaw        = 101
	linkLinuxSLL   = 113
)

// ReadPcap calls fn for every IPv4 TCP segment in a classic pcap stream.
// Loopback captures on Linux use the Ethernet link type.
func ReadPcap(r io.Reader, fn func(Segment) error) error {
	var hdr [24]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("pcap header: %w", err)
	}
	var order binary.ByteOrder = binary.LittleEndian
	magic := order.Uint32(hdr[0:4])
	if magic != pcapMagicMicro && magic != pcapMagicNano {
		order = binary.BigEndian
		magic = order.Uint32(hdr[0:4])
	}
	var scale int64
	switch magic {
	case pcapMagicMicro:
		scale = 1000
	case pcapMagicNano:
		scale = 1
	default:
		return fmt.Errorf("not a classic pcap file (magic %#x)", hdr[0:4])
	}
	link := order.Uint32(hdr[20:24])
	var rec [16]byte
	buf := make([]byte, 0, 65536)
	for {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("pcap record header: %w", err)
		}
		ts := int64(order.Uint32(rec[0:4]))*1e9 + int64(order.Uint32(rec[4:8]))*scale
		n := int(order.Uint32(rec[8:12]))
		orig := int(order.Uint32(rec[12:16]))
		if n != orig {
			return fmt.Errorf("truncated packet (%d of %d bytes): capture with -s 0", n, orig)
		}
		if cap(buf) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("pcap record: %w", err)
		}
		seg, ok, err := parsePacket(link, buf)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		seg.TimeNS = ts
		seg.Payload = append([]byte(nil), seg.Payload...) // buf is reused
		if err := fn(seg); err != nil {
			return err
		}
	}
}

func parsePacket(link uint32, b []byte) (Segment, bool, error) {
	switch link {
	case linkEthernet:
		if len(b) < 14 || binary.BigEndian.Uint16(b[12:14]) != 0x0800 {
			return Segment{}, false, nil
		}
		b = b[14:]
	case linkLinuxSLL:
		if len(b) < 16 || binary.BigEndian.Uint16(b[14:16]) != 0x0800 {
			return Segment{}, false, nil
		}
		b = b[16:]
	case linkRaw:
	default:
		return Segment{}, false, fmt.Errorf("unsupported link type %d", link)
	}
	if len(b) < 20 || b[0]>>4 != 4 || b[9] != 6 {
		return Segment{}, false, nil
	}
	ihl := int(b[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(b[2:4]))
	if total > len(b) || ihl < 20 || total < ihl+20 {
		return Segment{}, false, nil
	}
	var seg Segment
	copy(seg.Flow.Src.IP[:], b[12:16])
	copy(seg.Flow.Dst.IP[:], b[16:20])
	t := b[ihl:total]
	off := int(t[12]>>4) * 4
	if off < 20 || off > len(t) {
		return Segment{}, false, nil
	}
	seg.Flow.Src.Port = binary.BigEndian.Uint16(t[0:2])
	seg.Flow.Dst.Port = binary.BigEndian.Uint16(t[2:4])
	seg.Seq = binary.BigEndian.Uint32(t[4:8])
	seg.SYN = t[13]&0x02 != 0
	seg.Payload = t[off:]
	return seg, true, nil
}

// Chunk is in-order stream data and the capture time of the segment carrying it.
type Chunk struct {
	TimeNS int64
	Data   []byte
}

// Reassembler orders the payload of each flow whose SYN was captured. Flows
// that started before the capture are ignored, so a stream always begins at
// its first byte.
type Reassembler struct {
	streams map[Flow]*stream
	emit    func(Flow, Chunk) error
	// Gaps counts flows abandoned because a segment never arrived.
	Gaps int
}

type stream struct {
	next    uint32
	pending map[uint32]Segment
	broken  bool
}

// NewReassembler calls emit with each flow's data in stream order.
func NewReassembler(emit func(Flow, Chunk) error) *Reassembler {
	return &Reassembler{streams: map[Flow]*stream{}, emit: emit}
}

// maxPending bounds the out-of-order segments held per flow before the flow
// is declared to have a gap.
const maxPending = 4096

// Add takes the next captured segment.
func (r *Reassembler) Add(seg Segment) error {
	s := r.streams[seg.Flow]
	if seg.SYN {
		s = &stream{next: seg.Seq + 1, pending: map[uint32]Segment{}}
		r.streams[seg.Flow] = s
		seg.Seq++ // SYN consumes one sequence number
		if len(seg.Payload) == 0 {
			return nil
		}
	}
	if s == nil || s.broken || len(seg.Payload) == 0 {
		return nil
	}
	s.pending[seg.Seq] = seg
	for {
		next, ok := s.take()
		if !ok {
			break
		}
		// Buffered data becomes readable only now, when the stream is contiguous.
		if err := r.emit(seg.Flow, Chunk{TimeNS: seg.TimeNS, Data: next.Payload}); err != nil {
			return err
		}
	}
	if len(s.pending) > maxPending {
		s.broken = true
		r.Gaps++
	}
	return nil
}

// take returns the pending data that continues the stream, trimming any part
// already delivered (a retransmission).
func (s *stream) take() (Segment, bool) {
	if seg, ok := s.pending[s.next]; ok {
		delete(s.pending, s.next)
		s.next += uint32(len(seg.Payload))
		return seg, true
	}
	for seq, seg := range s.pending {
		behind := s.next - seq // wraps: small when seq precedes next
		if behind > 0 && behind < 1<<30 {
			delete(s.pending, seq)
			if int(behind) < len(seg.Payload) {
				seg.Payload = seg.Payload[behind:]
				seg.Seq = s.next
				s.next += uint32(len(seg.Payload))
				return seg, true
			}
		}
	}
	return Segment{}, false
}
