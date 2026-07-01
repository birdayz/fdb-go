package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// le8b is an 8-byte little-endian operand (the canonical FDB counter width; folds on equal length).
func le8b(n uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, n)
	return b
}

func mustReadGo(t *testing.T, k gofdb.Key) []byte {
	t.Helper()
	v, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
		return txw.(gofdb.Transaction).Get(k).Get()
	})
	if err != nil {
		t.Fatalf("go read-back %q: %v", k, err)
	}
	b, _ := v.([]byte)
	return b
}

func mustReadCgo(t *testing.T, k cgofdb.Key) []byte {
	t.Helper()
	v, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) { return tx.Get(k).Get() })
	if err != nil {
		t.Fatalf("cgo read-back %q: %v", k, err)
	}
	b, _ := v.([]byte)
	return b
}

// TestDifferential_CommitCoalescing_2101 pins RFC-172 / #28: a transaction that hammers ONE key N times
// ships ONE folded mutation (+ one write-conflict range) — the coalesced RYW write map libfdb_c commits —
// so under a SIZE_LIMIT the UNFOLDED op-log would exceed, the pure-Go client and libfdb_c must BOTH commit
// and agree on the value. Before #28 the Go client shipped the raw N-entry op-log (and N duplicate
// conflict ranges) and returned transaction_too_large (2101) where libfdb_c coalesced and committed fine —
// a cross-client app break (the same transaction works in C/Java on the shared cluster, fails in Go).
func TestDifferential_CommitCoalescing_2101(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("coal2101_%d_", os.Getpid())
	const n = 2000
	const sizeLimit = 2000 // fits ONE folded mutation+conflict (~120B); the unfolded N-op log is ~240KB.

	cases := []struct {
		name    string
		goApply func(tx gofdb.Transaction, k gofdb.Key)
		cApply  func(tx cgofdb.Transaction, k cgofdb.Key)
		want    []byte
	}{
		{
			"repeated_add",
			func(tx gofdb.Transaction, k gofdb.Key) {
				for i := 0; i < n; i++ {
					tx.Add(k, le8b(1))
				}
			},
			func(tx cgofdb.Transaction, k cgofdb.Key) {
				for i := 0; i < n; i++ {
					tx.Add(k, le8b(1))
				}
			},
			le8b(n),
		},
		{
			"repeated_set",
			func(tx gofdb.Transaction, k gofdb.Key) {
				for i := 0; i < n; i++ {
					tx.Set(k, []byte(fmt.Sprintf("v%06d", i)))
				}
			},
			func(tx cgofdb.Transaction, k cgofdb.Key) {
				for i := 0; i < n; i++ {
					tx.Set(k, []byte(fmt.Sprintf("v%06d", i)))
				}
			},
			[]byte(fmt.Sprintf("v%06d", n-1)),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goKey := gofdb.Key(pfx + "go_" + tc.name)
			cKey := cgofdb.Key(pfx + "c_" + tc.name)

			goCode := goErrCode(func(tx gofdb.Transaction) error {
				if err := tx.Options().SetSizeLimit(sizeLimit); err != nil {
					return err
				}
				tc.goApply(tx, goKey)
				return nil
			})
			cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
				if err := tx.Options().SetSizeLimit(sizeLimit); err != nil {
					return err
				}
				tc.cApply(tx, cKey)
				return nil
			})
			if goCode != cCode {
				t.Fatalf("%s: commit code diverges — go=%d cgo=%d (go>0 means the unfolded op-log tripped 2101 "+
					"where libfdb_c coalesced the write map and committed)", tc.name, goCode, cCode)
			}
			if goCode != 0 {
				t.Fatalf("%s: both clients rejected (code %d) — the folded commit must fit under SIZE_LIMIT=%d",
					tc.name, goCode, sizeLimit)
			}
			gv, cv := mustReadGo(t, goKey), mustReadCgo(t, cKey)
			if !bytes.Equal(gv, tc.want) || !bytes.Equal(cv, tc.want) {
				t.Fatalf("%s: committed value mismatch — go=%x cgo=%x want=%x", tc.name, gv, cv, tc.want)
			}
		})
	}
}

