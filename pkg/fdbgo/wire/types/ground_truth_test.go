package types

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

type testVectorEntry struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	FileID     uint32 `json:"file_id"`
	ReplyToken string `json:"reply_token"`
	Size       int    `json:"size"`
	Hex        string `json:"hex"`
}

func loadTestVectors(t *testing.T) []testVectorEntry {
	t.Helper()
	data, err := os.ReadFile("testdata.json")
	if err != nil {
		// testdata.json is a committed artifact declared as a bazel data dep
		// (types_test in BUILD.bazel). Absence is a build bug, never a skip.
		t.Fatalf("testdata.json not found (missing bazel data dep?): %v", err)
	}
	var vecs []testVectorEntry
	if err := json.Unmarshal(data, &vecs); err != nil {
		t.Fatalf("parse testdata.json: %v", err)
	}
	return vecs
}

// vectorBytes decodes the hex payload and, for request vectors, strips the
// 8-byte IncludeVersion protocol prefix (bytes [6]=0xDB, [7]=0x0F). Reply
// vectors are serialized with AssumeVersion and carry no prefix.
func vectorBytes(t *testing.T, vec testVectorEntry) []byte {
	t.Helper()
	raw, err := hex.DecodeString(vec.Hex)
	if err != nil {
		t.Fatalf("decode hex for %s: %v", vec.Name, err)
	}
	if len(raw) != vec.Size {
		t.Fatalf("%s: hex decodes to %d bytes, size field says %d", vec.Name, len(raw), vec.Size)
	}
	if vec.Kind == "request" {
		if len(raw) < 8 || raw[6] != 0xDB || raw[7] != 0x0F {
			t.Fatalf("%s: request vector missing IncludeVersion prefix", vec.Name)
		}
		raw = raw[8:]
	}
	return raw
}

// vectorReplyToken decodes the pinned reply-promise token, already in buffer
// order (LE bytes of UID first() then second() — scalar_traits<UID>).
func vectorReplyToken(t *testing.T, vec testVectorEntry) [16]byte {
	t.Helper()
	raw, err := hex.DecodeString(vec.ReplyToken)
	if err != nil || len(raw) != 16 {
		t.Fatalf("%s: bad reply_token %q", vec.Name, vec.ReplyToken)
	}
	var tok [16]byte
	copy(tok[:], raw)
	return tok
}

