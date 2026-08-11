package fdb_test

import (
	"fmt"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestByteDividedScanConflictsOverExactlyWhatItConsumed answers the one question the
// byte-division port raises that a division measurement cannot: moving batch boundaries moves
// the per-batch read-conflict ranges, so does a partially-drained scan still conflict over
// exactly the rows it consumed?
//
// Every batch takes its own read-conflict range, clamped to the extent it returned (RFC-121), so
// the union of those ranges is what decides whether a concurrent writer aborts this transaction.
// The port changed WHERE those boundaries fall — SMALL went from a flat 10-row page to a
// 256-byte target — which changes how many ranges there are and where each one ends. The union
// must still be exactly the consumed prefix. Two ways for that to be wrong, and they fail in
// opposite directions, so both are asserted:
//
//   - UNDER-conflicting: a concurrent write to a row this scan already READ does not abort it.
//     That is lost serializability — the transaction commits on data it saw change.
//   - OVER-conflicting: a concurrent write BEYOND what the scan consumed aborts it anyway. That
//     is a spurious retry, and at scale a livelock; it is also what a scan that conflicted over
//     its whole requested range rather than its consumed prefix would do.
//
// This is deliberately a LOGICAL test, not a timing one. The contention question the port raises
// is which keys conflict, and that is decided by the conflict extents, not by throughput — so it
// is answerable on a loaded machine, where a latency comparison is not.
//
// The scan is byte-divided on purpose: 100-byte values under SMALL's 256-byte target put roughly
// two rows in each fetch, so a partial drain crosses several boundaries and the union under test
// is a union of many ranges rather than one.
func TestByteDividedScanConflictsOverExactlyWhatItConsumed(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	const (
		n         = 60
		valueLen  = 100
		consumeN  = 6 // several batches in, and far short of the whole range
		farBeyond = 50
	)

	for _, arm := range []struct {
		name string
		// writeIdx is the row a concurrent transaction updates while the scan is open.
		writeIdx int
		// wantConflict is whether the scanning transaction must fail to commit.
		wantConflict bool
		why          string
	}{
		{
			name:         "inside_consumed_prefix",
			writeIdx:     1,
			wantConflict: true,
			why: "row 1 was returned by the scan's first batch, so it is in the read set and a " +
				"concurrent write to it must abort the scanning transaction",
		},
		{
			name:         "far_beyond_consumed_prefix",
			writeIdx:     farBeyond,
			wantConflict: false,
			why: "row 50 was never fetched — the scan stopped after " +
				"6 rows — so it is outside the read set and must NOT abort the scanning transaction",
		},
	} {
		arm := arm
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			pfx := fmt.Sprintf("bdconflict_%s_", arm.name)
			key := func(i int) gofdb.Key { return gofdb.Key(fmt.Sprintf("%s%04d", pfx, i)) }
			// A key outside the scanned range, so the scanning transaction is a WRITE
			// transaction and its commit is actually resolved.
			sentinel := gofdb.Key(pfx + "~sentinel")

			if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
				for i := 0; i < n; i++ {
					tr.Set(key(i), make([]byte, valueLen))
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			// The scanning transaction, driven WITHOUT the retry loop: Transact would retry a
			// conflict away and report success, which is exactly the signal under test.
			scan, err := db.CreateTransaction()
			if err != nil {
				t.Fatalf("CreateTransaction(scan): %v", err)
			}
			it := scan.GetRange(
				gofdb.KeyRange{Begin: gofdb.Key(pfx), End: gofdb.Key(pfx + "~")},
				gofdb.RangeOptions{Mode: gofdb.StreamingModeSmall},
			).Iterator()

			batches := 0
			it.SetTraceLog(func(_, _, returned int, _ bool, _ error) {
				if returned > 0 {
					batches++
				}
			})
			consumed := 0
			for consumed < consumeN && it.Advance() {
				if _, err := it.Get(); err != nil {
					t.Fatalf("iterate: %v", err)
				}
				consumed++
			}
			if consumed != consumeN {
				t.Fatalf("scan consumed %d rows, want %d — the fixture did not supply enough rows",
					consumed, consumeN)
			}
			// NON-VACUITY. If the whole prefix arrived in ONE batch this test degenerates into
			// the single-range case and says nothing about a union of moved boundaries.
			if batches < 2 {
				t.Fatalf("consuming %d rows took %d batch(es): the scan never crossed a boundary, "+
					"so the union of per-batch conflict ranges under test is a single range and "+
					"this arm cannot see a boundary-placement bug", consumeN, batches)
			}

			// A concurrent, independently committed write landing while the scan is open.
			if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
				tr.Set(key(arm.writeIdx), []byte("concurrent"))
				return nil, nil
			}); err != nil {
				t.Fatalf("concurrent write: %v", err)
			}

			scan.Set(sentinel, []byte("x"))
			commitErr := scan.Commit().Get()

			code, isFDBErr := codeOf(commitErr)
			gotConflict := commitErr != nil && isFDBErr && code == 1020 // not_committed
			if commitErr != nil && !gotConflict {
				t.Fatalf("scan commit failed with an unexpected error: %v", commitErr)
			}
			if gotConflict != arm.wantConflict {
				verb := "did not conflict"
				if gotConflict {
					verb = "conflicted"
				}
				t.Fatalf("scan consumed %d rows across %d batches, concurrent write hit row %d, and "+
					"the scan %s (want conflict=%v). %s. The union of the per-batch read-conflict "+
					"ranges must be exactly the consumed prefix: under-conflicting loses "+
					"serializability, over-conflicting turns every neighbouring write into a "+
					"spurious retry. Batch boundaries are byte-driven since the GetRangeLimits "+
					"port, so a change here means the conflict extent stopped tracking them",
					consumed, batches, arm.writeIdx, verb, arm.wantConflict, arm.why)
			}
			t.Logf("MEASURED %-26s consumed=%d batches=%d writeIdx=%d conflict=%v",
				arm.name, consumed, batches, arm.writeIdx, gotConflict)
		})
	}
}
