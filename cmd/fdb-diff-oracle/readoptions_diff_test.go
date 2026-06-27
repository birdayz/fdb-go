package difforacle

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// TestDiffReadOptions is the C++ byte-truth for RFC-117: ReadOptions'
// consistencyCheckStartVersion is Optional<Version> (int64), serialized as a bare
// out-of-line scalar behind the union RelativeOffset (C++ SaveAlternative,
// flat_buffers.h:848) — the field the extractor used to mis-emit as Go []byte. The
// oracle's handleReadOptions builds a GetValueRequest{key:"ro", options:{…}} with the
// FDB serializer; the Go side builds the same carrier with MarshalFDB; the structural
// compare parses BOTH with the Go parser — so if C++ wrote a bare int64 scalar and Go
// read it via ReadRelOffUint64, the values match; a []byte/length-prefix divergence
// would mis-parse and mismatch. Deterministic ⇒ runs in the per-PR `-run='TestDiff'` gate.
//
// Revert-prove: revert optionalInnerIsScalar to UID-only + regen → the field becomes
// []byte, Go writes a length-prefixed vector where C++ writes a bare scalar → this
// test's ConsistencyCheckStartVersion comparison reddens.
func TestDiffReadOptions(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	debugID := [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 2, 3, 4, 5, 6, 7, 8, 0xCA, 0xFE, 0xBA, 0xBE}

	cases := []struct {
		name       string
		roType     int32
		cache      bool
		hasDebugID bool
		debugID    [16]byte
		hasCCSV    bool
		ccsv       int64
		lockAware  bool
	}{
		// The headline case: consistencyCheckStartVersion present + the sibling
		// Optional<UID> debugID present (proves the two-Optional nested layout, slots 3 & 5).
		{"ccsv+debugID+lockAware", 7, true, true, debugID, true, 0x0102030405060708, true},
		// consistencyCheckStartVersion alone (no debugID) — isolates the new Optional<int64>.
		{"ccsv only", 0, false, false, [16]byte{}, true, 0x7766554433221100, false},
		// Boundary: ccsv == 0 still serializes (present-tag set, bare 8 zero bytes behind reloff).
		{"ccsv zero", 1, true, false, [16]byte{}, true, 0, false},
		// Boundary: ccsv == -1 (all 0xFF) — distinct from absent.
		{"ccsv negative one", 0, false, true, debugID, true, -1, true},
		// Absent: no consistencyCheckStartVersion (Has-tag false) — must not serialize it.
		{"ccsv absent, debugID present", 3, true, true, debugID, false, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			goMsg := &types.GetValueRequest{
				Key:        []byte("ro"),
				HasOptions: true,
				Options: types.ReadOptions{
					Type:                            tc.roType,
					CacheResult:                     tc.cache,
					HasDebugID:                      tc.hasDebugID,
					DebugID:                         tc.debugID,
					HasConsistencyCheckStartVersion: tc.hasCCSV,
					ConsistencyCheckStartVersion:    tc.ccsv,
					LockAware:                       tc.lockAware,
				},
				SsLatestCommitVersions: emptyVersionVector(),
			}
			goBytes := goMsg.MarshalFDB()

			cppBytes, err := o.SerializeReadOptions(
				tc.roType, tc.cache, tc.hasDebugID, tc.debugID, tc.hasCCSV, tc.ccsv, tc.lockAware)
			if err != nil {
				t.Fatalf("oracle error: %v", err)
			}
			if cppBytes == nil {
				t.Fatal("oracle returned error response")
			}

			compareBytesStructural(t, goBytes, cppBytes, "ReadOptions",
				unmarshalGetValueRequest, equalReadOptionsCarrier)
		})
	}
}

// equalReadOptionsCarrier compares only the fields under test — Key + the ReadOptions
// Optionals/scalars — and deliberately ignores ssLatestCommitVersions (the Go carrier sets
// emptyVersionVector while the C++ carrier leaves the default; both are off-topic for RFC-117).
func equalReadOptionsCarrier(a, b types.GetValueRequest) bool {
	if string(a.Key) != string(b.Key) || a.HasOptions != b.HasOptions {
		return false
	}
	ao, bo := a.Options, b.Options
	return ao.Type == bo.Type &&
		ao.CacheResult == bo.CacheResult &&
		ao.HasDebugID == bo.HasDebugID &&
		ao.DebugID == bo.DebugID &&
		ao.HasConsistencyCheckStartVersion == bo.HasConsistencyCheckStartVersion &&
		ao.ConsistencyCheckStartVersion == bo.ConsistencyCheckStartVersion &&
		ao.LockAware == bo.LockAware
}