// requestBuilders construct each request EXACTLY as the C++ extractor did
// (cmd/fdb-schema-extract/main.cpp generateTestVectors), with the vector's
// pinned reply token. Output must be byte-identical to the C++ ObjectWriter.
var requestBuilders = map[string]func(tok [16]byte) []byte{
	"GetKeyServerLocationsRequest_basic": func(tok [16]byte) []byte {
		return (&GetKeyServerLocationsRequest{
			Begin:            []byte("test_key"),
			Limit:            100,
			Reverse:          false,
			Reply:            ReplyPromise{Token: tok},
			Tenant:           TenantInfo{TenantId: -1},
			MinTenantVersion: -1,
		}).MarshalFDB()
	},
	"GetKeyServerLocationsRequest_with_end": func(tok [16]byte) []byte {
		return (&GetKeyServerLocationsRequest{
			Begin:            []byte("a_longer_test_key_with_more_bytes"),
			HasEnd:           true,
			End:              []byte("end_key"),
			Limit:            42,
			Reverse:          true,
			Reply:            ReplyPromise{Token: tok},
			Tenant:           TenantInfo{TenantId: -1},
			MinTenantVersion: -1,
		}).MarshalFDB()
	},
	"GetKeyServerLocationsRequest_empty": func(tok [16]byte) []byte {
		return (&GetKeyServerLocationsRequest{
			Begin:            []byte{},
			Limit:            0,
			Reply:            ReplyPromise{Token: tok},
			Tenant:           TenantInfo{TenantId: 0},
			MinTenantVersion: 0,
		}).MarshalFDB()
	},
	"GetValueRequest_basic": func(tok [16]byte) []byte {
		return (&GetValueRequest{
			Key:                    []byte("my_key"),
			Version:                12345678,
			Reply:                  ReplyPromise{Token: tok},
			TenantInfo:             TenantInfo{TenantId: -1},
			SsLatestCommitVersions: emptyVersionVector(),
		}).MarshalFDB()
	},
	"GetKeyRequest_basic": func(tok [16]byte) []byte {
		return (&GetKeyRequest{
			Sel: KeySelectorRef{
				Key:     []byte("selector_key"),
				OrEqual: true,
				Offset:  1,
			},
			Version:                99999,
			Reply:                  ReplyPromise{Token: tok},
			TenantInfo:             TenantInfo{TenantId: -1},
			SsLatestCommitVersions: emptyVersionVector(),
		}).MarshalFDB()
	},
	"GetKeyValuesRequest_basic": func(tok [16]byte) []byte {
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
			Version:                54321,
			Limit:                  1000,
			LimitBytes:             0x7fffffff,
			Reply:                  ReplyPromise{Token: tok},
			TenantInfo:             TenantInfo{TenantId: -1},
			SsLatestCommitVersions: emptyVersionVector(),
		}).MarshalFDB()
	},
	// getMappedRange. Identical to GetKeyValuesRequest_basic except for Mapper, so
	// a byte-identical pass here proves the spliced mapper slot lands where
	// GetMappedKeyValuesRequest::serialize puts it (StorageServerInterface.h:484-496)
	// rather than at the tail where an append-style port would put it.
	"GetMappedKeyValuesRequest_basic": func(tok [16]byte) []byte {
		return (&GetMappedKeyValuesRequest{
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
			Mapper:                 []byte("\x01idx\x00{K[1]}"),
			Version:                54321,
			Limit:                  1000,
			LimitBytes:             0x7fffffff,
			Reply:                  ReplyPromise{Token: tok},
			TenantInfo:             TenantInfo{TenantId: -1},
			SsLatestCommitVersions: emptyVersionVector(),
		}).MarshalFDB()
	},
	"CommitTransactionRequest_single_set": func(tok [16]byte) []byte {
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
			Reply:      ReplyPromise{Token: tok},
			TenantInfo: TenantInfo{TenantId: -1},
		}).MarshalFDB()
	},
	"CommitTransactionRequest_three_sets": func(tok [16]byte) []byte {
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
			Reply:      ReplyPromise{Token: tok},
			TenantInfo: TenantInfo{TenantId: -1},
		}).MarshalFDB()
	},
	"CommitTransactionRequest_empty": func(tok [16]byte) []byte {
		return (&CommitTransactionRequest{
			Transaction: CommitTransactionRef{
				ReadSnapshot: 0,
			},
			Reply:      ReplyPromise{Token: tok},
			TenantInfo: TenantInfo{TenantId: -1},
		}).MarshalFDB()
	},
	"CommitTransactionRequest_3_system_keys": func(tok [16]byte) []byte {
		idVal := []byte{3, 0, 0, 0, 0, 0, 0, 0}
		return (&CommitTransactionRequest{
			Flags: 1, // FLAG_IS_LOCK_AWARE
			Transaction: CommitTransactionRef{
				ReadSnapshot: 70000,
				Lock_aware:   true,
				Mutations: []MutationRef{
					// \xff/tenant/lastId + trailing NUL — the C++ vector uses
					// std::string(lit, 16), which includes the literal's NUL.
					{MutType: 0, Param1: []byte("\xff/tenant/lastId\x00"), Param2: idVal},
					{MutType: 0, Param1: []byte("\xff/tenant/map/\x1c\x00\x00\x00\x00\x00\x00\x00\x03"), Param2: []byte("test")},
					{MutType: 0, Param1: []byte("\xff/tenant/nameIndex/test-tenant-crud"), Param2: idVal},
				},
			},
			Reply:      ReplyPromise{Token: tok},
			TenantInfo: TenantInfo{TenantId: -1},
		}).MarshalFDB()
	},
	"GetReadVersionRequest_causal_risky": func(tok [16]byte) []byte {
		return (&GetReadVersionRequest{
			Flags:            1, // FLAG_CAUSAL_READ_RISKY
			TransactionCount: 1,
			MaxVersion:       -1, // invalidVersion (C++ default)
			Reply:            ReplyPromise{Token: tok},
		}).MarshalFDB()
	},
	// GetReadVersionRequest::tags is TransactionTagMap<uint32_t> — a vector of
	// {tag, count} objects. An empty map and an empty byte string both encode to
	// a bare four-byte zero, so only this NON-empty case can tell the correct
	// encoding from the opaque-blob one that preceded it.
	"GetReadVersionRequest_tagged": func(tok [16]byte) []byte {
		return (&GetReadVersionRequest{
			TransactionCount: 1,
			MaxVersion:       -1,
			Tags:             []TransactionTagCount{{Tag: []byte("tenant-a"), Count: 3}},
			Reply:            ReplyPromise{Token: tok},
		}).MarshalFDB()
	},
	// Three entries, which is what actually exercises the vector's offset table
	// — a single-element vector can hide an off-by-one there. The ELEMENT ORDER
	// below is libstdc++'s std::unordered_map iteration order for these keys
	// under the pinned toolchain; it is not part of the wire contract, so if a
	// toolchain bump reorders it, re-read the order off the emitted vector and
	// update this literal. What the comparison pins is the framing, not the order.
	"GetReadVersionRequest_tagged_multi": func(tok [16]byte) []byte {
		return (&GetReadVersionRequest{
			TransactionCount: 4,
			MaxVersion:       -1,
			Tags: []TransactionTagCount{
				{Tag: []byte("gamma"), Count: 7},
				{Tag: []byte("beta"), Count: 2},
				{Tag: []byte("alpha"), Count: 1},
			},
			Reply: ReplyPromise{Token: tok},
		}).MarshalFDB()
	},
	// CommitTransactionRequest::tagSet is Optional<TagSet>, and TagSet has
	// dynamic_size_traits — a flat [len:1][tag bytes] concatenation in insertion
	// order. A byte blob IS the right model here, unlike the GRV map above.
	"CommitTransactionRequest_tagged": func(tok [16]byte) []byte {
		return (&CommitTransactionRequest{
			Transaction: CommitTransactionRef{
				ReadSnapshot: 7,
				Mutations: []MutationRef{
					{MutType: 0, Param1: []byte("tk"), Param2: []byte("tv")},
				},
				WriteConflictRanges: []KeyRangeRef{
					{Begin: []byte("tk"), End: []byte("tk\x00")},
				},
			},
			HasTagSet:  true,
			TagSet:     []byte("\x08tenant-a\x04bulk"),
			Reply:      ReplyPromise{Token: tok},
			TenantInfo: TenantInfo{TenantId: -1},
		}).MarshalFDB()
	},
}

