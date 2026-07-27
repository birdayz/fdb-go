//go:build stress && realcluster

// The 10M-row scales. They carry `realcluster` ON TOP OF `stress` so they are
// REGISTERED only in an environment that can actually run them, rather than
// registered everywhere and skipped.
//
// Why they cannot run on the default harness: a single-node FDB testcontainer
// cannot sustain 10M rows of writes. Five parallelism configs x 1M sustained
// writes already drove one container past its throughput ceiling until it
// stopped responding, and the client (correctly, matching C++/Java: no default
// transaction timeout) retried the now-unreachable cluster forever, hanging the
// whole suite to the 1h test deadline. 10M serial inserts need ~55min at Docker
// rates even when the node stays healthy, and degrade it along the way.
//
// A `t.Skip` here would be worse than the tag: it makes the package advertise
// four scales and run three, so `-test.v` output reads as coverage that does
// not exist. The tag makes the advertised list equal the executed list — under
// the default `stress` tag this package is exactly 10K / 100K / 1M.
//
// Running them, against a REAL cluster (a testcontainer would just reproduce
// the ceiling above):
//
//	FDB_STRESS_CLUSTER_FILE=/etc/foundationdb/fdb.cluster \
//	  go test -tags 'stress realcluster' ./pkg/relational/sqldriver/stress/ \
//	  -run 'TestFDB_(Stress|Ingest)_10M' -timeout 3h -v
//
// FDB_STRESS_CLUSTER_FILE is what points TestMain at the real cluster instead
// of starting a container; see stress_test.go. Without it these tests would run
// against a Docker node and hit the very ceiling this tag exists to avoid.

package stress_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFDB_Stress_10M(t *testing.T) {
	runStressSuite(t, "10m", 10_000_000)
}

func TestFDB_Ingest_10M(t *testing.T) {
	h := newStressHarness(t, "ingest10m")

	// Minimal schema: single PK, no secondary indexes.
	h.createSchema(`
		CREATE TABLE items (
			id BIGINT NOT NULL,
			val BIGINT NOT NULL,
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
