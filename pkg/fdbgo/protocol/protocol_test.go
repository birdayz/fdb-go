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
	json.Unmarshal(data, &v)
	raw, _ := hex.DecodeString(v.Hex)
	return raw
}

func TestUnmarshal_WatchValueReply(t *testing.T) {
	t.Parallel()
	raw := loadVector(t, "WatchValueReply")
	if raw == nil {
		return
	}

	var msg WatchValueReply
	if err := msg.UnmarshalFDB(raw); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	// Default-constructed: version=-1 (FDB sentinel), cached=false
	t.Logf("WatchValueReply: version=%d, cached=%v", msg.Version, msg.Cached)
}

func TestUnmarshal_GetValueReply(t *testing.T) {
	t.Parallel()
	raw := loadVector(t, "GetValueReply")
	if raw == nil {
		return
	}

	var msg GetValueReply
	if err := msg.UnmarshalFDB(raw); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	t.Logf("GetValueReply: penalty=%v, cached=%v", msg.Penalty, msg.Cached)
}

func TestUnmarshal_CommitID(t *testing.T) {
	t.Parallel()
	raw := loadVector(t, "CommitID")
	if raw == nil {
		return
	}

	var msg CommitID
	if err := msg.UnmarshalFDB(raw); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	t.Logf("CommitID: version=%d, txnBatchId=%d", msg.Version, msg.TxnBatchId)
}

func TestRoundTrip_WatchValueReply(t *testing.T) {
	t.Parallel()

	// Marshal with known values.
	msg := WatchValueReply{
		Version: 12345,
		Cached:  true,
	}
	data := msg.MarshalFDB()

	// Unmarshal back.
	var msg2 WatchValueReply
	if err := msg2.UnmarshalFDB(data); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if msg2.Version != 12345 {
		t.Errorf("version: got %d, want 12345", msg2.Version)
	}
	if msg2.Cached != true {
		t.Errorf("cached: got %v, want true", msg2.Cached)
	}
}
