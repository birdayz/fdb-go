package types

import (
	"encoding/binary"
	"testing"
)

// TestCommitTransactionRequestMarshalFooter verifies that MarshalFDB produces
// valid FDB FlatBuffers footers for various CommitTransactionRequest inputs.
//
// Background: binding tester seed 6 crashes FDB because our MarshalFDB
// produces a 200-byte message with footer fileID=0x2 rootOffset=65535
// instead of the correct fileID=93948. This is a buffer size calculation bug.
func TestCommitTransactionRequestMarshalFooter(t *testing.T) {
	cases := []struct {
		name string
		req  CommitTransactionRequest
	}{
		{
			name: "empty transaction",
			req: CommitTransactionRequest{
				TenantInfo: TenantInfo{TenantId: -1},
			},
		},
		{
			name: "empty transaction with read version",
			req: CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 12345678,
				},
				TenantInfo: TenantInfo{TenantId: -1},
			},
		},
		{
			name: "single SET mutation",
			req: CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 12345678,
					Mutations: []MutationRef{
						{MutType: 0, Param1: []byte("key"), Param2: []byte("value")},
					},
					WriteConflictRanges: []KeyRangeRef{
						{Begin: []byte("key"), End: []byte("key\x00")},
					},
				},
				Reply:      ReplyPromise{Token: [16]byte{0x12, 0x34}},
				TenantInfo: TenantInfo{TenantId: -1},
			},
		},
		{
			name: "single ClearRange mutation",
			req: CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 12345678,
					Mutations: []MutationRef{
						{MutType: 1, Param1: make([]byte, 303), Param2: make([]byte, 304)},
					},
					WriteConflictRanges: []KeyRangeRef{
						{Begin: make([]byte, 303), End: make([]byte, 304)},
					},
				},
				Reply:      ReplyPromise{Token: [16]byte{0xaa, 0xbb}},
				TenantInfo: TenantInfo{TenantId: -1},
			},
		},
		{
			name: "100 SET mutations (preload batch)",
			req: func() CommitTransactionRequest {
				var muts []MutationRef
				var wcs []KeyRangeRef
				for i := 0; i < 100; i++ {
					key := make([]byte, 30+i%20)
					val := make([]byte, 10+i%50)
					muts = append(muts, MutationRef{MutType: 0, Param1: key, Param2: val})
					wcs = append(wcs, KeyRangeRef{Begin: key, End: append(append([]byte{}, key...), 0)})
				}
				return CommitTransactionRequest{
					Transaction: CommitTransactionRef{
						ReadSnapshot:        12345678,
						Mutations:           muts,
						WriteConflictRanges: wcs,
					},
					Reply:      ReplyPromise{Token: [16]byte{0x11, 0x22}},
					TenantInfo: TenantInfo{TenantId: -1},
				}
			}(),
		},
		{
			name: "write-only (no read conflicts, no read version)",
			req: CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 0, // This is the crashing case!
					Mutations: []MutationRef{
						{MutType: 0, Param1: []byte("key1"), Param2: []byte("value1")},
					},
					WriteConflictRanges: []KeyRangeRef{
						{Begin: []byte("key1"), End: []byte("key1\x00")},
					},
				},
				Reply:      ReplyPromise{Token: [16]byte{0xde, 0xad}},
				TenantInfo: TenantInfo{TenantId: -1},
			},
		},
		{
			name: "tenant ID 0 (default, not -1)",
			req: CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 99999,
					Mutations: []MutationRef{
						{MutType: 0, Param1: []byte("k"), Param2: []byte("v")},
					},
				},
				Reply:      ReplyPromise{Token: [16]byte{0x01, 0x02}},
				TenantInfo: TenantInfo{TenantId: 0},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := tc.req.MarshalFDB()
			t.Logf("marshaled %d bytes", len(buf))

			if len(buf) < 8 {
				t.Fatalf("buffer too small: %d bytes", len(buf))
			}

			// Footer is the FIRST 8 bytes: [4: offset to fake root] [4: file ID]
			fakeRootOff := binary.LittleEndian.Uint32(buf[0:4])
			fileID := binary.LittleEndian.Uint32(buf[4:8])

			t.Logf("footer: fileID=0x%x (%d) fakeRootOff=%d", fileID, fileID, fakeRootOff)
			t.Logf("first 16 bytes: %x", buf[:16])
			t.Logf("last  16 bytes: %x", buf[len(buf)-16:])

			// Verify file ID matches CommitTransactionRequest
			if fileID != CommitTransactionRequestFileID {
				t.Errorf("WRONG fileID: got 0x%x (%d), want 0x%x (%d)",
					fileID, fileID, CommitTransactionRequestFileID, CommitTransactionRequestFileID)
			}

			// fakeRootOff should point to a valid position within the buffer
			if int(fakeRootOff) >= len(buf) {
				t.Errorf("fakeRootOff %d points outside buffer (len=%d)", fakeRootOff, len(buf))
			}

			// Round-trip: unmarshal and check fields match
			var decoded CommitTransactionRequest
			if err := decoded.UnmarshalFDB(buf); err != nil {
				t.Errorf("round-trip unmarshal failed: %v", err)
				return
			}

			if decoded.Transaction.ReadSnapshot != tc.req.Transaction.ReadSnapshot {
				t.Errorf("ReadSnapshot mismatch: got %d, want %d",
					decoded.Transaction.ReadSnapshot, tc.req.Transaction.ReadSnapshot)
			}
			if len(decoded.Transaction.Mutations) != len(tc.req.Transaction.Mutations) {
				t.Errorf("Mutations count mismatch: got %d, want %d",
					len(decoded.Transaction.Mutations), len(tc.req.Transaction.Mutations))
			}
			if decoded.TenantInfo.TenantId != tc.req.TenantInfo.TenantId {
				t.Errorf("TenantId mismatch: got %d, want %d",
					decoded.TenantInfo.TenantId, tc.req.TenantInfo.TenantId)
			}
		})
	}
}
