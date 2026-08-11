package bench

import (
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Go-vs-libfdb_c differential for a range drain that CROSSES BATCH BOUNDARIES under a row
// limit. Every existing range-limit differential in this package compares single-fetch results:
// TestDifferential_RangeRead drives both sides through GetSliceWithError, and the pure-Go
// GetSliceWithError issues exactly one doRangeWithLimit with the full budget (range_result.go:93)
// — it never enters the iterator's batch loop at all. TestDifferential_ExactModeWithoutLimits
// drains iterators but over three seeded rows, comparing error codes only. So the batch loop's
// behaviour under a limit had no cross-client pin.
//
// WHAT THIS CAN AND CANNOT ASSERT, stated plainly because it bounds the claim:
//
// The Go side's batch DIVISION is observable (SetTraceLog) and is asserted here as a
// non-vacuity guard — a differential that happened to fit in one batch would compare nothing
// about boundaries while reading exactly like a passing boundary test. The libfdb_c side's
// division is NOT observable: Apple's Go binding keeps iteration/more/the advancing begin-key
// private on an unexported RangeIterator with no trace hook, and this repo's cgo adapter
// implements SetTraceLog as a documented no-op (libfdbc/backend.go:668) because there is
// nothing underneath to forward. Exposing it would require a new raw CGetRange binding
// alongside CGetMappedRange.
//
// So the cross-client assertion is on the final ROW SET, not on where each client split it.
// What that costs is narrow, and worth naming precisely: the two clients could agree on every
// row while dividing it differently, and this test would pass. It is not blind to mis-splitting
// in general, though — the failure modes a mis-split produces divide into ones this catches and
// ones its sibling catches:
//
//   - a wrong Go division (over-reading past the budget, a short final batch that should have
//     been merged, an off-by-one at the limit) — caught by
//     fdb:TestRangeIterator_BoundedModeReDerivesRemainingBudget, which asserts the exact
//     per-batch counts against real FDB;
//   - a wrong continuation across a boundary (a gap or a repeat) — caught HERE, because it
//     changes which rows come back, and independently by the row-identity arms;
//   - a division that differs from C's while producing identical rows — NOT caught by anything,
//     and it is not currently a wire-visible property: the per-batch read-conflict ranges union
//     to the same consumed extent either way.
//
// The last bullet is what a per-batch C differential would add, and it becomes load-bearing the
// moment a byte-dimension budget lands (see fdb:TestRangeIterator_ExactModeIsStructurallySingleBatch).
func TestDifferential_LimitedIteratorMultiBatchRowSets(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("diffmb_%d_", os.Getpid())
	const n = 600

	// Seed through the pure-Go client; both clients share the cluster.
	const perTx = 200
	for start := 0; start < n; start += perTx {
		start := start
		if c := goErrCode(func(tx gofdb.Transaction) error {
			for i := start; i < start+perTx && i < n; i++ {
				tx.Set(gofdb.Key(fmt.Sprintf("%s%06d", pfx, i)), []byte("v"))
			}
			return nil
		}); c != 0 {
			t.Fatalf("seed [%d,%d): error code %d", start, start+perTx, c)
		}
	}

	// The two drain loops differ deliberately: Apple's Advance() returns TRUE on error so the
	// following Get() can surface it, while the pure-Go Advance() returns FALSE and the error is
	// only reachable from Get() afterwards. Draining the pure-Go iterator with Apple's idiom
	// reports zero rows and no error — the mismeasurement the sibling probe documents.
	goDrain := func(opts gofdb.RangeOptions) (keys []string, batches int, code int) {
		code = goErrCode(func(tx gofdb.Transaction) error {
			keys, batches = nil, 0
			it := tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(pfx), End: gofdb.Key(pfx + "~")}, opts).Iterator()
			it.SetTraceLog(func(iteration, requested, returned int, more bool, err error) { batches++ })
			for it.Advance() {
				kv, err := it.Get()
				if err != nil {
					return err
				}
				keys = append(keys, string(kv.Key))
			}
			_, err := it.Get()
			return err
		})
		return keys, batches, code
	}
	cgoDrain := func(opts cgofdb.RangeOptions) (keys []string, code int) {
		code = cgoErrCode(func(tx cgofdb.Transaction) error {
			keys = nil
			it := tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(pfx), End: cgofdb.Key(pfx + "~")}, opts).Iterator()
			for it.Advance() {
				kv, err := it.Get()
				if err != nil {
					return err
				}
				keys = append(keys, string(kv.Key))
			}
			return nil
		})
		return keys, code
	}

	for _, c := range []struct {
		name  string
		mode  int // shared numbering: WantAll -1, Iterator 0, Exact 1, Small 2, Medium 3
		limit int
		want  int // rows expected
	}{
		// Bounded modes with a limit: the only shape where a LATER batch is clamped by the
		// leftover budget. Go divides these 100/100/50, 10/10/5 and so on.
		{"medium/250", 3, 250, 250},
		{"medium/199", 3, 199, 199},
		{"medium/201", 3, 201, 201},
		{"small/25", 2, 25, 25},
		{"small/175", 2, 175, 175},
		// Iterator mode past the doubling phase, limited mid-progression.
		{"iterator/300", 0, 300, 300},
		{"iterator/nolimit", 0, 0, n},
		// Limit exceeding the data: the range exhausts before the budget does.
		{"small/1000", 2, 1000, n},
		// EXACT with a budget spanning what would be many C batches. Go serves this in ONE
		// fetch by construction; C is free to split it. Identical rows either way is the claim.
		{"exact/600", 1, 600, n},
		{"exact/599", 1, 599, 599},
	} {
		c := c
		goKeys, goBatches, goCode := goDrain(gofdb.RangeOptions{
			Mode: gofdb.StreamingMode(c.mode), Limit: c.limit,
		})
		cgoKeys, cgoCode := cgoDrain(cgofdb.RangeOptions{
			Mode: cgofdb.StreamingMode(c.mode), Limit: c.limit,
		})

		if goCode != cgoCode {
			t.Errorf("%s: DIVERGENCE error code go=%d cgo=%d", c.name, goCode, cgoCode)
			continue
		}
		if goCode != 0 {
			t.Errorf("%s: both clients errored with %d, want success", c.name, goCode)
			continue
		}
		if len(goKeys) != c.want {
			t.Errorf("%s: go returned %d rows, want %d", c.name, len(goKeys), c.want)
		}
		if len(goKeys) != len(cgoKeys) {
			t.Errorf("%s: DIVERGENCE row count go=%d cgo=%d", c.name, len(goKeys), len(cgoKeys))
			continue
		}
		for i := range goKeys {
			if goKeys[i] != cgoKeys[i] {
				t.Errorf("%s: DIVERGENCE at row %d: go=%q cgo=%q — the clients disagree on which "+
					"rows a limited multi-batch drain returns, which is a continuation bug on "+
					"whichever side moved its scan bound wrongly", c.name, i, goKeys[i], cgoKeys[i])
				break
			}
		}
		// NON-VACUITY. Except for EXACT — which is single-batch on the Go side by construction,
		// the property fdb:TestRangeIterator_ExactModeIsStructurallySingleBatch pins — a drain
		// that fit in ONE batch compares nothing about boundaries. Guard it, or a future change
		// to the mode table silently turns this into a single-fetch comparison that still passes.
		if c.mode != 1 && goBatches < 2 {
			t.Errorf("%s: the Go drain took %d batches — this case is vacuous as a BOUNDARY "+
				"differential unless it crosses at least one", c.name, goBatches)
		}
		t.Logf("MEASURED %-16s go{rows:%d batches:%d} cgo{rows:%d} — row sets identical",
			c.name, len(goKeys), goBatches, len(cgoKeys))
	}
}