// TestDifferential_CommitCoalescing_ChainState pins that coalescing the commit vector preserves the exact
// committed state across op-combination CHAINS (repeated/overlapping ops on one key), byte-for-byte with
// libfdb_c. Each case runs the same chain on both clients under the default limit and asserts identical
// committed values — the value-correctness companion to the 2101 size proof above.
func TestDifferential_CommitCoalescing_ChainState(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("coalstate_%d_", os.Getpid())

	cases := []struct {
		name string
		goFn func(tx gofdb.Transaction, k gofdb.Key)
		cFn  func(tx cgofdb.Transaction, k cgofdb.Key)
	}{
		{
			"add_then_set_wins", // atomic chain then an absolute Set → Set wins (value-fold)
			func(tx gofdb.Transaction, k gofdb.Key) {
				tx.Add(k, le8b(3))
				tx.Add(k, le8b(4))
				tx.Set(k, []byte("final"))
			},
			func(tx cgofdb.Transaction, k cgofdb.Key) {
				tx.Add(k, le8b(3))
				tx.Add(k, le8b(4))
				tx.Set(k, []byte("final"))
			},
		},
		{
			"set_then_add", // Set base then atomic → resolved value (Site B fold)
			func(tx gofdb.Transaction, k gofdb.Key) { tx.Set(k, le8b(10)); tx.Add(k, le8b(5)) },
			func(tx cgofdb.Transaction, k cgofdb.Key) { tx.Set(k, le8b(10)); tx.Add(k, le8b(5)) },
		},
		{
			"set_then_clear", // absolute Set then Clear → cleared (absent)
			func(tx gofdb.Transaction, k gofdb.Key) { tx.Set(k, []byte("x")); tx.Clear(k) },
			func(tx cgofdb.Transaction, k cgofdb.Key) { tx.Set(k, []byte("x")); tx.Clear(k) },
		},
		{
			// ClearRange over [k, k\xff) — a range that CONTAINS the set key k (its inclusive begin) — then
			// Set k → clears-first ordering, the later Set on the in-range key survives. The clear is scoped
			// to k's OWN subtree so it can't clobber any other case's or the other client's key in the
			// shared cluster keyspace (the earlier bug: a pfx-wide clear wiped the sibling client's key).
			"clearrange_then_set",
			func(tx gofdb.Transaction, k gofdb.Key) {
				tx.ClearRange(gofdb.KeyRange{Begin: k, End: gofdb.Key(string(k) + "\xff")})
				tx.Set(k, []byte("survives"))
			},
			func(tx cgofdb.Transaction, k cgofdb.Key) {
				tx.ClearRange(cgofdb.KeyRange{Begin: k, End: cgofdb.Key(string(k) + "\xff")})
				tx.Set(k, []byte("survives"))
			},
		},
		{
			"mixed_atomic_types", // ADD then OR on one key → two ops kept (different types), resolved
			func(tx gofdb.Transaction, k gofdb.Key) { tx.Add(k, le8b(1)); tx.Or(k, le8b(0x0f00)) },
			func(tx cgofdb.Transaction, k cgofdb.Key) { tx.Add(k, le8b(1)); tx.Or(k, le8b(0x0f00)) },
		},
		{
			"nonassoc_size_change", // ADD 8-byte then ADD 4-byte → non-associative, both kept
			func(tx gofdb.Transaction, k gofdb.Key) { tx.Add(k, le8b(1)); tx.Add(k, []byte{1, 0, 0, 0}) },
			func(tx cgofdb.Transaction, k cgofdb.Key) { tx.Add(k, le8b(1)); tx.Add(k, []byte{1, 0, 0, 0}) },
		},
		{
			"compare_and_clear", // Set then a matching CompareAndClear → key cleared (absent)
			func(tx gofdb.Transaction, k gofdb.Key) {
				tx.Set(k, []byte("gone"))
				tx.CompareAndClear(k, []byte("gone"))
			},
			func(tx cgofdb.Transaction, k cgofdb.Key) {
				tx.Set(k, []byte("gone"))
				tx.CompareAndClear(k, []byte("gone"))
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goKey := gofdb.Key(pfx + "go_" + tc.name)
			cKey := cgofdb.Key(pfx + "c_" + tc.name)
			if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
				tc.goFn(txw.(gofdb.Transaction), goKey)
				return nil, nil
			}); err != nil {
				t.Fatalf("%s: go commit: %v", tc.name, err)
			}
			if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
				tc.cFn(tx, cKey)
				return nil, nil
			}); err != nil {
				t.Fatalf("%s: cgo commit: %v", tc.name, err)
			}
			gv, cv := mustReadGo(t, goKey), mustReadCgo(t, cKey)
			if !bytes.Equal(gv, cv) {
				t.Fatalf("%s: committed state diverges — go=%x cgo=%x", tc.name, gv, cv)
			}
		})
	}
}
