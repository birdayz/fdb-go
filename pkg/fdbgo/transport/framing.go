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
	payloadLen := 16 + len(body) // token + body
	headerSize := packetLenWidth
	if !tls {
		headerSize += checksumWidth
	}

	totalSize := headerSize + payloadLen

	// Get buffer from pool, grow if needed.
	bufp := writeFramePool.Get().(*[]byte)
	buf := *bufp
	if cap(buf) < totalSize {
		buf = make([]byte, totalSize)
	} else {
		buf = buf[:totalSize]
	}

	off := 0

	// Packet length (does not include itself or checksum).
	binary.LittleEndian.PutUint32(buf[off:], uint32(payloadLen))
	off += packetLenWidth

	// Payload: token + body.
	payloadStart := off
	if !tls {
		payloadStart += checksumWidth
	}
	binary.LittleEndian.PutUint64(buf[payloadStart:], token.First)
	binary.LittleEndian.PutUint64(buf[payloadStart+8:], token.Second)
	copy(buf[payloadStart+16:], body)

	// Checksum (non-TLS only): XXH3-64 over payload.
	if !tls {
		checksum := xxh3.Hash(buf[payloadStart : payloadStart+payloadLen])
		binary.LittleEndian.PutUint64(buf[off:], checksum)
	}

	_, err := w.Write(buf)
	*bufp = buf[:0] // reset length, keep capacity
	writeFramePool.Put(bufp)
	return err
}

// ReadFrame reads one framed message from r.
// Returns the endpoint token and the message body (without token).
func ReadFrame(r io.Reader, tls bool) (token UID, body []byte, err error) {
	// Read packet length.
	var lenBuf [packetLenWidth]byte
	if _, err = io.ReadFull(r, lenBuf[:]); err != nil {
		return UID{}, nil, fmt.Errorf("read packet length: %w", err)
	}
	payloadLen := binary.LittleEndian.Uint32(lenBuf[:])

	if payloadLen < minPayloadSize {
		return UID{}, nil, fmt.Errorf("payload too short: %d < %d", payloadLen, minPayloadSize)
	}
	if payloadLen > maxPayloadSize {
		return UID{}, nil, fmt.Errorf("payload too large: %d > %d", payloadLen, maxPayloadSize)
	}

	// Read checksum (non-TLS only).
	var expectedChecksum uint64
	if !tls {
		var csumBuf [checksumWidth]byte
		if _, err = io.ReadFull(r, csumBuf[:]); err != nil {
			return UID{}, nil, fmt.Errorf("read checksum: %w", err)
		}
		expectedChecksum = binary.LittleEndian.Uint64(csumBuf[:])
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

// UID is a 128-bit identifier (two uint64s). Used for endpoint tokens.
type UID struct {
	First  uint64
	Second uint64
}
