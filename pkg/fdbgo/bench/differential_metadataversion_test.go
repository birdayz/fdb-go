package bench

import (
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// metadataVersionKey write-validation differential vs libfdb_c — RFC-067 follow-up.
// metadataVersionKey ("\xff/metadataVersion") is a special system
// key with a narrow legal-write contract (C++ ReadYourWrites.actor.cpp):
//
//   - atomicOp (:2226-2229): the ONLY legal op is SetVersionstampedValue whose operand ==
//     metadataVersionRequiredValue (SystemData.cpp:1387 — 14 zero bytes). Any other operand, or
//     any non-SVV atomic op (Add, SetVersionstampedKey, …), → client_invalid_operation (2000).
//   - set (:2300): a plain Set to metadataVersionKey → client_invalid_operation (2000).
//   - clear()/clear(range) (:2357, :2406): NO metadataVersionKey gate — a clear whose begin is
//     metadataVersionKey hits the normal legal-range check → key_outside_legal_range (2004),
//     since metadataVersionKey >= maxWriteKey.
//
// The Go client previously short-circuited metadataVersionKey with a blanket `continue`, so it
// committed ALL of these silently (code 0) — a write-path divergence. The fix enforces the C++
// contract in the per-mutation commit validation loop (transaction.go). go==cgo==wantCode pinned.
//
// metadataVersionKey is GLOBAL (not prefixed); the one legal case commits a metadata-version
// bump, which FDB is explicitly designed to handle concurrently (these writes do not create
// read/write conflicts among themselves), so the parallel suite is unaffected. The six rejection
// cases never reach commit.
func TestDifferential_MetadataVersionKey(t *testing.T) {
	t.Parallel()
	const mvk = "\xff/metadataVersion"
	required := make([]byte, 14)                                                  // 10-byte stamp + 4-byte offset 0
	wrong := []byte("\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x00\x00\x00\x00")   // 14B, non-zero body
	zeros14 := []byte("\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00") // for SVK case

	cases := []struct {
		name     string
		wantCode int
		goSetup  func(tx gofdb.Transaction) error
		cSetup   func(tx cgofdb.Transaction) error
	}{
		{
			name: "svv_required_value", wantCode: 0, // the ONLY legal write
			goSetup: func(tx gofdb.Transaction) error { tx.SetVersionstampedValue(gofdb.Key(mvk), required); return nil },
			cSetup:  func(tx cgofdb.Transaction) error { tx.SetVersionstampedValue(cgofdb.Key(mvk), required); return nil },
		},
		{
			name: "svv_wrong_value", wantCode: 2000,
			goSetup: func(tx gofdb.Transaction) error { tx.SetVersionstampedValue(gofdb.Key(mvk), wrong); return nil },
			cSetup:  func(tx cgofdb.Transaction) error { tx.SetVersionstampedValue(cgofdb.Key(mvk), wrong); return nil },
		},
		{
			name: "svv_short_operand", wantCode: 2000,
			goSetup: func(tx gofdb.Transaction) error {
				tx.SetVersionstampedValue(gofdb.Key(mvk), []byte{0, 0, 0})
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				tx.SetVersionstampedValue(cgofdb.Key(mvk), []byte{0, 0, 0})
				return nil
			},
		},
		{
			name: "plain_set", wantCode: 2000,
			goSetup: func(tx gofdb.Transaction) error { tx.Set(gofdb.Key(mvk), []byte("x")); return nil },
			cSetup:  func(tx cgofdb.Transaction) error { tx.Set(cgofdb.Key(mvk), []byte("x")); return nil },
		},
		{
			name: "atomic_add", wantCode: 2000,
			goSetup: func(tx gofdb.Transaction) error {
				tx.Add(gofdb.Key(mvk), []byte("\x01\x00\x00\x00\x00\x00\x00\x00"))
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				tx.Add(cgofdb.Key(mvk), []byte("\x01\x00\x00\x00\x00\x00\x00\x00"))
				return nil
			},
		},
		{
			name: "svk_to_mvk", wantCode: 2000, // SetVersionstampedKey (not SVV) → invalid op
			goSetup: func(tx gofdb.Transaction) error { tx.SetVersionstampedKey(gofdb.Key(mvk), zeros14); return nil },
			cSetup:  func(tx cgofdb.Transaction) error { tx.SetVersionstampedKey(cgofdb.Key(mvk), zeros14); return nil },
		},
		{
			name: "clear_mvk", wantCode: 2004, // clear() has no mvk gate → legal-range
			goSetup: func(tx gofdb.Transaction) error { tx.Clear(gofdb.Key(mvk)); return nil },
			cSetup:  func(tx cgofdb.Transaction) error { tx.Clear(cgofdb.Key(mvk)); return nil },
		},
		{
			name: "clearrange_over_mvk", wantCode: 2004,
			goSetup: func(tx gofdb.Transaction) error {
				tx.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(mvk), End: gofdb.Key(mvk + "\x00")})
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				tx.ClearRange(cgofdb.KeyRange{Begin: cgofdb.Key(mvk), End: cgofdb.Key(mvk + "\x00")})
				return nil
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goCode := goErrCode(tc.goSetup)
			cCode := cgoErrCode(tc.cSetup)
			if goCode != cCode {
				t.Fatalf("%s: error code differs — go=%d cgo=%d (C++ spec: %d)", tc.name, goCode, cCode, tc.wantCode)
			}
			if goCode != tc.wantCode {
				t.Fatalf("%s: both clients returned %d but C++ spec is %d", tc.name, goCode, tc.wantCode)
			}
		})
	}
}
