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
// TestCommitTransactionRequestRoundtrip traces through MarshalFDB output
// byte by byte to find where the marshal and reader disagree.
func TestCommitTransactionRequestRoundtrip(t *testing.T) {
	req := CommitTransactionRequest{
		Transaction: CommitTransactionRef{
			ReadSnapshot: 42,
			Mutations: []MutationRef{
				{MutType: 0, Param1: []byte("key1"), Param2: []byte("val1")},
			},
			WriteConflictRanges: []KeyRangeRef{
				{Begin: []byte("key1"), End: []byte("key1\x00")},
			},
		},
		Reply:      ReplyPromise{Token: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		TenantInfo: TenantInfo{TenantId: -1},
	}

	buf := req.MarshalFDB()
	t.Logf("Marshaled %d bytes", len(buf))
	// Print in 16-byte rows
	for i := 0; i < len(buf); i += 16 {
		end := i + 16
		if end > len(buf) {
			end = len(buf)
		}
		t.Logf("  %04x: %x", i, buf[i:end])
	}

	// Trace reader manually
	t.Logf("\n=== Reader trace ===")
	rootOff := binary.LittleEndian.Uint32(buf[0:4])
	fileID := binary.LittleEndian.Uint32(buf[4:8])
	t.Logf("Footer: rootOff=%d fileID=0x%x", rootOff, fileID)

	absRoot := int(rootOff)
	t.Logf("FakeRoot at buf[%d]", absRoot)
	if absRoot+8 > len(buf) {
		t.Fatalf("FakeRoot out of bounds: absRoot=%d bufLen=%d", absRoot, len(buf))
	}

	// FakeRoot vtable soffset
	frVTSoff := int32(binary.LittleEndian.Uint32(buf[absRoot : absRoot+4]))
	t.Logf("FakeRoot vtable soffset=%d → vtable at %d", frVTSoff, absRoot-int(frVTSoff))

	// FakeRoot field 0 at offset 4: relative offset to message object
	msgRelOff := binary.LittleEndian.Uint32(buf[absRoot+4 : absRoot+8])
	msgAbsPos := absRoot + 4 + int(msgRelOff)
	t.Logf("FakeRoot field0 (msgRelOff)=%d → message object at buf[%d]", msgRelOff, msgAbsPos)

	if msgAbsPos+4 > len(buf) {
		t.Fatalf("Message object out of bounds: pos=%d bufLen=%d", msgAbsPos, len(buf))
	}

	// Message object vtable
	msgVTSoff := int32(binary.LittleEndian.Uint32(buf[msgAbsPos : msgAbsPos+4]))
	msgVTPos := msgAbsPos - int(msgVTSoff)
	t.Logf("MsgObj vtable soffset=%d → vtable at buf[%d]", msgVTSoff, msgVTPos)

	if msgVTPos < 0 || msgVTPos+4 > len(buf) {
		t.Fatalf("Message vtable out of bounds: pos=%d", msgVTPos)
	}

	vtSize := binary.LittleEndian.Uint16(buf[msgVTPos : msgVTPos+2])
	objSize := binary.LittleEndian.Uint16(buf[msgVTPos+2 : msgVTPos+4])
	t.Logf("MsgObj vtable: vtSize=%d objSize=%d", vtSize, objSize)

	// Print all vtable entries
	nFields := (int(vtSize) - 4) / 2
	t.Logf("MsgObj vtable has %d field offsets:", nFields)
	for i := 0; i < nFields; i++ {
		off := binary.LittleEndian.Uint16(buf[msgVTPos+4+i*2 : msgVTPos+4+i*2+2])
		t.Logf("  field[%d] = offset %d", i, off)
	}

	// Try to read ReadSnapshot (slot 3 → field index 3)
	// CommitTransactionRef is nested at slot 0 of CommitTransactionRequest
	txnFieldOff := binary.LittleEndian.Uint16(buf[msgVTPos+4 : msgVTPos+6]) // field 0
	t.Logf("Transaction field offset (slot 0) = %d", txnFieldOff)

	if txnFieldOff > 0 {
		txnRelOff := binary.LittleEndian.Uint32(buf[msgAbsPos+int(txnFieldOff) : msgAbsPos+int(txnFieldOff)+4])
		txnAbsPos := msgAbsPos + int(txnFieldOff) + int(txnRelOff)
		t.Logf("Transaction relOff=%d → abs pos=%d", txnRelOff, txnAbsPos)

		if txnAbsPos+4 <= len(buf) {
			txnVTSoff := int32(binary.LittleEndian.Uint32(buf[txnAbsPos : txnAbsPos+4]))
			txnVTPos := txnAbsPos - int(txnVTSoff)
			t.Logf("Transaction vtable soffset=%d → vtable at buf[%d]", txnVTSoff, txnVTPos)
			if txnVTPos >= 0 && txnVTPos+4 <= len(buf) {
				txnVTSize := binary.LittleEndian.Uint16(buf[txnVTPos : txnVTPos+2])
				txnObjSize := binary.LittleEndian.Uint16(buf[txnVTPos+2 : txnVTPos+4])
				t.Logf("Transaction vtable: vtSize=%d objSize=%d", txnVTSize, txnObjSize)
				txnNFields := (int(txnVTSize) - 4) / 2
				for i := 0; i < txnNFields; i++ {
					off := binary.LittleEndian.Uint16(buf[txnVTPos+4+i*2 : txnVTPos+4+i*2+2])
					t.Logf("  field[%d] = offset %d", i, off)
				}
			}
		}
	}

	// Now try the actual unmarshal
	var decoded CommitTransactionRequest
	if err := decoded.UnmarshalFDB(buf); err != nil {
		t.Logf("UnmarshalFDB error: %v", err)
	} else {
		t.Logf("Decoded: ReadSnapshot=%d Mutations=%d TenantId=%d",
			decoded.Transaction.ReadSnapshot,
			len(decoded.Transaction.Mutations),
			decoded.TenantInfo.TenantId)
	}
}

// TestCrashFrame92Candidates tests the 3 candidate mutations from the
// debug log that could have been frame #92 (200 bytes, crashes FDB).
func TestCrashFrame92Candidates(t *testing.T) {
	candidates := []struct {
		name    string
		mutType uint8
		keyLen  int
		valLen  int
	}{
		{"ClearRange 303/304", 1, 303, 304},
		{"ClearRange 537/538", 1, 537, 538},
		{"SET 47/63", 0, 47, 63},
	}

	for _, c := range candidates {
		t.Run(c.name, func(t *testing.T) {
			req := CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 15000, // approximate read version
					Mutations: []MutationRef{
						{MutType: c.mutType, Param1: make([]byte, c.keyLen), Param2: make([]byte, c.valLen)},
					},
					WriteConflictRanges: []KeyRangeRef{
						{Begin: make([]byte, c.keyLen), End: make([]byte, c.valLen)},
					},
				},
				Reply:      ReplyPromise{Token: [16]byte{0xaa, 0xbb, 0xcc}},
				TenantInfo: TenantInfo{TenantId: -1},
			}
			buf := req.MarshalFDB()
			t.Logf("marshaled %d bytes (target: 200)", len(buf))

			// Round-trip
			var decoded CommitTransactionRequest
			if err := decoded.UnmarshalFDB(buf); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if decoded.Transaction.ReadSnapshot != req.Transaction.ReadSnapshot {
				t.Errorf("ReadSnapshot: got %d want %d", decoded.Transaction.ReadSnapshot, req.Transaction.ReadSnapshot)
			}
			if len(decoded.Transaction.Mutations) != 1 {
				t.Errorf("Mutations: got %d want 1", len(decoded.Transaction.Mutations))
			}
		})
	}
}

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
		{
			name: "3 system key mutations (tenant CRUD repro)",
			req: CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 70000,
					Mutations: []MutationRef{
						{MutType: 0, Param1: []byte("\xff/tenant/lastId"), Param2: []byte{3, 0, 0, 0, 0, 0, 0, 0}},
						{MutType: 0, Param1: []byte("\xff/tenant/map/\x1c\x00\x00\x00\x00\x00\x00\x00\x03"), Param2: []byte("test")},
						{MutType: 0, Param1: []byte("\xff/tenant/nameIndex/test-tenant-crud"), Param2: []byte{3, 0, 0, 0, 0, 0, 0, 0}},
					},
				},
				Flags:      1, // FLAG_IS_LOCK_AWARE
				Reply:      ReplyPromise{Token: [16]byte{0xde, 0xad}},
				TenantInfo: TenantInfo{TenantId: -1},
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
