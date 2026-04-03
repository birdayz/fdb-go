package types

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type testVectorEntry struct {
	Name   string `json:"name"`
	FileID uint32 `json:"file_id"`
	Size   int    `json:"size"`
	Hex    string `json:"hex"`
}

func loadTestVectors(t *testing.T) []testVectorEntry {
	t.Helper()
	data, err := os.ReadFile("testdata.json")
	if err != nil {
		t.Skipf("testdata.json not found: %v", err)
	}
	var vecs []testVectorEntry
	if err := json.Unmarshal(data, &vecs); err != nil {
		t.Fatalf("parse testdata.json: %v", err)
	}
	return vecs
}

// TestGroundTruthMarshal compares our Go MarshalFDB output against C++
// ObjectWriter ground-truth bytes for every critical message type.
// If this test fails, our FlatBuffers layout diverges from C++ and
// the FDB server will crash or misbehave.
func TestGroundTruthMarshal(t *testing.T) {
	vecs := loadTestVectors(t)

	builders := map[string]func() []byte{
		"GetKeyServerLocationsRequest_basic": func() []byte {
			return (&GetKeyServerLocationsRequest{
				Begin:            []byte("test_key"),
				Limit:            100,
				Reverse:          false,
				Reply:            ReplyPromise{}, // zero token
				Tenant:           TenantInfo{TenantId: -1},
				MinTenantVersion: -1,
			}).MarshalFDB()
		},
		"GetKeyServerLocationsRequest_with_end": func() []byte {
			return (&GetKeyServerLocationsRequest{
				Begin:            []byte("a_longer_test_key_with_more_bytes"),
				HasEnd:           true,
				End:              []byte("end_key"),
				Limit:            42,
				Reverse:          true,
				Reply:            ReplyPromise{},
				Tenant:           TenantInfo{TenantId: -1},
				MinTenantVersion: -1,
			}).MarshalFDB()
		},
		"GetKeyServerLocationsRequest_empty": func() []byte {
			return (&GetKeyServerLocationsRequest{
				Begin:            []byte{},
				Limit:            0,
				Reply:            ReplyPromise{},
				Tenant:           TenantInfo{TenantId: 0},
				MinTenantVersion: 0,
			}).MarshalFDB()
		},
		"GetValueRequest_basic": func() []byte {
			// C++ VersionVector default encodes as 16 bytes:
			// [utlCount=0 (8 bytes LE)][maxVersion=invalidVersion=-1 (8 bytes LE)]
			emptyVV := make([]byte, 16)
			emptyVV[8] = 0xFF; emptyVV[9] = 0xFF; emptyVV[10] = 0xFF; emptyVV[11] = 0xFF
			emptyVV[12] = 0xFF; emptyVV[13] = 0xFF; emptyVV[14] = 0xFF; emptyVV[15] = 0xFF
			return (&GetValueRequest{
				Key:                    []byte("my_key"),
				Version:               12345678,
				Reply:                  ReplyPromise{},
				TenantInfo:            TenantInfo{TenantId: -1},
				SsLatestCommitVersions: emptyVV,
			}).MarshalFDB()
		},
		"GetKeyRequest_basic": func() []byte {
			return (&GetKeyRequest{
				Sel: KeySelectorRef{
					Key:     []byte("selector_key"),
					OrEqual: true,
					Offset:  1,
				},
				Version:               99999,
				Reply:      ReplyPromise{},
				TenantInfo: TenantInfo{TenantId: -1},
			}).MarshalFDB()
		},
		"GetKeyValuesRequest_basic": func() []byte {
			emptyVV := make([]byte, 16)
			emptyVV[8] = 0xFF; emptyVV[9] = 0xFF; emptyVV[10] = 0xFF; emptyVV[11] = 0xFF
			emptyVV[12] = 0xFF; emptyVV[13] = 0xFF; emptyVV[14] = 0xFF; emptyVV[15] = 0xFF
			return (&GetKeyValuesRequest{
				Begin: KeySelectorRef{
					Key:     []byte("begin_key"),
					OrEqual: true,
					Offset:  1,
				},
				End: KeySelectorRef{
					Key:     []byte("end_key"),
					OrEqual: false,
					Offset:  0,
				},
				Version:               54321,
				Limit:                 1000,
				LimitBytes:            0x7fffffff,
				Reply:                  ReplyPromise{},
				TenantInfo:            TenantInfo{TenantId: -1},
				SsLatestCommitVersions: emptyVV,
			}).MarshalFDB()
		},
		"CommitTransactionRequest_single_set": func() []byte {
			return (&CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 42,
					Mutations: []MutationRef{
						{MutType: 0, Param1: []byte("key1"), Param2: []byte("val1")},
					},
					WriteConflictRanges: []KeyRangeRef{
						{Begin: []byte("key1"), End: []byte("key1\x00")},
					},
					ReadConflictRanges: []KeyRangeRef{
						{Begin: []byte("key1"), End: []byte("key1\x00")},
					},
				},
				Reply:      ReplyPromise{},
				TenantInfo: TenantInfo{TenantId: -1},
			}).MarshalFDB()
		},
		"CommitTransactionRequest_three_sets": func() []byte {
			var muts []MutationRef
			var wcs []KeyRangeRef
			for i := 0; i < 3; i++ {
				key := []byte("key_" + string(rune('0'+i)))
				val := []byte("val_" + string(rune('0'+i)))
				muts = append(muts, MutationRef{MutType: 0, Param1: key, Param2: val})
				wcs = append(wcs, KeyRangeRef{Begin: key, End: append(append([]byte{}, key...), 0)})
			}
			return (&CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot:        99999,
					Mutations:           muts,
					WriteConflictRanges: wcs,
				},
				Reply:      ReplyPromise{},
				TenantInfo: TenantInfo{TenantId: -1},
			}).MarshalFDB()
		},
		"CommitTransactionRequest_empty": func() []byte {
			return (&CommitTransactionRequest{
				Transaction: CommitTransactionRef{
					ReadSnapshot: 0,
				},
				Reply:      ReplyPromise{},
				TenantInfo: TenantInfo{TenantId: -1},
			}).MarshalFDB()
		},
		"GetReadVersionRequest_causal_risky": func() []byte {
			return (&GetReadVersionRequest{
				Flags:            1, // FLAG_CAUSAL_READ_RISKY
				TransactionCount: 1,
				MaxVersion:       -1, // invalidVersion (C++ default)
				Reply:            ReplyPromise{},
			}).MarshalFDB()
		},
	}

	for _, vec := range vecs {
		t.Run(vec.Name, func(t *testing.T) {
			buildFn, ok := builders[vec.Name]
			if !ok {
				t.Skipf("no Go builder for %s", vec.Name)
				return
			}

			goBytes := buildFn()
			cppBytes, err := hex.DecodeString(vec.Hex)
			if err != nil {
				t.Fatalf("decode hex: %v", err)
			}

			// The C++ output includes a protocol version prefix (first 8 bytes).
			// Strip it if present: bytes [6]=0xDB, [7]=0x0F.
			if len(cppBytes) >= 8 && cppBytes[7] == 0x0F && cppBytes[6] == 0xDB {
				cppBytes = cppBytes[8:]
			}

			t.Logf("Go:  %d bytes", len(goBytes))
			t.Logf("C++: %d bytes (after stripping version prefix)", len(cppBytes))

			if len(goBytes) != len(cppBytes) {
				t.Errorf("SIZE MISMATCH: Go=%d C++=%d", len(goBytes), len(cppBytes))
				// Find first divergent byte
				minLen := len(goBytes)
				if len(cppBytes) < minLen {
					minLen = len(cppBytes)
				}
				for i := 0; i < minLen; i++ {
					if goBytes[i] != cppBytes[i] {
						t.Logf("First divergence at byte %d: Go=0x%02x C++=0x%02x", i, goBytes[i], cppBytes[i])
						break
					}
				}
			} else {
				divergences := 0
				for i := 0; i < len(goBytes); i++ {
					if goBytes[i] != cppBytes[i] {
						if divergences < 5 {
							t.Errorf("BYTE MISMATCH at offset %d: Go=0x%02x C++=0x%02x", i, goBytes[i], cppBytes[i])
						}
						divergences++
					}
				}
				if divergences > 5 {
					t.Errorf("... and %d more byte mismatches", divergences-5)
				}
				if divergences == 0 {
					t.Logf("BYTE-IDENTICAL with C++ (%d bytes)", len(goBytes))
				}
			}

			if t.Failed() {
				// Dump both for manual comparison
				t.Logf("Go  hex: %s", hex.EncodeToString(goBytes))
				t.Logf("C++ hex: %s", hex.EncodeToString(cppBytes))
			}
		})
	}
}
