//go:build stress

// The 10M-row scales, in their own Bazel target (stress_10m_test) so that the
// stress_test target advertises exactly the scales it runs: 10K / 100K / 1M.
//
// A single-node FDB testcontainer cannot sustain 10M rows of writes: sustained
// load wedged the container, and the client (no default transaction timeout,
// matching C++ and Java) retried the unreachable cluster to the 1h deadline. So
// this binary refuses to start a container and runs only against a real cluster:
//
//	just stress-10m /etc/foundationdb/fdb.cluster

package stress_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func init() { realClusterOnly = true }

func TestFDB_Stress_10M(t *testing.T) {
	runStressSuite(t, "10m", 10_000_000)
}

func TestFDB_Ingest_10M(t *testing.T) {
	h := newStressHarness(t, "ingest10m")

	// Minimal schema: single PK, no secondary indexes.
	h.createSchema(`
		CREATE TABLE items (
			id BIGINT,
			val BIGINT,
			PRIMARY KEY (id)
		)
	`)

	n := 10_000_000
	start := time.Now()
	inserted := 0
	batchSize := 500
	lastLog := time.Now()

	for offset := 0; offset < n; offset += batchSize {
		end := offset + batchSize
		if end > n {
			end = n
		}
		var rows []string
		for i := offset; i < end; i++ {
			rows = append(rows, fmt.Sprintf("(%d, %d)", i, i*7))
		}
		stmt := fmt.Sprintf("INSERT INTO items VALUES %s", strings.Join(rows, ", "))

		var lastErr error
		for attempt := range 5 {
			batchStart := time.Now()
			if _, lastErr = h.db.ExecContext(context.Background(), stmt); lastErr == nil {
				batchDur := time.Since(batchStart)
				if batchDur > 3*time.Second {
					t.Logf("  SLOW batch [%d..%d): %v", offset, end, batchDur)
				}
				break
			}
			t.Logf("  RETRY %d batch [%d..%d): %v", attempt+1, offset, end, lastErr)
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
		if lastErr != nil {
			t.Fatalf("INSERT batch [%d..%d) failed after retries: %v (inserted %d/%d so far)", offset, end, lastErr, inserted, n)
		}
		inserted += end - offset

		if time.Since(lastLog) > 10*time.Second {
			elapsed := time.Since(start)
			rate := float64(inserted) / elapsed.Seconds()
			t.Logf("  progress: %d/%d (%.1f%%) in %v (%.0f rows/s)", inserted, n, float64(inserted)*100/float64(n), elapsed, rate)
			lastLog = time.Now()
		}
	}

	elapsed := time.Since(start)
	t.Logf("INSERT complete: %d rows in %v (%.0f rows/s)", inserted, elapsed, float64(inserted)/elapsed.Seconds())

	// Verify count.
	var count int64
	if err := h.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM items").Scan(&count); err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	if count != int64(n) {
		t.Fatalf("COUNT(*) = %d, want %d", count, n)
	}
	t.Logf("COUNT(*) verified: %d", count)

	// Needle in haystack: unindexed filter on val column.
	r := h.timeQuery("SELECT id FROM items WHERE val + 0 = 35 ORDER BY id")
	r.mustSucceed(t, "sparse filter val+0=35")
	r.expectRows(t, "sparse filter val+0=35", 1) // only id=5 has val=35
	t.Logf("sparse filter at 10M: %v", r.Duration)
}
