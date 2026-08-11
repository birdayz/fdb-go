//go:build cgo && libfdbc

package libfdbc_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	fdbclient "fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
)

// TestLibFDBC_BoundedRangeDivisionDifferential measures how the two clients divide a range read
// that carries BOTH a row limit and a per-mode byte target.
//
// The unbounded differential (TestLibFDBC_RangeBatchDivision) drives every mode with an
// unlimited row budget, so it exercises only the byte dimension. This covers the interaction:
// C++ transformRangeLimits (NativeAPI.actor.cpp:4223) puts BOTH on the request —
// `req.limit = min(REPLY_BYTE_LIMIT, limits.rows)` alongside `req.limitBytes` — and decrements
// limits.rows per batch, so a later batch can be clamped by the LEFTOVER ROW BUDGET rather than
// by the byte target.
//
// That interaction is what a Go-side test alone cannot settle. When the byte-dimension port
// landed, the pure-Go client's bounded divisions moved from the old per-mode ROW sizes
// (medium = 100 rows/batch) to byte-driven ones, and the natural but WRONG response would have
// been to rewrite the Go pin to whatever Go now produced. Rewriting a pin to match the code it
// is meant to check is not a measurement. This asserts the new division against libfdb_c
// instead, so the Go expectation is anchored to the reference rather than to itself.
//
// The invariant the original Go test protected is preserved and asserted explicitly below: the
// FINAL batch must be clamped to the remaining budget, never over-read and discarded, because
// over-reading takes a wider read-conflict range than the caller's limit justifies.
func TestLibFDBC_BoundedRangeDivisionDifferential(t *testing.T) {
	t.Parallel()

	clusterFile := startCluster(t)

	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c database: %v", err)
	}
	defer cdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	goDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("open pure-Go client: %v", err)
	}
	defer goDB.Close()

	// Same row shape as fdb:TestRangeIterator_BoundedModeReDerivesRemainingBudget — a 17-byte
	// key and a 1-byte value — so the divisions measured here are directly comparable to the
	// pin that test carries.
	const (
		prefix = "boundeddiv_"
		n      = 600
	)
	begin := []byte(prefix)
	end := []byte(prefix + "~")

	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatalf("create seed transaction: %v", err)
	}
	for i := 0; i < n; i++ {
		tr.Set([]byte(fmt.Sprintf("%s%06d", prefix, i)), []byte("v"))
	}
	if err := tr.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	tr.Close()

	// Both drivers below mirror a binding's fetch loop: iteration climbs from 1, the begin key
	// advances past the last row returned, and the ROW budget is decremented by what came back
	// so a later request carries only the remainder.
	cDivision := func(mode int, limit int) (total int, counts []int) {
		ctr, err := cdb.CreateTransaction()
		if err != nil {
			t.Fatalf("create transaction: %v", err)
		}
		defer ctr.Close()
		curBegin := append([]byte(nil), begin...)
		remaining := limit
		for iteration := 1; remaining > 0; iteration++ {
			batch, more, err := libfdbc.CGetRangeBatch(ctr, curBegin, end,
				remaining, 0 /* target_bytes: the mode's own default */, mode, iteration,
				false /* reverse */, false /* snapshot */)
			if err != nil {
				t.Fatalf("CGetRangeBatch(mode=%d, iteration=%d): %v", mode, iteration, err)
			}
			if len(batch) == 0 {
				break
			}
			counts = append(counts, len(batch))
			total += len(batch)
			remaining -= len(batch)
			if !more {
				break
			}
			last := batch[len(batch)-1].Key
			curBegin = append(append([]byte(nil), last...), 0) // keyAfter
			if iteration > 500 {
				t.Fatalf("mode=%d did not terminate within 500 fetches", mode)
			}
		}
		return total, counts
	}

	goDivision := func(mode gofdb.StreamingMode, limit int) (total int, counts []int) {
		gtx := goDB.CreateTransaction()
		defer gtx.Cancel()
		curBegin := append([]byte(nil), begin...)
		remaining := limit
		for iteration := 1; remaining > 0; iteration++ {
			target, err := gofdb.ModeTargetBytes(mode, iteration)
			if err != nil {
				t.Fatalf("ModeTargetBytes(mode=%v, iteration=%d): %v", mode, iteration, err)
			}
			batch, more, err := gtx.GetRangeWithByteTarget(ctx, curBegin, end, remaining, target, false)
			if err != nil {
				t.Fatalf("pure-Go GetRangeWithByteTarget(mode=%v, iteration=%d): %v", mode, iteration, err)
			}
			if len(batch) == 0 {
				break
			}
			counts = append(counts, len(batch))
			total += len(batch)
			remaining -= len(batch)
			if !more {
				break
			}
			last := batch[len(batch)-1].Key
			curBegin = append(append([]byte(nil), last...), 0)
			if iteration > 500 {
				t.Fatalf("mode=%v did not terminate within 500 fetches", mode)
			}
		}
		return total, counts
	}

	for _, c := range []struct {
		name  string
		cMode int
		goDe  gofdb.StreamingMode
		limit int
	}{
		{"medium/250", libfdbc.CStreamingModeMedium, gofdb.StreamingModeMedium, 250},
		{"medium/199", libfdbc.CStreamingModeMedium, gofdb.StreamingModeMedium, 199},
		{"small/25", libfdbc.CStreamingModeSmall, gofdb.StreamingModeSmall, 25},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cTotal, cCounts := cDivision(c.cMode, c.limit)
			gTotal, gCounts := goDivision(c.goDe, c.limit)

			t.Logf("MEASURED %-11s limit=%-4d libfdb_c %v (total %d) | pure-Go %v (total %d)",
				c.name, c.limit, cCounts, cTotal, gCounts, gTotal)

			// THE ROW BUDGET IS HONOURED EXACTLY. Neither client may return more than the
			// caller asked for, whatever the byte target does.
			if cTotal != c.limit {
				t.Errorf("%s: libfdb_c returned %d rows for a limit of %d", c.name, cTotal, c.limit)
			}
			if gTotal != c.limit {
				t.Errorf("%s: the pure-Go client returned %d rows for a limit of %d",
					c.name, gTotal, c.limit)
			}

			// THE DIVISIONS AGREE. This is the assertion the Go-side pin cannot make about
			// itself: it anchors the post-port bounded division to libfdb_c rather than to
			// whatever the Go client happens to do.
			if !equalInts(cCounts, gCounts) {
				t.Errorf("%s: libfdb_c divided %v but the pure-Go client divided %v — a bounded "+
					"read must batch identically in both, since the batch boundary is where the "+
					"per-batch read-conflict range is taken and where a continuation lands",
					c.name, cCounts, gCounts)
			}

			// THE FINAL BATCH IS CLAMPED BY THE LEFTOVER BUDGET, not rounded up to the byte
			// target and discarded. This is the invariant the pre-port Go test existed to
			// protect, restated in byte-driven terms: every batch but the last is the same
			// size (the byte target's own division) and the last is whatever remained.
			if len(gCounts) >= 2 {
				full := gCounts[0]
				last := gCounts[len(gCounts)-1]
				if last > full {
					t.Errorf("%s: final batch %d exceeds the steady-state batch %d (%v) — the "+
						"leftover row budget must CLAMP the final fetch, never widen it",
						c.name, last, full, gCounts)
				}
				sum := 0
				for _, b := range gCounts {
					sum += b
				}
				if sum != c.limit {
					t.Errorf("%s: batches %v sum to %d, want exactly the limit %d — an over-read "+
						"that is discarded takes a wider read-conflict range than the caller's "+
						"budget justifies", c.name, gCounts, sum, c.limit)
				}
			}

			if len(cCounts) < 2 {
				t.Errorf("%s: libfdb_c returned %d fetch(es) %v — this arm cannot measure a "+
					"DIVISION unless the read crosses at least one boundary", c.name, len(cCounts), cCounts)
			}
		})
	}
}
