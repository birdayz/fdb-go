package bench

import (
	"fmt"
	"os"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// Error-CODE differential vs libfdb_c — RFC-010 C3 (fresh axis). Apps branch on the
// exact FDB error code, so a Go client that rejects with the WRONG code (or a different
// size threshold) for an oversized key/value/transaction or an out-of-range key is a
// silent wire divergence. Existing probes (KeySizeBoundary, VersionstampErrors) assert
// rejection/acceptance but NOT the error code; codes 2101/2102/2103 (transaction/key/
// value too large) were entirely unprobed at the code level.
//
// C++ (7.3.75, error_definitions.h): key_outside_legal_range=2004,
// transaction_too_large=2101, key_too_large=2102, value_too_large=2103. Limits
// (ClientKnobs): KEY_SIZE_LIMIT=10000, VALUE_SIZE_LIMIT=100000, TRANSACTION_SIZE_LIMIT=1e7.
//
// The differential compares the CODE each client returns for the same trigger — it does
// not hard-code the expected code (a both-clients-agree-on-the-wrong-code case would be a
// C++-spec question, surfaced by the wantCode cross-check), but it DOES record the
// C++-spec code so a single-client divergence is obvious.

func goErrCode(setup func(tx gofdb.Transaction) error) int {
	_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		return nil, setup(tx)
	})
	return fdbErrorCode(err)
}

func cgoErrCode(setup func(tx cgofdb.Transaction) error) int {
	_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		return nil, setup(tx)
	})
	return fdbErrorCode(err)
}

func TestDifferential_ErrorCodes(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("differ_errcode_%d_", os.Getpid())

	cases := []struct {
		name     string
		wantCode int // C++-spec code (0 = both must succeed)
		goSetup  func(tx gofdb.Transaction) error
		cSetup   func(tx cgofdb.Transaction) error
	}{
		{
			name:     "value_at_limit_ok",
			wantCode: 0,
			goSetup:  func(tx gofdb.Transaction) error { tx.Set(gofdb.Key(pfx+"v0"), make([]byte, 100000)); return nil },
			cSetup:   func(tx cgofdb.Transaction) error { tx.Set(cgofdb.Key(pfx+"v0"), make([]byte, 100000)); return nil },
		},
		{
			name:     "value_too_large", // 2103
			wantCode: 2103,
			goSetup:  func(tx gofdb.Transaction) error { tx.Set(gofdb.Key(pfx+"v1"), make([]byte, 100001)); return nil },
			cSetup:   func(tx cgofdb.Transaction) error { tx.Set(cgofdb.Key(pfx+"v1"), make([]byte, 100001)); return nil },
		},
		{
			name:     "key_too_large", // 2102
			wantCode: 2102,
			goSetup:  func(tx gofdb.Transaction) error { tx.Set(gofdb.Key(make([]byte, 10001)), []byte{1}); return nil },
			cSetup:   func(tx cgofdb.Transaction) error { tx.Set(cgofdb.Key(make([]byte, 10001)), []byte{1}); return nil },
		},
		{
			name:     "transaction_too_large", // 2101
			wantCode: 2101,
			goSetup: func(tx gofdb.Transaction) error {
				for i := 0; i < 110; i++ {
					tx.Set(gofdb.Key(fmt.Sprintf("%stxl_%03d", pfx, i)), make([]byte, 100000))
				}
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				for i := 0; i < 110; i++ {
					tx.Set(cgofdb.Key(fmt.Sprintf("%stxl_%03d", pfx, i)), make([]byte, 100000))
				}
				return nil
			},
		},
		{
			name:     "read_system_key_no_access", // 2004
			wantCode: 2004,
			goSetup: func(tx gofdb.Transaction) error {
				_, err := tx.Get(gofdb.Key("\xff\x05")).Get()
				return err
			},
			cSetup: func(tx cgofdb.Transaction) error {
				_, err := tx.Get(cgofdb.Key("\xff\x05")).Get()
				return err
			},
		},
		{
			// VALIDATION ORDER (codex P2 / RFC-067): an oversized key in a >10 MB txn must
			// report key_too_large(2102), NOT transaction_too_large(2101) — C++ validates
			// per-mutation before the size check (commitMutations runs after set()'s checks).
			name:     "oversized_key_precedes_size",
			wantCode: 2102,
			goSetup: func(tx gofdb.Transaction) error {
				for i := 0; i < 110; i++ {
					tx.Set(gofdb.Key(fmt.Sprintf("%sok_%03d", pfx, i)), make([]byte, 100000))
				}
				tx.Set(gofdb.Key(make([]byte, 10001)), []byte{1})
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				for i := 0; i < 110; i++ {
					tx.Set(cgofdb.Key(fmt.Sprintf("%sok_%03d", pfx, i)), make([]byte, 100000))
				}
				tx.Set(cgofdb.Key(make([]byte, 10001)), []byte{1})
				return nil
			},
		},
		{
			// READ-ONLY fast path (codex P2 / RFC-067): a read-only txn (no mutations, no
			// write conflicts) with >10 MB of READ-conflict ranges must NOT be rejected for
			// size — C++ returns at the read-only fast path (NativeAPI:6800) before getSize.
			// 800×~16 KB disjoint ranges ≈ 12.8 MB of read-conflict bytes.
			name:     "readonly_large_read_conflicts",
			wantCode: 0,
			goSetup: func(tx gofdb.Transaction) error {
				for i := 0; i < 800; i++ {
					b := []byte(fmt.Sprintf("%src_%04d_", pfx, i))
					begin := append(append([]byte{}, b...), make([]byte, 8000)...)
					end := append(append([]byte{}, b...), make([]byte, 8000)...)
					end[len(end)-1] = 0xff
					_ = tx.AddReadConflictRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)})
				}
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				for i := 0; i < 800; i++ {
					b := []byte(fmt.Sprintf("%src_%04d_", pfx, i))
					begin := append(append([]byte{}, b...), make([]byte, 8000)...)
					end := append(append([]byte{}, b...), make([]byte, 8000)...)
					end[len(end)-1] = 0xff
					_ = tx.AddReadConflictRange(cgofdb.KeyRange{Begin: cgofdb.Key(begin), End: cgofdb.Key(end)})
				}
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
				t.Fatalf("%s: both clients returned %d but C++ spec is %d — agreed-but-wrong code (or knob drift)", tc.name, goCode, tc.wantCode)
			}
		})
	}
}
