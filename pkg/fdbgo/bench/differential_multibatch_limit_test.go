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
// division is not observable THROUGH THIS PATH: Apple's Go binding keeps iteration/more/the
// advancing begin-key private on an unexported RangeIterator with no trace hook, and this
// repo's cgo adapter implements SetTraceLog as a documented no-op (libfdbc/backend.go:668)
// because there is nothing underneath to forward.
//
// It IS observable through a different one. libfdbc.CGetRangeBatch (libfdbc/range_cref.go)
// issues a single raw fdb_transaction_get_range with target_bytes and iteration exposed, and
// libfdbc:TestLibFDBC_RangeBatchDivision drives that loop to measure exactly where C splits.
// That test is tag-gated (`-tags libfdbc`) and runs in the cross-client differential workflow
// rather than in the default bazel build, which is why the per-batch comparison lives there and
// this test — which runs everywhere — stays on row sets.
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
//   - a division that differs from C's while producing identical rows — not caught HERE, and
//     it is not a wire-visible property: the per-batch read-conflict ranges union to the same
//     consumed extent either way. It is nonetheless REAL and now measured:
//     libfdbc:TestLibFDBC_RangeBatchDivision records C dividing 60 200-byte rows as
//     [2]x30 under SMALL where the pure-Go client takes [10]x6, because C derives target_bytes
//     per mode while Go pins 80000 for every mode. Booked in TODO.md Phase 12.
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
		name string
		// Named constants on BOTH sides rather than one int cast into each. The two enums do
		// coincide today (Apple generated.go vs fdb/range.go, identical -1..5), but a comment
		// asserting an invariant the compiler could enforce is the shape that rots — and the
		// cgo side's mode is otherwise unverified here, so a silent mismatch would still pass
		// on row sets and this test would report agreement between two DIFFERENT requests.
		goMode gofdb.StreamingMode
		cMode  cgofdb.StreamingMode
		limit  int
		want   int // rows expected
	}{
		// Bounded modes with a limit: the only shape where a LATER batch is clamped by the
		// leftover budget. Go divides these 100/100/50, 10/10/5 and so on.
		{"medium/250", gofdb.StreamingModeMedium, cgofdb.StreamingModeMedium, 250, 250},
		{"medium/199", gofdb.StreamingModeMedium, cgofdb.StreamingModeMedium, 199, 199},
		{"medium/201", gofdb.StreamingModeMedium, cgofdb.StreamingModeMedium, 201, 201},
		{"small/25", gofdb.StreamingModeSmall, cgofdb.StreamingModeSmall, 25, 25},
		{"small/175", gofdb.StreamingModeSmall, cgofdb.StreamingModeSmall, 175, 175},
		// Iterator mode past the doubling phase, limited mid-progression.
		{"iterator/300", gofdb.StreamingModeIterator, cgofdb.StreamingModeIterator, 300, 300},
		{"iterator/nolimit", gofdb.StreamingModeIterator, cgofdb.StreamingModeIterator, 0, n},
		// Limit exceeding the data: the range exhausts before the budget does.
		{"small/1000", gofdb.StreamingModeSmall, cgofdb.StreamingModeSmall, 1000, n},
		// EXACT with a budget spanning what would be many C batches. Go serves this in ONE
		// fetch by construction; C is free to split it. Identical rows either way is the claim.
		{"exact/600", gofdb.StreamingModeExact, cgofdb.StreamingModeExact, 600, n},
		{"exact/599", gofdb.StreamingModeExact, cgofdb.StreamingModeExact, 599, 599},
	} {
		c := c
		goKeys, goBatches, goCode := goDrain(gofdb.RangeOptions{
			Mode: c.goMode, Limit: c.limit,
		})
		cgoKeys, cgoCode := cgoDrain(cgofdb.RangeOptions{
			Mode: c.cMode, Limit: c.limit,
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
		if c.goMode != gofdb.StreamingModeExact && goBatches < 2 {
			t.Errorf("%s: the Go drain took %d batches — this case is vacuous as a BOUNDARY "+
				"differential unless it crosses at least one", c.name, goBatches)
		}
		t.Logf("MEASURED %-16s go{rows:%d batches:%d} cgo{rows:%d} — row sets identical",
			c.name, len(goKeys), goBatches, len(cgoKeys))
	}
}
