package bench

import (
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// TestDifferential_TransactionSizeLimit pins the transaction_too_large (2101) boundary against
// libfdb_c: for the same workload under the same SIZE_LIMIT, the pure-Go client and libfdb_c must
// accept/reject at the IDENTICAL limit. The commit-size accounting is entirely client-side, so a
// per-op overhead mismatch is a cross-client commit incompatibility — the same large transaction
// would fail in Go but succeed in C/Java sharing the cluster.
//
// The 2101 check uses the native commit accounting: each mutation charged sizeof(MutationRef)=44,
// each conflict range sizeof(KeyRangeRef)=24 (StringRef is packed to 12 by flow/Arena.h:370). It
// covers BOTH a Set workload and a single-key-Clear workload — the latter pins that a single-key
// clear is charged sizeof(MutationRef) here (unlike GetApproximateSize, which charges it
// sizeof(KeyRangeRef)). This was RED before the 44/24 constant fix (the old 48/32 rejected ~12%
// earlier than libfdb_c).
func TestDifferential_TransactionSizeLimit(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("szlim_%d_", os.Getpid())
	const n = 12
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("%s%03d", pfx, i))
	}

	// threshold returns the smallest SIZE_LIMIT at which a commit of the given workload is ACCEPTED
	// (code 0); just below it the commit is rejected with transaction_too_large (2101).
	threshold := func(commit func(limit int64) int) int64 {
		lo, hi := int64(1), int64(20000)
		for lo < hi {
			mid := (lo + hi) / 2
			if commit(mid) == 0 {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		return lo
	}

	cases := []struct {
		name string
		goOp func(tx gofdb.Transaction, k []byte)
		cOp  func(tx cgofdb.Transaction, k []byte)
	}{
		{
			name: "set",
			goOp: func(tx gofdb.Transaction, k []byte) { tx.Set(gofdb.Key(k), make([]byte, 20)) },
			cOp:  func(tx cgofdb.Transaction, k []byte) { tx.Set(cgofdb.Key(k), make([]byte, 20)) },
		},
		{
			name: "single_key_clear",
			goOp: func(tx gofdb.Transaction, k []byte) { tx.Clear(gofdb.Key(k)) },
			cOp:  func(tx cgofdb.Transaction, k []byte) { tx.Clear(cgofdb.Key(k)) },
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goThresh := threshold(func(limit int64) int {
				return goErrCode(func(tx gofdb.Transaction) error {
					if err := tx.Options().SetSizeLimit(limit); err != nil {
						return err
					}
					for _, k := range keys {
						tc.goOp(tx, k)
					}
					return nil
				})
			})
			cThresh := threshold(func(limit int64) int {
				return cgoErrCode(func(tx cgofdb.Transaction) error {
					if err := tx.Options().SetSizeLimit(limit); err != nil {
						return err
					}
					for _, k := range keys {
						tc.cOp(tx, k)
					}
					return nil
				})
			})
			if goThresh != cThresh {
				t.Errorf("commit-size 2101 boundary diverges: go accepts at limit>=%d, cgo at limit>=%d (delta=%+d)",
					goThresh, cThresh, goThresh-cThresh)
			}
		})
	}
}
