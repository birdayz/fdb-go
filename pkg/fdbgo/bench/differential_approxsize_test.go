package bench

import (
	"fmt"
	"os"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
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
//     ^ a single-key clear's MUTATION part is charged sizeof(KeyRangeRef) (32), NOT
//     sizeof(MutationRef) (48) — it is modeled as a range entry in the write map.
//   - add{Read,Write}ConflictRange: range + sizeof(KeyRangeRef)                                          (:1978/:2492)
//
// where sizeof(MutationRef)=48, sizeof(KeyRangeRef)=32, range.expectedSize()=len(begin)+len(end),
// and a single-key clear's range is [key, key+\x00) so range.expectedSize() = 2*len(key)+1.
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
