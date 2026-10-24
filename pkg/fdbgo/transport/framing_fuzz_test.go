package transport

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/zeebo/xxh3"
)

// FuzzFrameReader: Read never panics, returns only a frame of the declared length with a
// valid checksum, and reads a frame WriteFrame built back unchanged.
func FuzzFrameReader(f *testing.F) {
	frame := func(body []byte, tls bool) []byte {
		var b bytes.Buffer
		if err := WriteFrame(&b, UID{First: 1, Second: 2}, body, tls); err != nil {
			f.Fatal(err)
		}
		return b.Bytes()
	}
	f.Add([]byte{}, false)
	f.Add(frame(nil, false), false)
	f.Add(frame([]byte("body"), false), false)
	f.Add(frame([]byte("body"), true), true)
	f.Add(binary.LittleEndian.AppendUint32(nil, maxPayloadSize+1), true)
	f.Add(binary.LittleEndian.AppendUint32(nil, minPayloadSize-1), false)
	f.Fuzz(func(t *testing.T, data []byte, tls bool) {
		var fr FrameReader
		token, body, err := fr.Read(bytes.NewReader(data), tls)
		if err == nil {
			header := packetLenWidth
			if !tls {
				header += checksumWidth
			}
			n := int(binary.LittleEndian.Uint32(data))
			if n < minPayloadSize || n > maxPayloadSize || len(body) != n-16 || len(data) < header+n {
				t.Fatalf("read a %d-byte body from a frame declaring %d", len(body), n)
			}
			payload := data[header : header+n]
			if !tls && xxh3.Hash(payload) != binary.LittleEndian.Uint64(data[packetLenWidth:]) {
				t.Fatal("read a frame whose checksum does not match")
			}
			if !bytes.Equal(body, payload[16:]) || token.First != binary.LittleEndian.Uint64(payload) ||
				token.Second != binary.LittleEndian.Uint64(payload[8:]) {
				t.Fatal("token or body differ from the frame's payload")
			}
		}

		want := UID{First: uint64(len(data)), Second: 7}
		var b bytes.Buffer
		if err := WriteFrame(&b, want, data, tls); err != nil {
			t.Fatal(err)
		}
		token, body, err = fr.Read(&b, tls)
		if err != nil || token != want || !bytes.Equal(body, data) {
			t.Fatalf("WriteFrame round trip: %v, %v, %d bytes", err, token, len(body))
		}
	})
}
