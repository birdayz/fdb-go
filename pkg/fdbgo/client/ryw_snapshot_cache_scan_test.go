package client

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
)

// TestSnapshotCache_LargeScanStaysFragmented drives the shape that made the
// snapshot cache quadratic: ONE transaction walking a large subspace with
// bounded snapshot range reads, which is what an unbounded
// fdb.RangeResult iterator issues under any streaming mode — a sequence of
// Snapshot().GetRange calls whose begin advances past the last key returned.
// Every one of those reads lands in snapshotCache.insert.
//
// The pin is structural, not a stopwatch: each fetch must leave its own
// fragment holding only its own rows. That is C++'s layout (SnapshotCache.h:376
// inserts one IndexedSet node per fetched range and never merges a neighbour
// in), and it is what makes the scan linear. Coalescing the fetches into one
// entry — the defect — collapses the fragment count to 1 and makes the Nth
// fetch copy and re-sort the N-1 pages before it.
//
// Measured on this path, 500 rows per fetch, real FDB, coalescing vs fragments:
//
//	rows    coalesced        fragmented
//	10000    12 MiB /  12ms   2 MiB /   7ms
//	20000    45 MiB /  33ms   4 MiB /   7ms
//	40000   173 MiB / 113ms   8 MiB /  13ms
//	80000   677 MiB / 361ms  16 MiB /  28ms
//
// The allocation column is the honest measure of the defect (a clean 4x per
// doubling against a clean 2x), but TotalAlloc is process-global and this
// package runs its tests in parallel, so asserting on it would be flaky. The
// fragment count is the same property observed deterministically. What this
// test therefore CANNOT catch: a quadratic introduced somewhere other than
// cross-fetch coalescing — work done inside a single fragment would leave the
// counts intact.
//
// Retention is deliberately NOT asserted here. C++ keeps every row of every
// snapshot range read for the life of the transaction as well (the SnapshotCache
// is arena-allocated with no eviction), so bounding it would be a divergence.
func TestSnapshotCache_LargeScanStaysFragmented(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	const (
		rows    = 20000
		perTx   = 4000
		perScan = 500
	)
	prefix := t.Name() + "_"
	key := func(i int) []byte { return []byte(fmt.Sprintf("%s%07d", prefix, i)) }

	for base := 0; base < rows; base += perTx {
		lo := base
		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			for i := lo; i < lo+perTx; i++ {
				tx.Set(key(i), []byte("v"))
			}
			return nil, nil
		})
		g.Expect(err).ToNot(gomega.HaveOccurred())
	}

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		end := append([]byte(prefix), 0xFF)
		cur := []byte(prefix)
		got, fetches := 0, 0
		for bytes.Compare(cur, end) < 0 {
			kvs, _, err := tx.Snapshot().GetRange(ctx, cur, end, perScan)
			if err != nil {
				return nil, err
			}
			// Counted before the exhaustion check: a trailing zero-row fetch
			// still marks its range known and so still leaves a fragment.
			fetches++
			if len(kvs) == 0 {
				break
			}
			got += len(kvs)
			cur = append(append([]byte{}, kvs[len(kvs)-1].Key...), 0)
		}
		g.Expect(got).To(gomega.Equal(rows))
		g.Expect(fetches).To(gomega.BeNumerically(">=", rows/perScan))

		// One fragment per fetch, each holding only that fetch's rows.
		entries := tx.ryw.serverCache.entries
		g.Expect(entries).To(gomega.HaveLen(fetches),
			"snapshot cache coalesced fetches — a sequential scan is quadratic again")
		total := 0
		for _, e := range entries {
			g.Expect(len(e.kvs)).To(gomega.BeNumerically("<=", perScan),
				"a fragment absorbed more than one fetch")
			total += len(e.kvs)
		}
		g.Expect(total).To(gomega.Equal(rows))

		// Fragmentation must not cost coverage: the whole subspace still reads
		// back from the cache as one fully-known range.
		cached, ok := tx.ryw.serverCache.getRangeKVs([]byte(prefix), end)
		g.Expect(ok).To(gomega.BeTrue())
		g.Expect(cached).To(gomega.HaveLen(rows))
		g.Expect(cached[0].Key).To(gomega.Equal(key(0)))
		g.Expect(cached[rows-1].Key).To(gomega.Equal(key(rows - 1)))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}