// requestRoundTrip parses the C++ bytes with the generated UnmarshalFDB for
// the vector's type and re-marshals — the parse-direction ground truth for
// requests. Returns nil if the type has no unmarshal round-trip.
func requestRoundTrip(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	rt := func(m interface {
		UnmarshalFDB([]byte) error
		MarshalFDB() []byte
	},
	) []byte {
		if err := m.UnmarshalFDB(data); err != nil {
			t.Fatalf("%s: UnmarshalFDB on C++ bytes: %v", name, err)
		}
		return m.MarshalFDB()
	}
	switch {
	case hasPrefix(name, "GetKeyServerLocationsRequest"):
		return rt(&GetKeyServerLocationsRequest{})
	case hasPrefix(name, "GetValueRequest"):
		return rt(&GetValueRequest{})
	case hasPrefix(name, "GetKeyRequest"):
		return rt(&GetKeyRequest{})
	case hasPrefix(name, "GetKeyValuesRequest"):
		return rt(&GetKeyValuesRequest{})
	case hasPrefix(name, "GetMappedKeyValuesRequest"):
		return rt(&GetMappedKeyValuesRequest{})
	case hasPrefix(name, "CommitTransactionRequest"):
		return rt(&CommitTransactionRequest{})
	case hasPrefix(name, "GetReadVersionRequest"):
		return rt(&GetReadVersionRequest{})
	}
	t.Fatalf("%s: no round-trip type mapping", name)
	return nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func emptyVersionVector() []byte {
	// C++ VersionVector default encodes as 16 bytes:
	// [utlCount=0 (8 bytes LE)][maxVersion=invalidVersion=-1 (8 bytes LE)]
	vv := make([]byte, 16)
	binary.LittleEndian.PutUint64(vv[8:], ^uint64(0))
	return vv
}

func diffBytes(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: SIZE MISMATCH: Go=%d C++=%d", name, len(got), len(want))
	}
	n := min(len(got), len(want))
	shown := 0
	total := 0
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			if shown < 5 {
				t.Errorf("%s: BYTE MISMATCH at offset %d: Go=0x%02x C++=0x%02x", name, i, got[i], want[i])
				shown++
			}
			total++
		}
	}
	if total > shown {
		t.Errorf("%s: ... and %d more byte mismatches", name, total-shown)
	}
	if t.Failed() {
		t.Logf("Go  hex: %s", hex.EncodeToString(got))
		t.Logf("C++ hex: %s", hex.EncodeToString(want))
	}
}

// encodeVTable renders a vtable the way it appears in a serialized message's
// vtable region: uint16 little-endian entries, the first of which is the
// vtable's own byte length.
func encodeVTable(vt wire.VTable) []byte {
	out := make([]byte, 0, len(vt)*2)
	for _, e := range vt {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], e)
		out = append(out, b[:]...)
	}
	return out
}

