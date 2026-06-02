package bench

import (
	"bytes"
	"encoding/binary"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// L2 differential write battery — RFC-053 (RFC-010 C2). Run the SAME logical write
// through BOTH clients (pure-Go and libfdb_c) to its own prefix on ONE cluster,
// then read the persisted value back via the C binding (the reference reader) AND
// the pure-Go client, and assert byte-identical persisted state.
//
// What each layer proves:
//   - Set: the stored bytes ARE the client's output → a true byte-identity proof.
//   - Atomics: the SERVER computes the result, so this is an end-to-end encode+
//     server equivalence check, NOT an encode-identity proof (that is the L1 golden
//     test in package client). It still catches gross op-code divergence — most
//     valuably a missing Min→MinV2 / And→AndV2 upgrade, whose MISSING-KEY semantics
//     differ: Min(absent,5) under the legacy op yields min(0,5)=0, but MinV2 yields
//     5. Every atomic below runs on a freshly-cleared (absent) key for exactly this
//     reason.
//
// The Go client must read Go's own write identically to the C reader (cross-read),
// closing the circularity: both readers agree on what the server actually holds.

type diffOp struct {
	name string
	goW  func(tx gofdb.Transaction, key []byte)
	cW   func(tx cgofdb.Transaction, key []byte)
}

func TestDifferential_WriteBattery(t *testing.T) {
	t.Parallel()
	le8 := func(n uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, n); return b }

	// Method-expression adapters: bind a fixed operand to an atomic method so the
	// battery rows stay one-liners. gofdb/cgofdb expose identical method sets.
	goA := func(m func(gofdb.Transaction, gofdb.KeyConvertible, []byte), p []byte) func(gofdb.Transaction, []byte) {
		return func(tx gofdb.Transaction, k []byte) { m(tx, gofdb.Key(k), p) }
	}
	cA := func(m func(cgofdb.Transaction, cgofdb.KeyConvertible, []byte), p []byte) func(cgofdb.Transaction, []byte) {
		return func(tx cgofdb.Transaction, k []byte) { m(tx, cgofdb.Key(k), p) }
	}

	ops := []diffOp{
		{
			"set_ascii",
			func(tx gofdb.Transaction, k []byte) { tx.Set(gofdb.Key(k), []byte("hello world")) },
			func(tx cgofdb.Transaction, k []byte) { tx.Set(cgofdb.Key(k), []byte("hello world")) },
		},
		{
			"set_empty",
			func(tx gofdb.Transaction, k []byte) { tx.Set(gofdb.Key(k), []byte{}) },
			func(tx cgofdb.Transaction, k []byte) { tx.Set(cgofdb.Key(k), []byte{}) },
		},
		{
			"set_binary",
			func(tx gofdb.Transaction, k []byte) { tx.Set(gofdb.Key(k), []byte{0x00, 0x01, 0xFE, 0xFF, 0x00, 0x80}) },
			func(tx cgofdb.Transaction, k []byte) {
				tx.Set(cgofdb.Key(k), []byte{0x00, 0x01, 0xFE, 0xFF, 0x00, 0x80})
			},
		},
		{
			"set_at_value_limit", // exactly VALUE_SIZE_LIMIT — must be accepted by both
			func(tx gofdb.Transaction, k []byte) { tx.Set(gofdb.Key(k), bytes.Repeat([]byte{0x5a}, 100000)) },
			func(tx cgofdb.Transaction, k []byte) { tx.Set(cgofdb.Key(k), bytes.Repeat([]byte{0x5a}, 100000)) },
		},
		// Atomics on a MISSING key — op-code + missing-key semantics.
		{"add", goA(gofdb.Transaction.Add, le8(5)), cA(cgofdb.Transaction.Add, le8(5))},
		{"min_missing_V2", goA(gofdb.Transaction.Min, le8(8)), cA(cgofdb.Transaction.Min, le8(8))},
		{"and_missing_V2", goA(gofdb.Transaction.And, le8(0xFF)), cA(cgofdb.Transaction.And, le8(0xFF))},
		{"bitand_missing_V2", goA(gofdb.Transaction.BitAnd, le8(0xFF)), cA(cgofdb.Transaction.BitAnd, le8(0xFF))},
		{"or", goA(gofdb.Transaction.Or, le8(0x0F)), cA(cgofdb.Transaction.Or, le8(0x0F))},
		{"xor", goA(gofdb.Transaction.Xor, le8(0xAA)), cA(cgofdb.Transaction.Xor, le8(0xAA))},
		{"max", goA(gofdb.Transaction.Max, le8(7)), cA(cgofdb.Transaction.Max, le8(7))},
		{"bytemin", goA(gofdb.Transaction.ByteMin, []byte("mmm")), cA(cgofdb.Transaction.ByteMin, []byte("mmm"))},
		{"bytemax", goA(gofdb.Transaction.ByteMax, []byte("zzz")), cA(cgofdb.Transaction.ByteMax, []byte("zzz"))},
		{"appendiffits", goA(gofdb.Transaction.AppendIfFits, []byte("abc")), cA(cgofdb.Transaction.AppendIfFits, []byte("abc"))},
	}

	for _, op := range ops {
		op := op
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			goKey := []byte("diff_go_" + op.name)
			cKey := []byte("diff_c_" + op.name)

			// Clean slate: missing-key atomic semantics depend on absence.
			mustCGo(t, func(tx cgofdb.Transaction) {
				tx.Clear(cgofdb.Key(goKey))
				tx.Clear(cgofdb.Key(cKey))
			})

			if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
				op.goW(tx, goKey)
				return nil, nil
			}); err != nil {
				t.Fatalf("go write: %v", err)
			}
			mustCGo(t, func(tx cgofdb.Transaction) { op.cW(tx, cKey) })

			// Read both back via the C reference reader; the persisted residual must
			// be byte-identical.
			goByC := cgoGet(t, goKey)
			cByC := cgoGet(t, cKey)
			if !bytes.Equal(goByC, cByC) {
				t.Fatalf("%s: persisted bytes differ (C reader): go=%x cgo=%x", op.name, goByC, cByC)
			}
			// Cross-read: the Go client must agree with the C reader on Go's own
			// write (otherwise the byte-compare above could be hiding a read-path bug).
			if goByGo := goGet(t, goKey); !bytes.Equal(goByGo, goByC) {
				t.Fatalf("%s: Go reader (%x) disagrees with C reader (%x) on Go's own write", op.name, goByGo, goByC)
			}
		})
	}
}

