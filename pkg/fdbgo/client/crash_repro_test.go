package client

import (
	"encoding/binary"
	"io"
	"os"
	"testing"
	// "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// TestDecodeCrashFrame reads the wire log from the binding tester crash
// (seed 6) and decodes the CommitTransactionRequest that killed FDB.
// Run with: bazelisk test //pkg/fdbgo/client:client_test --test_arg="-test.run=TestDecodeCrashFrame" --test_output=streamed
func TestDecodeCrashFrame(t *testing.T) {
	const wirelogPath = "/tmp/go-seed6-v2.wirelog"
	f, err := os.Open(wirelogPath)
	if err != nil {
		t.Skipf("wire log not found at %s (run binding tester with FDB_WIRE_LOG first)", wirelogPath)
	}
	defer f.Close()

	idx := 0
	for {
		var hdr [29]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			break
		}
		bodyLen := binary.LittleEndian.Uint32(hdr[25:29])
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(f, body); err != nil {
			t.Fatalf("truncated frame %d", idx)
		}

		if hdr[0] == 'S' {
			tokenHi := binary.LittleEndian.Uint64(hdr[9:17])
			tokenLo := binary.LittleEndian.Uint64(hdr[17:25])
			// Endpoint is encoded in the low 32 bits of tokenLo
			endpoint := tokenLo & 0xFFFFFFFF
			t.Logf("#%04d SEND len=%5d token=%016x:%016x endpoint=0x%x",
				idx, bodyLen, tokenHi, tokenLo, endpoint)
		}
		idx++
	}
}
