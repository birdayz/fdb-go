package fdb_test

import (
	"fmt"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestRangeIterator_DivisionIsRowDrivenNotByteDriven pins the pure-Go client's batch division
// as a function of ROW COUNT ALONE, invariant under row SIZE.
//
// It records a DIVERGENCE from libfdb_c, deliberately, because the divergence is currently the
// behaviour and an undocumented divergence is how one becomes permanent. libfdb_c derives a
// per-mode byte target from mode_bytes_array (bindings/c/fdb_c.cpp:1002) — SMALL 256,
// MEDIUM 1000, LARGE 4096, SERIAL 80000 — and ends a call when limits.isReached() over
// GetRangeLimits{rows, bytes} (NativeAPI.actor.cpp:4761). The pure-Go client has no byte
// dimension in that decision: batchSize is row-only, so identical row COUNTS divide identically
// no matter how fat the rows are. Measured side by side over the same 60 rows of 200 bytes,
// libfdbc:TestLibFDBC_RangeBatchDivision records C dividing SMALL as [2]x30 where this divides
// it [10]x6.
//
// WHY A BYTE CEILING DOES NOT EXPLAIN IT, since that was the first answer and it was wrong.
// A byte limit IS sent on every request — LimitBytes: replyByteLimit, 80000
// (client/readpath.go:1102, constant at :27) — and the storage server does truncate on it. It
// changes nothing here because rangeScanImpl (client/readpath.go:669) ABSORBS a truncated
// reply and re-queries the same shard until the ROW budget is filled, so the per-request byte
// limit never reaches the iterator's division. That was measured directly: dropping
// replyByteLimit from 80000 to 256 — a 312x reduction, below SMALL's own C target — left the
// divisions below byte-for-byte identical. The reply CEILING and the per-mode TARGET are
// different things, and conflating them is what makes the byte dimension look absent.
//
// DIRECTION OF THIS GUARD: it asserts today's row-only behaviour, so it FAILS when the
// GetRangeLimits{rows,bytes} port booked in TODO.md Phase 12 lands. That is intended. At that
// point these divisions must converge on libfdb_c's, and this test's job becomes asserting the
// converged values rather than these — do not relax it, rewrite it against the C table.
func TestRangeIterator_DivisionIsRowDrivenNotByteDriven(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	const n = 60

	// The same row COUNT at two very different row SIZES. 200-byte rows put the range at
	// 12 KB — far past SMALL's 256-byte and MEDIUM's 1000-byte C targets, so a byte-aware
	// client MUST divide the two differently. A row-driven one cannot tell them apart, and
	// that indistinguishability is exactly what this pins.
	for _, seed := range []struct {
		name     string
		valueLen int
	}{
		{"thin", 1},
		{"fat", 200},
	} {
		seed := seed
		t.Run(seed.name, func(t *testing.T) {
			t.Parallel()
			pfx := fmt.Sprintf("bytediv_%s_", seed.name)
			if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
				for i := 0; i < n; i++ {
					tr.Set(gofdb.Key(fmt.Sprintf("%s%04d", pfx, i)), make([]byte, seed.valueLen))
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			for _, c := range []struct {
				name string
				mode gofdb.StreamingMode
				want []int
			}{
				// Literal expected divisions, not a comparison against a second derivation of
				// the same rule: a check of the form "thin division == fat division" would hold
				// vacuously if the batching broke identically for both.
				{"small", gofdb.StreamingModeSmall, []int{10, 10, 10, 10, 10, 10, 0}},
				{"medium", gofdb.StreamingModeMedium, []int{60}},
				{"large", gofdb.StreamingModeLarge, []int{60}},
			} {
				c := c
				keys, batches := drainClientBatches(t, db, pfx, pfx+"~", gofdb.RangeOptions{Mode: c.mode})
				if len(keys) != n {
					t.Errorf("%s/%s: %d rows, want %d", seed.name, c.name, len(keys), n)
				}
				if len(batches) != len(c.want) {
					t.Errorf("%s/%s: division %v, want %v — the pure-Go division is row-driven, so "+
						"a change here means either the mode table moved or the byte-dimension "+
						"port landed (TODO.md Phase 12), in which case this must be rewritten to "+
						"assert libfdb_c's division rather than relaxed",
						seed.name, c.name, batches, c.want)
					continue
				}
				for i := range c.want {
					if batches[i] != c.want[i] {
						t.Errorf("%s/%s: batch[%d]=%d, want %d (division %v, want %v)",
							seed.name, c.name, i, batches[i], c.want[i], batches, c.want)
					}
				}
				t.Logf("MEASURED %-6s %-6s rows=%d division=%v", seed.name, c.name, len(keys), batches)
			}
		})
	}
}