// TestDifferential_VersionstampedValue proves SetVersionstampedValue places the
// 10-byte stamp at the same offset and ships the same surrounding bytes through
// both clients. The stamp itself is the commit version (different per txn), so it
// is masked out; the persisted length and the non-stamp bytes must match.
func TestDifferential_VersionstampedValue(t *testing.T) {
	t.Parallel()
	// value = [10-byte stamp placeholder][user data]["\x00\x00\x00\x00" LE offset=0]
	// The server fills the stamp at the offset and strips the trailing 4 bytes, so
	// the persisted value is [10-byte stamp][user data].
	mkVal := func() []byte {
		v := make([]byte, 0, 10+5+4)
		v = append(v, make([]byte, 10)...)    // stamp placeholder
		v = append(v, []byte("DATA!")...)     // user data
		v = append(v, 0x00, 0x00, 0x00, 0x00) // offset = 0 (LE)
		return v
	}
	goKey := []byte("diff_svv_go")
	cKey := []byte("diff_svv_c")
	mustCGo(t, func(tx cgofdb.Transaction) {
		tx.Clear(cgofdb.Key(goKey))
		tx.Clear(cgofdb.Key(cKey))
	})
	if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		tx.SetVersionstampedValue(gofdb.Key(goKey), mkVal())
		return nil, nil
	}); err != nil {
		t.Fatalf("go SVV: %v", err)
	}
	mustCGo(t, func(tx cgofdb.Transaction) { tx.SetVersionstampedValue(cgofdb.Key(cKey), mkVal()) })

	goV := cgoGet(t, goKey)
	cV := cgoGet(t, cKey)
	if len(goV) != len(cV) {
		t.Fatalf("SVV persisted length differs: go=%d c=%d", len(goV), len(cV))
	}
	if len(goV) < 10 {
		t.Fatalf("SVV persisted value too short: %d", len(goV))
	}
	// Mask the 10-byte stamp (offset 0); the rest (user data) must be identical.
	if !bytes.Equal(goV[10:], cV[10:]) {
		t.Fatalf("SVV non-stamp bytes differ: go=%x c=%x", goV[10:], cV[10:])
	}
}

// TestDifferential_KeySizeBoundary exercises the KEY_SIZE_LIMIT knob against the
// LINKED libfdb_c, not just a hardcoded constant. A key of exactly KEY_SIZE_LIMIT
// (10000) is at the boundary (`size > limit` is false) and must be accepted and
// persist identically by both clients. If the Go KEY_SIZE_LIMIT knob drifted below
// the C client's, Go would reject this key at commit and the read-back would differ
// (go=nil vs cgo=value). The value-limit boundary is covered by the battery's
// set_at_value_limit row.
func TestDifferential_KeySizeBoundary(t *testing.T) {
	t.Parallel()
	const keyLimit = 10000 // CLIENT_KNOBS->KEY_SIZE_LIMIT
	mkKey := func(prefix string) []byte {
		k := make([]byte, keyLimit)
		copy(k, prefix)
		for i := len(prefix); i < keyLimit; i++ {
			k[i] = 'k'
		}
		return k
	}
	goKey := mkKey("diff_ksz_go_")
	cKey := mkKey("diff_ksz_c_")
	val := []byte("boundary-value")

	if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		tx.Set(gofdb.Key(goKey), val)
		return nil, nil
	}); err != nil {
		t.Fatalf("go write at key limit: %v", err)
	}
	mustCGo(t, func(tx cgofdb.Transaction) { tx.Set(cgofdb.Key(cKey), val) })

	goByC := cgoGet(t, goKey)
	cByC := cgoGet(t, cKey)
	if !bytes.Equal(goByC, cByC) {
		t.Fatalf("key at KEY_SIZE_LIMIT: persisted differs (go accepted=%v c accepted=%v) — knob drift?",
			goByC != nil, cByC != nil)
	}
	if !bytes.Equal(goByC, val) {
		t.Fatalf("key at KEY_SIZE_LIMIT must be accepted by both; got %x want %x", goByC, val)
	}
}

func cgoGet(t *testing.T, key []byte) []byte {
	t.Helper()
	v, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		return tx.Get(cgofdb.Key(key)).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("cgo get %q: %v", key, err)
	}
	if b, ok := v.([]byte); ok {
		return b
	}
	return nil
}

func goGet(t *testing.T, key []byte) []byte {
	t.Helper()
	v, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		return tx.Get(gofdb.Key(key)).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("go get %q: %v", key, err)
	}
	if b, ok := v.([]byte); ok {
		return b
	}
	return nil
}

func mustCGo(t *testing.T, f func(tx cgofdb.Transaction)) {
	t.Helper()
	if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		f(tx)
		return nil, nil
	}); err != nil {
		t.Fatalf("cgo txn: %v", err)
	}
}
