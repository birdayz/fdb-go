// Package transport implements FDB's TCP wire protocol: framing, handshake,
// and request/response multiplexing via endpoint tokens.
package transport

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/zeebo/xxh3"
)

// Frame buffer pool. WriteFrame allocates a buffer per frame.
// Pool to reduce GC pressure. Uses *[]byte to avoid interface boxing allocation.
var writeFramePool = sync.Pool{New: func() any {
	b := make([]byte, 0, 4096)
	return &b
}}

// Frame layout on the wire:
//
//	Non-TLS: [packetLen(4 LE)][checksum(8 LE, XXH3-64)][payload(packetLen bytes)]
//	TLS:     [packetLen(4 LE)][payload(packetLen bytes)]
//
// packetLen does NOT include itself or the checksum. It IS the payload size.
// Minimum payload = 16 bytes (one UID token).
//
// Payload layout: [endpointToken(16 bytes = 2x uint64 LE)][serialized message body]

const (
	packetLenWidth = 4
	checksumWidth  = 8
	minPayloadSize = 16        // UID = 2x uint64
	maxPayloadSize = 100 << 20 // 100 MiB — prevent OOM on malicious payloadLen
)

// WriteFrame writes a framed message to w.
// If tls is true, the checksum is omitted.
func WriteFrame(w io.Writer, token UID, body []byte, tls bool) error {
	bufp, frame := encodeFrame(token, body, tls)
	_, err := w.Write(frame)
	putFrame(bufp, frame)
	return err
}

func frameSize(body []byte, tls bool) int {
	n := packetLenWidth + 16 + len(body)
	if !tls {
		n += checksumWidth
	}
	return n
}

// encodeFrameInto writes the framed message into dst (len(dst) == frameSize(body, tls)).
func encodeFrameInto(dst []byte, token UID, body []byte, tls bool) {
	payloadLen := 16 + len(body)
	binary.LittleEndian.PutUint32(dst, uint32(payloadLen))
	p := packetLenWidth
	if !tls {
		p += checksumWidth
	}
	binary.LittleEndian.PutUint64(dst[p:], token.First)
	binary.LittleEndian.PutUint64(dst[p+8:], token.Second)
	copy(dst[p+16:], body)
	if !tls {
		binary.LittleEndian.PutUint64(dst[packetLenWidth:], xxh3.Hash(dst[p:p+payloadLen]))
	}
}

// encodeFrame builds the frame in a pooled buffer; putFrame returns it.
func encodeFrame(token UID, body []byte, tls bool) (*[]byte, []byte) {
	totalSize := frameSize(body, tls)

	// Get buffer from pool, grow if needed.
	bufp := writeFramePool.Get().(*[]byte)
	buf := *bufp
	if cap(buf) < totalSize {
		buf = make([]byte, totalSize)
	} else {
		buf = buf[:totalSize]
	}

	encodeFrameInto(buf, token, body, tls)
	return bufp, buf
}

func putFrame(bufp *[]byte, frame []byte) {
	*bufp = frame[:0] // reset length, keep capacity
	writeFramePool.Put(bufp)
}

// FrameReader reads framed messages with a persistent header buffer to avoid
// per-frame heap allocations for the 4-byte length + 8-byte checksum reads.
// Not safe for concurrent use — intended for single-goroutine readLoop.
type FrameReader struct {
	hdr [packetLenWidth + checksumWidth]byte // reusable header buffer
}

// Read reads one framed message from r.
// Returns the endpoint token and the message body (without token).
func (fr *FrameReader) Read(r io.Reader, tls bool) (token UID, body []byte, err error) {
	// Read packet length using persistent header buffer.
	if _, err = io.ReadFull(r, fr.hdr[:packetLenWidth]); err != nil {
		return UID{}, nil, fmt.Errorf("read packet length: %w", err)
	}
	payloadLen := binary.LittleEndian.Uint32(fr.hdr[:packetLenWidth])

	if payloadLen < minPayloadSize {
		return UID{}, nil, fmt.Errorf("payload too short: %d < %d", payloadLen, minPayloadSize)
	}
	if payloadLen > maxPayloadSize {
		return UID{}, nil, fmt.Errorf("payload too large: %d > %d", payloadLen, maxPayloadSize)
	}

	// Read checksum (non-TLS only) using persistent header buffer.
	var expectedChecksum uint64
	if !tls {
		if _, err = io.ReadFull(r, fr.hdr[:checksumWidth]); err != nil {
			return UID{}, nil, fmt.Errorf("read checksum: %w", err)
		}
		expectedChecksum = binary.LittleEndian.Uint64(fr.hdr[:checksumWidth])
	}

	// Read payload.
	payload := make([]byte, payloadLen)
	if _, err = io.ReadFull(r, payload); err != nil {
		return UID{}, nil, fmt.Errorf("read payload: %w", err)
	}

	// Verify checksum (non-TLS only).
	if !tls {
		actualChecksum := xxh3.Hash(payload)
		if actualChecksum != expectedChecksum {
			return UID{}, nil, fmt.Errorf("checksum mismatch: got %x, want %x", actualChecksum, expectedChecksum)
		}
	}

	// Extract token.
	token.First = binary.LittleEndian.Uint64(payload[0:8])
	token.Second = binary.LittleEndian.Uint64(payload[8:16])

	body = payload[16:]
	return token, body, nil
}

// ReadFrame reads one framed message from r.
// Returns the endpoint token and the message body (without token).
// Deprecated: Use FrameReader.Read for reduced allocations.
func ReadFrame(r io.Reader, tls bool) (token UID, body []byte, err error) {
	var fr FrameReader
	return fr.Read(r, tls)
}

// UID is a 128-bit identifier (two uint64s). Used for endpoint tokens.
type UID struct {
	First  uint64
	Second uint64
}
