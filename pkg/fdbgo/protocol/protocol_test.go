package protocol

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type testVector struct {
	Name           string `json:"name"`
	FileIdentifier uint32 `json:"file_identifier"`
	Size           int    `json:"size"`
	Hex            string `json:"hex"`
}

func loadVector(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../wire/testdata/" + name + ".json")
	if err != nil {
		t.Skipf("no test vector for %s", name)
		return nil
	}
	var v testVector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parse test vector: %v", err)
	}
	raw, _ := hex.DecodeString(v.Hex)
	return raw
}

// TestUnmarshal_GroundTruth verifies every generated message type can
// unmarshal its C++ ground-truth test vector.
func TestUnmarshal_GroundTruth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  interface{ UnmarshalFDB([]byte) error }
		fid  uint32
	}{
		{"GetValueRequest", &GetValueRequest{}, GetValueRequest_FileIdentifier},
		{"GetValueReply", &GetValueReply{}, GetValueReply_FileIdentifier},
		{"GetKeyRequest", &GetKeyRequest{}, GetKeyRequest_FileIdentifier},
		{"GetKeyReply", &GetKeyReply{}, GetKeyReply_FileIdentifier},
		{"GetKeyValuesRequest", &GetKeyValuesRequest{}, GetKeyValuesRequest_FileIdentifier},
		{"GetKeyValuesReply", &GetKeyValuesReply{}, GetKeyValuesReply_FileIdentifier},
		{"WatchValueRequest", &WatchValueRequest{}, WatchValueRequest_FileIdentifier},
		{"WatchValueReply", &WatchValueReply{}, WatchValueReply_FileIdentifier},
		{"CommitTransactionRequest", &CommitTransactionRequest{}, CommitTransactionRequest_FileIdentifier},
		{"CommitID", &CommitID{}, CommitID_FileIdentifier},
		{"GetKeyServerLocationsRequest", &GetKeyServerLocationsRequest{}, GetKeyServerLocationsRequest_FileIdentifier},
		{"GetKeyServerLocationsReply", &GetKeyServerLocationsReply{}, GetKeyServerLocationsReply_FileIdentifier},
		{"GetReadVersionRequest", &GetReadVersionRequest{}, GetReadVersionRequest_FileIdentifier},
		{"GetReadVersionReply", &GetReadVersionReply{}, GetReadVersionReply_FileIdentifier},
		{"ClientDBInfo", &ClientDBInfo{}, ClientDBInfo_FileIdentifier},
		{"OpenDatabaseCoordRequest", &OpenDatabaseCoordRequest{}, OpenDatabaseCoordRequest_FileIdentifier},
		{"Error", &Error{}, Error_FileIdentifier},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := loadVector(t, tt.name)
			if raw == nil {
				return
			}
			if err := tt.msg.UnmarshalFDB(raw); err != nil {
				t.Fatalf("UnmarshalFDB: %v", err)
			}
		})
	}
}

// TestRoundTrip verifies Marshal → Unmarshal preserves field values.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("WatchValueReply", func(t *testing.T) {
		t.Parallel()
		msg := WatchValueReply{Version: 12345, Cached: true}
		data := msg.MarshalFDB()
		var msg2 WatchValueReply
		if err := msg2.UnmarshalFDB(data); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if msg2.Version != 12345 {
			t.Errorf("version: got %d, want 12345", msg2.Version)
		}
		if !msg2.Cached {
			t.Error("cached: got false, want true")
		}
	})

	t.Run("GetValueRequest", func(t *testing.T) {
		t.Parallel()
		msg := GetValueRequest{Key: []byte("mykey"), Version: 100}
		data := msg.MarshalFDB()
		var msg2 GetValueRequest
		if err := msg2.UnmarshalFDB(data); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if string(msg2.Key) != "mykey" {
			t.Errorf("key: got %q, want %q", msg2.Key, "mykey")
		}
		if msg2.Version != 100 {
			t.Errorf("version: got %d, want 100", msg2.Version)
		}
	})

	t.Run("CommitID", func(t *testing.T) {
		t.Parallel()
		msg := CommitID{Version: 999, TxnBatchId: 42}
		data := msg.MarshalFDB()
		var msg2 CommitID
		if err := msg2.UnmarshalFDB(data); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if msg2.Version != 999 {
			t.Errorf("version: got %d, want 999", msg2.Version)
		}
		if msg2.TxnBatchId != 42 {
			t.Errorf("txnBatchId: got %d, want 42", msg2.TxnBatchId)
		}
	})
}
