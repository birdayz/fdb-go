//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	fdbclient "fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
)

// TestLibFDBC_RangeBatchDivision measures where libfdb_c SPLITS a range read, which nothing in
// this repo could observe before CGetRangeBatch existed.
//
// Row-set differentials between the two clients are cheap and already exist
// (bench:TestDifferential_LimitedIteratorMultiBatchRowSets). They compare the ANSWER. This
// compares the DIVISION, and the division is where a read-conflict range is taken and where a
// cursor continuation lands. cgofdb cannot answer it — iteration/more/the advancing begin-key
// are private on an unexported iterator — so every previous statement about C's batching here
// was read off source rather than measured, and this file's whole reason to exist is that the
// last such inference turned out to be wrong.
//
// It asserts two things of very different character, deliberately kept apart:
//
//  1. AN INVARIANT. Concatenating C's batches must reproduce exactly the rows the pure-Go
//     client returns for the same range. That must hold forever; a failure is a real bug.
//  2. A MEASURED DIVERGENCE, pinned as currently-true. libfdb_c derives target_bytes PER MODE
//     from mode_bytes_array (SMALL 256, MEDIUM 1000, LARGE 4096, SERIAL 80000 — recorded at
//     fdb/range_result.go:215-217 from fdb_c.cpp:1002), so a C SMALL fetch stops at ~256 bytes.
//     The pure-Go client pins LimitBytes to 80000 on EVERY request (client/readpath.go:1102,
//     constant at :27) regardless of mode, because StreamingMode never reaches pkg/fdbgo/client
//     at all — its batching budget is rows-only (fdb.BatchSize). So the two divide the same
//     range differently, and a Go SMALL fetch asks the storage server for 80000 bytes where C
//     asks for 256.
//
// Assertion 2 pins a DIVERGENCE, not desired behaviour, and its direction is the point: it
// fails when the divergence GOES AWAY. That is the signal that the byte-dimension port booked
// in TODO.md Phase 12 has landed, at which point this test must be rewritten to assert the two
// divisions AGREE. Leaving it asserting the old expectation after that would be an unwatched
// revival of the very thing the port removed.
func TestLibFDBC_RangeBatchDivision(t *testing.T) {
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

	// Values are deliberately fat. With 200-byte rows a 256-byte SMALL target admits roughly
	// one row per fetch in C while the pure-Go client's SMALL batch is a flat 10 rows — the
	// byte dimension is invisible on tiny rows, which is why every earlier probe missed it.
	const (
		prefix   = "rangediv_"
		n        = 60
		valueLen = 200
	)
	begin := []byte(prefix)
	end := []byte(prefix + "~")

	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatalf("create seed transaction: %v", err)
	}
	for i := 0; i < n; i++ {
		v := bytes.Repeat([]byte{byte('a' + i%26)}, valueLen)
		tr.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), v)
	}
	if err := tr.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	tr.Close()

	// The pure-Go client's answer for the whole range: the row-set oracle for assertion 1.
	goTx := goDB.CreateTransaction()
	defer goTx.Cancel()
	goRows, _, err := goTx.GetRange(ctx, begin, end, 0)
	if err != nil {
		t.Fatalf("pure-Go GetRange: %v", err)
	}
	if len(goRows) != n {
		t.Fatalf("pure-Go returned %d rows, want %d", len(goRows), n)
	}

	// cDivision drives libfdb_c's own loop one fetch at a time, exactly as a binding would:
	// iteration climbs from 1, the begin key advances past the last row returned, and
	// target_bytes is left 0 so the C client applies its per-mode default — the value under
	// measurement.
	cDivision := func(mode int) (rows []libfdbc.CKeyValue, counts []int) {
		ctr, err := cdb.CreateTransaction()
		if err != nil {
			t.Fatalf("create transaction: %v", err)
		}
		defer ctr.Close()
		curBegin := append([]byte(nil), begin...)
		for iteration := 1; ; iteration++ {
			batch, more, err := libfdbc.CGetRangeBatch(ctr, curBegin, end,
				0 /* limit: unlimited */, 0 /* target_bytes: C's per-mode default */, mode, iteration,
				false /* reverse */, false /* snapshot */)
			if err != nil {
				t.Fatalf("CGetRangeBatch(mode=%d, iteration=%d): %v", mode, iteration, err)
			}
			counts = append(counts, len(batch))
			rows = append(rows, batch...)
			if !more || len(batch) == 0 {
				return rows, counts
			}
			last := batch[len(batch)-1].Key
			curBegin = append(append([]byte(nil), last...), 0) // keyAfter
			if iteration > 500 {
				t.Fatalf("mode=%d did not terminate within 500 fetches", mode)
			}
		}
	}

	// goDivision is the division the pure-Go client's iterator WOULD take for the same mode:
	// fdb.BatchSize is exported precisely so the batching rule has one definition.
	goDivision := func(mode gofdb.StreamingMode) []int {
		var counts []int
		remaining := n
		for iteration := 1; remaining > 0; iteration++ {
			b := gofdb.BatchSize(mode, iteration, remaining)
			if b > remaining {
				b = remaining
			}
			counts = append(counts, b)
			remaining -= b
		}
		return counts
	}

	for _, c := range []struct {
		name  string
		cMode int
		goDe  gofdb.StreamingMode
		// byteBound is true when C's per-mode target_bytes is small enough that it, rather
		// than any row count, decides the split for 200-byte rows.
		byteBound bool
	}{
		{"small", libfdbc.CStreamingModeSmall, gofdb.StreamingModeSmall, true},
		{"medium", libfdbc.CStreamingModeMedium, gofdb.StreamingModeMedium, true},
		{"large", libfdbc.CStreamingModeLarge, gofdb.StreamingModeLarge, true},
		{"serial", libfdbc.CStreamingModeSerial, gofdb.StreamingModeSerial, false},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cRows, cCounts := cDivision(c.cMode)
			gCounts := goDivision(c.goDe)

			// (1) THE INVARIANT — the two clients must agree on the answer.
			if len(cRows) != len(goRows) {
				t.Fatalf("libfdb_c returned %d rows across %d fetches %v, pure-Go returned %d — "+
					"the clients disagree on the ANSWER, which is a bug regardless of how either "+
					"one divided it", len(cRows), len(cCounts), cCounts, len(goRows))
			}
			for i := range cRows {
				if !bytes.Equal(cRows[i].Key, []byte(goRows[i].Key)) ||
					!bytes.Equal(cRows[i].Value, goRows[i].Value) {
					t.Fatalf("row %d differs: libfdb_c key=%q, pure-Go key=%q",
						i, cRows[i].Key, goRows[i].Key)
				}
			}
			// A drain whose last fetch reported more=true ends with an empty confirming fetch;
			// it is a property of this loop, not of the division, so drop it before comparing.
			cCounts = trimTrailingZero(cCounts)

			t.Logf("MEASURED %-7s libfdb_c division %v (%d fetches) | pure-Go modelled division %v (%d)",
				c.name, cCounts, len(cCounts), gCounts, len(gCounts))

			if !c.byteBound {
				// POSITIVE CONTROL. SERIAL is the one mode whose C target_bytes (80000) equals
				// the value the pure-Go client pins for EVERY mode, so it is the arm where the
				// two must divide identically. It is what proves the divergences below are
				// specific to the per-mode byte table and not an artifact of driving C's loop
				// by hand — without it, "the divisions differ" is equally consistent with this
				// harness simply not reproducing C's loop correctly.
				if !equalInts(cCounts, gCounts) {
					t.Fatalf("%s: libfdb_c divided %v but the pure-Go model says %v. These must "+
						"agree: SERIAL's C byte target (80000) is exactly what the pure-Go client "+
						"pins for every mode, so a difference here means this harness is not "+
						"reproducing C's loop and the divergence arms above cannot be trusted",
						c.name, cCounts, gCounts)
				}
				return
			}

			if len(cCounts) < 2 {
				t.Fatalf("libfdb_c split %d rows into %d fetch(es) %v — this arm is vacuous as a "+
					"DIVISION measurement unless C crosses at least one boundary", len(cRows),
					len(cCounts), cCounts)
			}

			// (2) THE MEASURED DIVERGENCE — pinned as currently true, failing when it is FIXED.
			if equalInts(cCounts, gCounts) {
				t.Fatalf("libfdb_c and the pure-Go client now divide %s identically (%v). That is "+
					"the OUTCOME WE WANT, not a passing state for this assertion: it means the "+
					"byte-dimension budget booked in TODO.md Phase 12 has landed and the pure-Go "+
					"client no longer pins LimitBytes to 80000 for every mode. Rewrite this arm to "+
					"assert the divisions AGREE, and drop the divergence note from the TODO item.",
					c.name, cCounts)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// trimTrailingZero drops a final zero-row fetch. The loop that drives C stops on more=false, so
// a drain whose last data-bearing fetch still reported more=true takes one extra empty fetch to
// discover the end. That is an artifact of driving the loop, not part of how C divided the rows.
func trimTrailingZero(counts []int) []int {
	if len(counts) > 0 && counts[len(counts)-1] == 0 {
		return counts[:len(counts)-1]
	}
	return counts
}
