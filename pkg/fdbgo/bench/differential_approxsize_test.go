package bench

import (
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// TestDifferential_ApproximateSize compares GetApproximateSize() — the client-side running
// transaction-size estimate apps poll to decide when to commit/split a large transaction — between
// the pure-Go client and libfdb_c, op by op. The estimate is computed entirely client-side from the
// accumulated mutations + conflict ranges (no server round-trip), so it must match libfdb_c exactly:
// a Go app that splits a transaction at a byte threshold would split at a different point than the
// equivalent C/Java app if the accounting diverges.
//
// C++ accounts approximateSize INCREMENTALLY per op (ReadYourWrites.actor.cpp):
//   - set / atomicOp:  k + v + sizeof(MutationRef) + (writeConflict ? sizeof(KeyRangeRef) + 2k+1 : 0)   (:2289/:2337)
//   - clear(range):    range + sizeof(MutationRef) + (writeConflict ? sizeof(KeyRangeRef) + range : 0)  (:2374)
//   - clear(KEY):      range + sizeof(KeyRangeRef) + (writeConflict ? sizeof(KeyRangeRef) + range : 0)  (:2431)
//     ^ a single-key clear's MUTATION part is charged sizeof(KeyRangeRef) (24), NOT
//     sizeof(MutationRef) (44) — it is modeled as a range entry in the write map.
//   - add{Read,Write}ConflictRange: range + sizeof(KeyRangeRef)                                          (:1978/:2492)
//
// where sizeof(MutationRef)=44, sizeof(KeyRangeRef)=24 (StringRef is packed to 12 bytes by
// Arena.h:370 #pragma pack(push,4)), range.expectedSize()=len(begin)+len(end), and a single-key
// clear's range is [key, key+\x00) so range.expectedSize() = 2*len(key)+1.
func TestDifferential_ApproximateSize(t *testing.T) {
	t.Parallel()
	pfx := []byte(fmt.Sprintf("apxsize_%d_%s_", os.Getpid(), t.Name()))
	k := func(s string) []byte { return append(append([]byte{}, pfx...), s...) }

	goTr, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go CreateTransaction: %v", err)
	}
	defer goTr.Cancel()
	cTr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo CreateTransaction: %v", err)
	}
	defer cTr.Cancel()

	gkr := func(a, b []byte) gofdb.KeyRange { return gofdb.KeyRange{Begin: gofdb.Key(a), End: gofdb.Key(b)} }
	ckr := func(a, b []byte) cgofdb.KeyRange { return cgofdb.KeyRange{Begin: cgofdb.Key(a), End: cgofdb.Key(b)} }

	// Each step applies the SAME op (same key/value lengths) to both transactions; after each, the
	// cumulative GetApproximateSize must be identical. The first step where they differ pinpoints the
	// op whose accounting diverges.
	steps := []struct {
		name string
		g    func()
		c    func()
	}{
		{"set", func() { goTr.Set(gofdb.Key(k("aaaa")), []byte("val1")) }, func() { cTr.Set(cgofdb.Key(k("aaaa")), []byte("val1")) }},
		{"atomic_add", func() { goTr.Add(gofdb.Key(k("counter")), make([]byte, 8)) }, func() { cTr.Add(cgofdb.Key(k("counter")), make([]byte, 8)) }},
		{"clear_range", func() { goTr.ClearRange(gkr(k("r0"), k("r9"))) }, func() { cTr.ClearRange(ckr(k("r0"), k("r9"))) }},
		{"clear_single_key", func() { goTr.Clear(gofdb.Key(k("singlekey"))) }, func() { cTr.Clear(cgofdb.Key(k("singlekey"))) }},
		{"add_read_conflict", func() { _ = goTr.AddReadConflictRange(gkr(k("rc0"), k("rc9"))) }, func() { _ = cTr.AddReadConflictRange(ckr(k("rc0"), k("rc9"))) }},
		{"add_write_conflict", func() { _ = goTr.AddWriteConflictRange(gkr(k("wc0"), k("wc9"))) }, func() { _ = cTr.AddWriteConflictRange(ckr(k("wc0"), k("wc9"))) }},
		{"second_single_key_clear", func() { goTr.Clear(gofdb.Key(k("another1"))) }, func() { cTr.Clear(cgofdb.Key(k("another1"))) }},
	}

	for _, s := range steps {
		s.g()
		s.c()
		goSize, err := goTr.GetApproximateSize().Get()
		if err != nil {
			t.Fatalf("go GetApproximateSize after %q: %v", s.name, err)
		}
		cSize, err := cTr.GetApproximateSize().Get()
		if err != nil {
			t.Fatalf("cgo GetApproximateSize after %q: %v", s.name, err)
		}
		if goSize != cSize {
			t.Errorf("GetApproximateSize diverges after %q: go=%d cgo=%d (delta=%+d)", s.name, goSize, cSize, goSize-cSize)
		} else {
			t.Logf("after %-24s go=cgo=%d", s.name, goSize)
		}
	}
}

// FuzzDifferential_ApproximateSize hardens the GetApproximateSize accounting fix across RANDOM op
// sequences: it applies the same fuzz-decoded ops (Set / single-key Clear / ClearRange / atomics /
// CompareAndClear) to a go-txn and a cgo-txn one at a time and asserts GetApproximateSize stays
// byte-identical after every op. A single divergent op (a missed mutation/conflict charge, a
// single-key-clear vs range-clear mischarge) is caught with a minimized reproducer. No commit: the
// estimate is purely client-side, so this is fast (no RPC) and exercises only the accounting.
func FuzzDifferential_ApproximateSize(f *testing.F) {
	f.Add([]byte{fzSet, 0, 1, 0xaa, fzClear, 1, fzAdd, 0, 1, 0x01})
	f.Add([]byte{fzClear, 0, fzClear, 1, fzClear, 2, fzClearRange, 0, 3})
	f.Add([]byte{fzSet, 0, 2, 0xde, 0xad, fzClearRange, 0, 3, fzSet, 1, 1, 0xff})
	f.Add([]byte{fzByteMax, 0, 3, 0x6d, 0x6d, 0x6d, fzCompareAndClear, 0, 1, 0x7a})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13})

	f.Fuzz(func(t *testing.T, data []byte) {
		batches := decodeFuzzOps(data)
		if len(batches) == 0 || len(batches[0]) == 0 {
			return
		}
		ops := batches[0]
		pfx := fmt.Sprintf("apxfuzz_%d_", os.Getpid())

		goTr, err := goClient.CreateTransaction()
		if err != nil {
			t.Fatalf("go CreateTransaction: %v", err)
		}
		defer goTr.Cancel()
		cTr, err := cgoClient.CreateTransaction()
		if err != nil {
			t.Fatalf("cgo CreateTransaction: %v", err)
		}
		defer cTr.Cancel()

		for i := range ops {
			applyGo(goTr, ops[i:i+1], pfx)
			applyC(cTr, ops[i:i+1], pfx)
			goSize, gerr := goTr.GetApproximateSize().Get()
			if gerr != nil {
				t.Fatalf("go GetApproximateSize after op %d: %v", i, gerr)
			}
			cSize, cerr := cTr.GetApproximateSize().Get()
			if cerr != nil {
				t.Fatalf("cgo GetApproximateSize after op %d: %v", i, cerr)
			}
			if goSize != cSize {
				t.Fatalf("GetApproximateSize diverges after op %d (kind=%d keyIdx=%d operandLen=%d): go=%d cgo=%d",
					i, ops[i].kind, ops[i].keyIdx, len(ops[i].operand), goSize, cSize)
			}
		}
	})
}
