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
//  2. CONVERGENCE. The two clients must divide the range IDENTICALLY, per mode, and both must
//     match the literal division recorded per arm below.
//
// Assertion 2 used to pin the opposite — a measured DIVERGENCE, stated as currently-true,
// failing when it went away — because the pure-Go client had no byte dimension in this decision
// and pinned LimitBytes to 80000 on every request regardless of mode. That is history now: the
// client derives target_bytes per mode from the same table C does (fdb.ModeTargetBytes, from
// fdb_c.cpp:1002/1006), puts it on the request where the storage server truncates against it,
// and ends the call at that reply via the soft byte limit. The old expectation is not kept
// anywhere as a fallback — leaving it would be an unwatched revival of exactly what the port
// removed.
//
// The LITERAL divisions are asserted alongside the agreement on purpose. Both sides now derive
// from one table, so a bare equality check has two arms sharing a derivation and would hold
// vacuously against anything that moved both at once — a wrong table, or the Go->C enum offset
// being applied in the wrong direction.
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

	// goDivision drives the REAL pure-Go client one fetch at a time, mirroring cDivision
	// exactly: the mode's own per-fetch byte target (fdb.ModeTargetBytes, the single definition
	// of the rule), an unlimited row budget, and a begin key advanced past the last row
	// returned. It deliberately does NOT model the division — an earlier version of this test
	// predicted it from fdb.BatchSize, which is a ROW rule and therefore could not observe the
	// byte dimension at all, so it reported the pre-port division no matter what the client did.
	goDivision := func(mode gofdb.StreamingMode) (rows []fdbclient.KeyValue, counts []int) {
		gtx := goDB.CreateTransaction()
		defer gtx.Cancel()
		curBegin := append([]byte(nil), begin...)
		for iteration := 1; ; iteration++ {
			target, err := gofdb.ModeTargetBytes(mode, iteration)
			if err != nil {
				t.Fatalf("ModeTargetBytes(mode=%v, iteration=%d): %v", mode, iteration, err)
			}
			batch, more, err := gtx.GetRangeWithByteTarget(ctx, curBegin, end,
				0 /* limit: unlimited */, target, false /* reverse */)
			if err != nil {
				t.Fatalf("pure-Go GetRangeWithByteTarget(mode=%v, iteration=%d): %v", mode, iteration, err)
			}
			counts = append(counts, len(batch))
			rows = append(rows, batch...)
			if !more || len(batch) == 0 {
				return rows, counts
			}
			last := batch[len(batch)-1].Key
			curBegin = append(append([]byte(nil), last...), 0) // keyAfter
			if iteration > 500 {
				t.Fatalf("mode=%v did not terminate within 500 fetches", mode)
			}
		}
	}

	for _, c := range []struct {
		name  string
		cMode int
		goDe  gofdb.StreamingMode
		// want is the LITERAL division both clients must produce for 60 rows of 200 bytes.
		// Asserted as well as the two clients agreeing with each other, because since the
		// GetRangeLimits port both sides derive from the SAME table (fdb_c.cpp:1002 on one
		// side, fdb.ModeTargetBytes on the other) — so an equality check alone would hold
		// vacuously under any change that moved both, which is exactly what a wrong table or
		// a wrong enum offset would do.
		want []int
		// byteBound is true when the per-mode target_bytes is small enough that it, rather
		// than any row count, decides the split for 200-byte rows.
		byteBound bool
	}{
		{"small", libfdbc.CStreamingModeSmall, gofdb.StreamingModeSmall, repeatInts(2, 30), true},
		{"medium", libfdbc.CStreamingModeMedium, gofdb.StreamingModeMedium, repeatInts(5, 12), true},
		{"large", libfdbc.CStreamingModeLarge, gofdb.StreamingModeLarge, []int{18, 18, 18, 6}, true},
		{"serial", libfdbc.CStreamingModeSerial, gofdb.StreamingModeSerial, []int{60}, false},
		// ITERATOR is the arm whose target depends on the ITERATION NUMBER
		// (iteration_progression, fdb_c.cpp:1006: 4096, 6144, 9216, ...), so it is the one
		// that catches a client which derived a byte target but never threaded the iteration
		// count — such a client would divide every fetch at 4096 and still pass every other
		// arm here. 18 rows at the first target, then the range finishes inside the second.
		{"iterator", libfdbc.CStreamingModeIterator, gofdb.StreamingModeIterator, []int{18, 26, 16}, true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cRows, cCounts := cDivision(c.cMode)
			goDivRows, gCounts := goDivision(c.goDe)
			_ = goDivRows

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
			// it is a property of this loop, not of the division, so drop it on BOTH sides
			// before comparing — the two clients need not agree on whether they spent that
			// extra probe, only on where they cut the data.
			cCounts = trimTrailingZero(cCounts)
			gCounts = trimTrailingZero(gCounts)

			t.Logf("MEASURED %-7s libfdb_c division %v (%d fetches) | pure-Go division %v (%d)",
				c.name, cCounts, len(cCounts), gCounts, len(gCounts))

			// (2) CONVERGENCE. Both clients must divide identically, AND both must match the
			// literal expected division. The literal is what keeps this from being a pair whose
			// two sides move together: since the port, both derive from the same byte table, so
			// "they agree" alone would survive a wrong table, a wrong enum offset, or the byte
			// target failing to reach the request on both paths at once.
			if !equalInts(cCounts, c.want) {
				t.Errorf("%s: libfdb_c divided %v, want %v — this is C's own measured division "+
					"and the reference the port targets; a change here means the C client or the "+
					"storage server's reply-size accounting moved, not that Go regressed",
					c.name, cCounts, c.want)
			}
			if !equalInts(gCounts, c.want) {
				t.Errorf("%s: the pure-Go client divided %v, want %v. The per-mode byte target "+
					"must reach the REQUEST (so the storage server truncates there) and the soft "+
					"byte limit must END the call at that reply. Either half missing changes this: "+
					"without the request half the loop absorbs and re-queries to one big batch; "+
					"without the loop half it cuts a row late (19 where C cuts 18 at 4096)",
					c.name, gCounts, c.want)
			}
			if !equalInts(cCounts, gCounts) {
				t.Errorf("%s: libfdb_c divided %v but the pure-Go client divided %v — the two "+
					"clients must batch identically, since a batch boundary is where a per-batch "+
					"read-conflict range is taken (RFC-121) and where a cursor continuation lands",
					c.name, cCounts, gCounts)
			}

			if c.byteBound && len(cCounts) < 2 {
				t.Errorf("libfdb_c split %d rows into %d fetch(es) %v — this arm is vacuous as a "+
					"DIVISION measurement unless C crosses at least one boundary", len(cRows),
					len(cCounts), cCounts)
			}
			if !c.byteBound && len(cCounts) != 1 {
				// POSITIVE CONTROL, inverted since the port. SERIAL's target (80000) exceeds
				// this whole 12 KB range, so it must stay a SINGLE fetch on both sides. If
				// SERIAL starts splitting, a byte target is being applied where none should
				// bite, and the byte-bound arms above cannot be trusted either.
				t.Errorf("%s: expected a single undivided fetch, got %v. SERIAL's 80000-byte "+
					"target is larger than this entire range, so nothing should cut it",
					c.name, cCounts)
			}
		})
	}
}

func repeatInts(v, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
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
