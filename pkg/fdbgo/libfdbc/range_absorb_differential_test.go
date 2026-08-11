//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/libfdbc"
)

// TestLibFDBC_ExactModeAbsorbsByteCappedReplies establishes, by measurement, that ONE libfdb_c
// range call absorbs a byte-capped short reply and re-queries until its ROW budget is filled —
// and that which modes do so is decided by the per-mode byte target, not by the reply cap.
//
// This is the load-bearing measurement behind the exact-mode finding in
// fdb:TestRangeIterator_ExactModeIsStructurallySingleBatch. That test asserts the pure-Go
// client performs exactly one fetch for EXACT at any size. Whether that AGREES with libfdb_c or
// diverges from it is a completely different claim, and for a client where C++ is the spec it
// is the one that decides whether the Go behaviour is correct or a bug. It cannot be answered
// through Apple's binding, whose iterator loops internally and hides the per-call result.
//
// The mechanism, from the C source rather than inferred from these numbers:
//
//   - mode_bytes_array (bindings/c/fdb_c.cpp:1002) is
//     { BYTE_LIMIT_UNLIMITED, 256, 1000, 4096, 80000 }, indexed EXACT=0, SMALL=1, MEDIUM=2,
//     LARGE=3, SERIAL=4. EXACT's byte target is UNLIMITED; WANT_ALL maps to SERIAL (:1011).
//   - C++'s getRange returns only when `limits.isReached()` over GetRangeLimits{rows, bytes},
//     or when the server reports no more data (NativeAPI.actor.cpp:4761, :4814). A short reply
//     with rep.more=true does NOT end it — the loop re-queries, exactly as the pure-Go
//     client's rangeScanImpl does.
//   - CLIENT_KNOBS->REPLY_BYTE_LIMIT (fdbclient/ClientKnobs.cpp:66) is 80000. That is the
//     per-REPLY ceiling the storage server truncates at; it is not the per-mode target, and
//     conflating the two is what makes a mode's batching look like it has no byte dimension.
//
// So EXACT, alone among the modes, has no byte budget to reach and stops only on rows. The
// arms below drive one raw call each over ~97 KB of data — comfortably past the 80000-byte
// reply cap, so every arm's first reply IS truncated and the only question is what each mode
// does next.
func TestLibFDBC_ExactModeAbsorbsByteCappedReplies(t *testing.T) {
	t.Parallel()

	clusterFile := startCluster(t)

	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c database: %v", err)
	}
	defer cdb.Close()

	const (
		prefix   = "absorb_"
		n        = 100
		valueLen = 1000 // 100 KB of values against an 80000-byte reply cap
	)
	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatalf("create seed transaction: %v", err)
	}
	for i := 0; i < n; i++ {
		tr.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), bytes.Repeat([]byte{'v'}, valueLen))
	}
	if err := tr.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	tr.Close()

	begin, end := []byte(prefix), []byte(prefix+"~")

	for _, c := range []struct {
		name string
		mode int
		// limit is the row budget handed to the single call.
		limit int
		// wantRows is what ONE call must return. The non-EXACT arms are bounded by their byte
		// target, so they are expressed as a range rather than a literal: the exact row count
		// depends on per-row encoding overhead, which is not what this test pins.
		wantExact int // >0 means an exact count is required
		wantMax   int // else: 0 < rows <= wantMax
		why       string
	}{
		{
			name: "EXACT", mode: libfdbc.CStreamingModeExact, limit: n, wantExact: n,
			why: "mode_bytes_array[EXACT] is UNLIMITED, so nothing but the row budget can end " +
				"the loop; the truncated replies are absorbed and re-queried",
		},
		{
			name: "SERIAL", mode: libfdbc.CStreamingModeSerial, limit: 0, wantMax: n - 1,
			why: "SERIAL's byte target is 80000, so limits.isReached() ends the call short of " +
				"the full range",
		},
		{
			name: "WANT_ALL", mode: libfdbc.CStreamingModeWantAll, limit: 0, wantMax: n - 1,
			why: "WANT_ALL maps to SERIAL (fdb_c.cpp:1011), so it must behave identically to it",
		},
		{
			name: "SMALL", mode: libfdbc.CStreamingModeSmall, limit: 0, wantMax: 5,
			why: "SMALL's byte target is 256 — smaller than a single 1000-byte row, so the call " +
				"returns the minimum the server will yield",
		},
	} {
		c := c
		// NOT t.Parallel() on these subtests, deliberately: a parallel subtest runs after its
		// parent function returns, so the parent's `defer cdb.Close()` would fire first and
		// every arm would fail with database_closed(2015) before issuing a single read. The
		// parent carries t.Parallel(); the arms share its database handle and must not outlive
		// it.
		t.Run(c.name, func(t *testing.T) {
			ctr, err := cdb.CreateTransaction()
			if err != nil {
				t.Fatalf("create transaction: %v", err)
			}
			defer ctr.Close()

			// target_bytes 0 makes the C client apply its own per-mode value — the thing under
			// measurement. iteration 1 because none of these arms is ITERATOR mode.
			rows, more, err := libfdbc.CGetRangeBatch(ctr, begin, end,
				c.limit, 0 /* target_bytes */, c.mode, 1 /* iteration */, false, false)
			if err != nil {
				t.Fatalf("CGetRangeBatch(%s): %v", c.name, err)
			}
			t.Logf("MEASURED %-8s one call -> rows=%3d more=%v (data %d KB, reply cap 80000 B)",
				c.name, len(rows), more, n*valueLen/1024)

			if len(rows) == 0 {
				t.Fatalf("%s: one call returned ZERO rows — the arm is vacuous, and a mode that "+
					"cannot return a single row would break every caller", c.name)
			}
			if c.wantExact > 0 {
				if len(rows) != c.wantExact {
					t.Fatalf("%s: one call returned %d rows, want %d. %s. If this now returns "+
						"FEWER, libfdb_c has stopped absorbing byte-capped replies for this mode "+
						"and the pure-Go client's single-fetch EXACT (pinned by "+
						"fdb:TestRangeIterator_ExactModeIsStructurallySingleBatch) has become a "+
						"DIVERGENCE rather than a match",
						c.name, len(rows), c.wantExact, c.why)
				}
				return
			}
			if len(rows) > c.wantMax {
				t.Fatalf("%s: one call returned %d rows, want at most %d. %s. Returning the whole "+
					"range would mean this mode's byte target is no longer applied, which is the "+
					"same conflation of the reply CEILING with the per-mode TARGET that the "+
					"pure-Go client currently makes",
					c.name, len(rows), c.wantMax, c.why)
			}
		})
	}
}
