package client

import (
	"encoding/binary"
	"io"
	"os"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
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
			tokenLo := binary.LittleEndian.Uint64(hdr[17:25])
			endpoint := tokenLo & 0xFFFFFFFF
			if endpoint == 0x9c { // commit proxy
				t.Logf("#%04d COMMIT len=%d", idx, bodyLen)
				var req types.CommitTransactionRequest
				if err := req.UnmarshalFDB(body); err != nil {
					t.Logf("  unmarshal error: %v", err)
				} else {
					t.Logf("  ReadSnapshot=%d Mutations=%d ReadCR=%d WriteCR=%d TenantId=%d",
						req.Transaction.ReadSnapshot,
						len(req.Transaction.Mutations),
						len(req.Transaction.ReadConflictRanges),
						len(req.Transaction.WriteConflictRanges),
						req.TenantInfo.TenantId)
					for i, m := range req.Transaction.Mutations {
						if i < 5 {
							t.Logf("    mut[%d] type=%d keyLen=%d valLen=%d", i, m.MutType, len(m.Param1), len(m.Param2))
						}
					}
				}
			}
		}
		idx++
	}
}