// TestGroundTruthReplyVTables pins the object LAYOUTS of the GRV reply's
// tagThrottleInfo element types against real C++ bytes.
//
// The request vectors above pin TransactionTagCount byte-exactly, but its
// sibling TransactionTagThrottle is structurally different — its second slot is
// a nested OBJECT (ClientTagThrottleLimits), not a scalar — and no emitted C++
// vector carries a non-empty tagThrottleInfo, so the reply direction has no
// byte-exact golden. That direction is the one production actually parses, so
// leaving it entirely unpinned would repeat the failure this whole area was
// fixed for: a decoder green only against itself.
//
// What makes a pin possible anyway: a serialized message carries the vtables of
// its whole type closure regardless of which fields are populated, so the C++
// bytes for GetReadVersionReply contain the exact vtables of both types even
// though its map is empty. A vtable IS the layout — object size and per-slot
// offsets — so matching the generated constants against the region is a real
// C++-derived check on the only degrees of freedom the decode has.
//
// This also documents why GetReadVersionReply_locked's bytes are permitted to
// move without its C++ constructor changing: the region's vtable ORDER is an
// artifact of the emitting binary's instantiation order, not of the protocol
// (readers resolve a vtable by the offset stored in each object). Order is what
// this test deliberately does not assert.
func TestGroundTruthReplyVTables(t *testing.T) {
	t.Parallel()

	var vec *testVectorEntry
	for _, v := range loadTestVectors(t) {
		if v.Name == "GetReadVersionReply_locked" {
			v := v
			vec = &v
			break
		}
	}
	if vec == nil {
		t.Fatal("GetReadVersionReply_locked vector missing from testdata.json")
	}
	raw := vectorBytes(t, *vec)

	// The vtable region is a leading uint32 byte-count followed by the region
	// itself at offset 8. Bounding the search there keeps this from passing on
	// a coincidental byte run inside the payload.
	if len(raw) < 8 {
		t.Fatalf("reply vector too short: %d bytes", len(raw))
	}
	regionEnd := 8 + int(binary.LittleEndian.Uint32(raw[0:4]))
	if regionEnd > len(raw) {
		t.Fatalf("vtable region length %d overruns the %d-byte vector", regionEnd-8, len(raw))
	}
	region := raw[:regionEnd]

	for _, tc := range []struct {
		name string
		vt   wire.VTable
	}{
		{"ClientTagThrottleLimits", ClientTagThrottleLimitsVTable},
		{"TransactionTagThrottle", TransactionTagThrottleVTable},
	} {
		if !bytes.Contains(region, encodeVTable(tc.vt)) {
			t.Errorf("%s vtable %v is not in the C++ vtable region of "+
				"GetReadVersionReply_locked — the generated layout does not match "+
				"what the real ObjectWriter emitted\nregion hex: %s",
				tc.name, tc.vt, hex.EncodeToString(region))
		}
	}
}

// TestGroundTruthMarshal compares Go MarshalFDB output against C++
// ObjectWriter ground-truth bytes for every request vector — BYTE-IDENTICAL,
// zero tolerance (the reply-promise token is pinned in the vector, so there
// is no legitimate source of difference left). Then round-trips the C++
// bytes through the generated UnmarshalFDB and re-marshals: the
// parse-direction ground truth.
func TestGroundTruthMarshal(t *testing.T) {
	t.Parallel()
	vecs := loadTestVectors(t)

	seen := make(map[string]bool)
	for _, vec := range vecs {
		if vec.Kind != "request" {
			continue
		}
		vec := vec
		seen[vec.Name] = true
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()
			buildFn, ok := requestBuilders[vec.Name]
			if !ok {
				// Every request vector MUST have a builder — an unmatched
				// name is a broken net, not a skip (no-skip rule).
				t.Fatalf("no Go builder for request vector %s", vec.Name)
			}
			cppBytes := vectorBytes(t, vec)
			tok := vectorReplyToken(t, vec)

			goBytes := buildFn(tok)
			if !bytes.Equal(goBytes, cppBytes) {
				diffBytes(t, vec.Name+"/marshal", goBytes, cppBytes)
			}

			remarshalled := requestRoundTrip(t, vec.Name, cppBytes)
			if !bytes.Equal(remarshalled, cppBytes) {
				diffBytes(t, vec.Name+"/roundtrip", remarshalled, cppBytes)
			}
		})
	}

	// Reverse direction: every builder must be backed by a vector, so a
	// renamed/dropped vector fails loudly instead of silently un-testing.
	for name := range requestBuilders {
		if !seen[name] {
			t.Errorf("builder %s has no vector in testdata.json", name)
		}
	}
}
