package bench

import (
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Versionstamp-validation ORDER differential vs libfdb_c — RFC-067 P2 follow-up.
//
// The size-limit RFC made the transaction_too_large (2101) check fire by default. codex
// flagged that this could pre-empt versionstamp-offset validation. The differential below
// establishes the FULL ordering ground truth (this is the spec, not the C++ reading):
//
//   - In libfdb_c, key-size (2102), value-size (2103), and versionstamp-offset (2000) are
//     all EAGER — validated at the Set()/atomicOp() call, in CALL order. The FIRST eagerly-
//     invalid operation's code wins.
//   - WITHIN a single op, the key/value SIZE check runs BEFORE the versionstamp-offset
//     check (an oversized versionstamp key/value reports 2102/2103, never 2000).
//   - transaction_too_large (2101) is DEFERRED to commit, so it NEVER pre-empts an eager
//     error — it only fires when no per-mutation op is itself invalid.
//
// The Go client defers all validation to commit, so it reproduces "first eagerly-invalid op
// wins" by validating per-mutation in MUTATION order (= call order), with the versionstamp-
// offset check sitting in that same loop after the size checks and before the deferred
// transaction-size check (transaction.go). Before the fix the versionstamp check ran AFTER
// the size check (separate loop), so the four "versionstamp op first" cases returned 2101/
// 2102/2103 instead of 2000 — the red→green proof.
func TestDifferential_VersionstampValidationOrder(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("vsord_%d_", os.Getpid())
	badVS := vsOperand(make([]byte, vsStampLen), 99) // offset 99 past a 10-byte body → 2000

	cases := []struct {
		name     string
		wantCode int
		goSetup  func(tx gofdb.Transaction) error
		cSetup   func(tx cgofdb.Transaction) error
	}{
		{
			// codex's case: bad versionstamp + >10 MB of otherwise-valid sets. The deferred
			// 2101 must NOT pre-empt the eager 2000 — order-independent (txn-size is deferred).
			name:     "vsfirst_then_oversized_txn",
			wantCode: 2000,
			goSetup: func(tx gofdb.Transaction) error {
				tx.SetVersionstampedKey(gofdb.Key(pfx+"a_vs"), badVS)
				for i := 0; i < 110; i++ {
					tx.Set(gofdb.Key(fmt.Sprintf("%sa_%03d", pfx, i)), make([]byte, 100000))
				}
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				tx.SetVersionstampedKey(cgofdb.Key(pfx+"a_vs"), badVS)
				for i := 0; i < 110; i++ {
					tx.Set(cgofdb.Key(fmt.Sprintf("%sa_%03d", pfx, i)), make([]byte, 100000))
				}
				return nil
			},
		},
		{
			name:     "oversized_txn_then_vs", // reversed order — still 2000 (2101 deferred)
			wantCode: 2000,
			goSetup: func(tx gofdb.Transaction) error {
				for i := 0; i < 110; i++ {
					tx.Set(gofdb.Key(fmt.Sprintf("%sb_%03d", pfx, i)), make([]byte, 100000))
				}
				tx.SetVersionstampedKey(gofdb.Key(pfx+"b_vs"), badVS)
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				for i := 0; i < 110; i++ {
					tx.Set(cgofdb.Key(fmt.Sprintf("%sb_%03d", pfx, i)), make([]byte, 100000))
				}
				tx.SetVersionstampedKey(cgofdb.Key(pfx+"b_vs"), badVS)
				return nil
			},
		},
		{
			name:     "vsfirst_then_oversized_value", // vs op first → eager 2000 wins
			wantCode: 2000,
			goSetup: func(tx gofdb.Transaction) error {
				tx.SetVersionstampedKey(gofdb.Key(pfx+"c_vs"), badVS)
				tx.Set(gofdb.Key(pfx+"c_big"), make([]byte, 100001))
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				tx.SetVersionstampedKey(cgofdb.Key(pfx+"c_vs"), badVS)
				tx.Set(cgofdb.Key(pfx+"c_big"), make([]byte, 100001))
				return nil
			},
		},
		{
			name:     "oversized_value_then_vs", // oversized value first → eager 2103 wins
			wantCode: 2103,
			goSetup: func(tx gofdb.Transaction) error {
				tx.Set(gofdb.Key(pfx+"d_big"), make([]byte, 100001))
				tx.SetVersionstampedKey(gofdb.Key(pfx+"d_vs"), badVS)
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				tx.Set(cgofdb.Key(pfx+"d_big"), make([]byte, 100001))
				tx.SetVersionstampedKey(cgofdb.Key(pfx+"d_vs"), badVS)
				return nil
			},
		},
		{
			name:     "vsfirst_then_oversized_key", // vs op first → eager 2000 wins
			wantCode: 2000,
			goSetup: func(tx gofdb.Transaction) error {
				tx.SetVersionstampedKey(gofdb.Key(pfx+"e_vs"), badVS)
				tx.Set(gofdb.Key(make([]byte, 10001)), []byte{1})
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				tx.SetVersionstampedKey(cgofdb.Key(pfx+"e_vs"), badVS)
				tx.Set(cgofdb.Key(make([]byte, 10001)), []byte{1})
				return nil
			},
		},
		{
			name:     "oversized_key_then_vs", // oversized key first → eager 2102 wins
			wantCode: 2102,
			goSetup: func(tx gofdb.Transaction) error {
				tx.Set(gofdb.Key(make([]byte, 10001)), []byte{1})
				tx.SetVersionstampedKey(gofdb.Key(pfx+"f_vs"), badVS)
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				tx.Set(cgofdb.Key(make([]byte, 10001)), []byte{1})
				tx.SetVersionstampedKey(cgofdb.Key(pfx+"f_vs"), badVS)
				return nil
			},
		},
		{
			// WITHIN one op: oversized versionstamp KEY + bad offset → key-size (2102) wins.
			name:     "vskey_oversized_and_badoffset",
			wantCode: 2102,
			goSetup: func(tx gofdb.Transaction) error {
				op := make([]byte, 10001) // > KEY_SIZE_LIMIT; last 4 bytes = LE offset past body
				op[9997], op[9998], op[9999], op[10000] = 0x9f, 0x86, 0x01, 0x00
				tx.SetVersionstampedKey(gofdb.Key(op), []byte("v"))
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				op := make([]byte, 10001)
				op[9997], op[9998], op[9999], op[10000] = 0x9f, 0x86, 0x01, 0x00
				tx.SetVersionstampedKey(cgofdb.Key(op), []byte("v"))
				return nil
			},
		},
		{
			// WITHIN one op: oversized versionstamp VALUE + bad offset → value-size (2103) wins.
			name:     "vsvalue_oversized_and_badoffset",
			wantCode: 2103,
			goSetup: func(tx gofdb.Transaction) error {
				op := make([]byte, 100001) // > VALUE_SIZE_LIMIT
				op[99997], op[99998], op[99999], op[100000] = 0xff, 0xff, 0xff, 0x7f
				tx.SetVersionstampedValue(gofdb.Key(pfx+"g_k"), op)
				return nil
			},
			cSetup: func(tx cgofdb.Transaction) error {
				op := make([]byte, 100001)
				op[99997], op[99998], op[99999], op[100000] = 0xff, 0xff, 0xff, 0x7f
				tx.SetVersionstampedValue(cgofdb.Key(pfx+"g_k"), op)
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
