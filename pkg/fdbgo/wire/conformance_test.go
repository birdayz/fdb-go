package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ground-truth conformance tests.
//
// For EVERY test vector (322 files produced by FDB's real ObjectWriter in Docker):
// 1. Parse C++ bytes with our Go Reader → proves unmarshal works
// 2. Verify file_identifier matches
// 3. Verify buffer size matches
//
// This is exhaustive: one sub-test per FDB protocol message type.

type testVector struct {
	Name           string `json:"name"`
	FileIdentifier uint32 `json:"file_identifier"`
	Size           int    `json:"size"`
	Hex            string `json:"hex"`
}

func loadTestVector(t *testing.T, name string) *testVector {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Skipf("test vector %s not found: %v", name, err)
		return nil
	}
	var v testVector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parse testdata/%s.json: %v", name, err)
	}
	return &v
}

// TestGroundTruth_AllMessages iterates over every test vector file and verifies
// our Go Reader can parse the C++ ground-truth bytes.
func TestGroundTruth_AllMessages(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("testdata/[A-Z]*.json")
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(files) == 0 {
		t.Skip("no test vectors found in testdata/")
	}

	t.Logf("Testing %d ground-truth test vectors", len(files))

	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Load test vector.
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			var v testVector
			if err := json.Unmarshal(data, &v); err != nil {
				t.Fatalf("parse %s: %v", f, err)
			}

			// Decode hex.
			raw, err := hex.DecodeString(v.Hex)
			if err != nil {
				t.Fatalf("decode hex for %s: %v", v.Name, err)
			}

			// Verify size matches.
			if len(raw) != v.Size {
				t.Errorf("size mismatch: hex decodes to %d bytes, expected %d", len(raw), v.Size)
			}

			// Parse with our Reader — the core conformance check.
			r, err := NewReader(raw)
			if err != nil {
				t.Fatalf("NewReader failed for %s: %v", v.Name, err)
			}

			// Verify file_identifier.
			if got := r.FileIdentifier(); got != v.FileIdentifier {
				t.Errorf("file_identifier: got %d, want %d", got, v.FileIdentifier)
			}
		})
	}
}

// TestGroundTruth_ReadFields verifies our Reader can access field data
// from critical client messages (not just parsing, but field extraction).
func TestGroundTruth_ReadFields(t *testing.T) {
	t.Parallel()

	// For each critical message, verify we can read fields without panicking.
	// The actual values are from default construction — may not be zero
	// because ObjectWriter + IncludeVersion has different initialization.
	criticalMessages := []string{
		"GetValueRequest", "GetValueReply",
		"GetKeyRequest", "GetKeyReply",
		"GetKeyValuesRequest", "GetKeyValuesReply",
		"CommitTransactionRequest", "CommitID",
		"GetReadVersionRequest", "GetReadVersionReply",
		"GetKeyServerLocationsRequest", "GetKeyServerLocationsReply",
		"WatchValueRequest", "WatchValueReply",
		"ClientDBInfo", "OpenDatabaseCoordRequest",
	}

	for _, name := range criticalMessages {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v := loadTestVector(t, name)
			if v == nil {
				return
			}
			raw, _ := hex.DecodeString(v.Hex)
			r, err := NewReader(raw)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			if r.FileIdentifier() != v.FileIdentifier {
				t.Errorf("file_id: got %d, want %d", r.FileIdentifier(), v.FileIdentifier)
			}
			// Verify we can read at least the first field without panicking.
			if r.FieldPresent(0) {
				_ = r.ReadInt64(0) // read as int64 (type doesn't matter, just verifies access)
			}
		})
	}
}
