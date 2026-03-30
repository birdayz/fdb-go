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
	json.Unmarshal(data, &v)
	raw, _ := hex.DecodeString(v.Hex)
	return raw
}

func TestUnmarshal_GroundTruth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  interface{ UnmarshalFDB([]byte) error }
	}{
		{"GetValueRequest", &GetValueRequest{}},
		{"GetValueReply", &GetValueReply{}},
		{"GetKeyRequest", &GetKeyRequest{}},
		{"GetKeyReply", &GetKeyReply{}},
		{"GetKeyValuesRequest", &GetKeyValuesRequest{}},
		{"GetKeyValuesReply", &GetKeyValuesReply{}},
		{"WatchValueRequest", &WatchValueRequest{}},
		{"WatchValueReply", &WatchValueReply{}},
		{"CommitTransactionRequest", &CommitTransactionRequest{}},
		{"CommitID", &CommitID{}},
		{"GetKeyServerLocationsRequest", &GetKeyServerLocationsRequest{}},
		{"GetKeyServerLocationsReply", &GetKeyServerLocationsReply{}},
		{"GetReadVersionRequest", &GetReadVersionRequest{}},
		{"GetReadVersionReply", &GetReadVersionReply{}},
		{"ClientDBInfo", &ClientDBInfo{}},
		{"OpenDatabaseCoordRequest", &OpenDatabaseCoordRequest{}},
		{"Error", &Error{}},
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
	t.Run("GetValueReply_WithPenalty", func(t *testing.T) {
		t.Parallel()
		msg := GetValueReply{Penalty: 1.5, Cached: true}
		data := msg.MarshalFDB()
		var msg2 GetValueReply
		if err := msg2.UnmarshalFDB(data); err != nil {
			t.Fatalf("UnmarshalFDB: %v", err)
		}
		if msg2.Penalty != 1.5 {
			t.Errorf("penalty: got %f, want 1.5", msg2.Penalty)
		}
		if !msg2.Cached {
			t.Error("cached: got false, want true")
		}
	})
}
