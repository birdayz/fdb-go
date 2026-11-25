package types

import (
	"encoding/binary"
	"testing"
)

// TestGetKeyServerLocationsRequestRoundtrip verifies the marshal/unmarshal
// round-trip for GetKeyServerLocationsRequest. This is the message type
// that crashes FDB in binding tester seed 6 (frame #92, fileID=0x8b8968).
func TestGetKeyServerLocationsRequestRoundtrip(t *testing.T) {
	cases := []struct {
		name  string
		begin []byte
		limit int32
	}{
		{"short key", []byte("hello"), 100},
		{"empty key", []byte{}, 100},
		{"nil key", nil, 100},
		{"long key 256", make([]byte, 256), 100},
		{"long key 1024", make([]byte, 1024), 100},
		{"limit 1", []byte("x"), 1},
		{"limit 0", []byte("x"), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := GetKeyServerLocationsRequest{
				Begin:            tc.begin,
				Limit:            tc.limit,
				Reply:            ReplyPromise{Token: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
				Tenant:           TenantInfo{TenantId: -1},
				MinTenantVersion: -1,
			}

			buf := req.MarshalFDB()
			t.Logf("marshaled %d bytes", len(buf))

			// Check footer
			rootOff := binary.LittleEndian.Uint32(buf[0:4])
			fileID := binary.LittleEndian.Uint32(buf[4:8])
			t.Logf("rootOff=%d fileID=0x%x (want 0x%x)", rootOff, fileID, GetKeyServerLocationsRequestFileID)

			if fileID != GetKeyServerLocationsRequestFileID {
				t.Errorf("WRONG fileID: got 0x%x, want 0x%x", fileID, GetKeyServerLocationsRequestFileID)
			}

			// Round-trip unmarshal
			var decoded GetKeyServerLocationsRequest
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("UnmarshalFDB PANICKED: %v\nbuf hex: %x", r, buf)
					}
				}()
				if err := decoded.UnmarshalFDB(buf); err != nil {
					t.Fatalf("UnmarshalFDB error: %v", err)
				}
			}()

			if string(decoded.Begin) != string(tc.begin) {
				t.Errorf("Begin mismatch: got %q, want %q", decoded.Begin, tc.begin)
			}
			if decoded.Limit != tc.limit {
				t.Errorf("Limit mismatch: got %d, want %d", decoded.Limit, tc.limit)
			}
			if decoded.Tenant.TenantId != -1 {
				t.Errorf("TenantId mismatch: got %d, want -1", decoded.Tenant.TenantId)
			}
			if decoded.MinTenantVersion != -1 {
				t.Errorf("MinTenantVersion mismatch: got %d, want -1", decoded.MinTenantVersion)
			}
		})
	}
}
