package simfdb_test

import (
	"errors"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

// codeOf extracts the FDB error code, or 0 when err is nil / not an fdb.Error.
func codeOf(err error) int {
	if err == nil {
		return 0
	}
	var fe fdb.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return -1
}

// TestCancelledTransactionReadEntryPoints pins that EVERY read entry point reports
// transaction_cancelled(1025) after Cancel(), not just Get/GetKey/Commit.
//
// The client reaches 1025 on the range and version paths through ensureReadVersion, whose first
// act is checkCancelled (client/transaction.go:662-665); the two metrics paths never fetch a read
// version, so libfdb_c gates them explicitly at op entry (client/metrics.go:29-38, :177-187). A
// sim that keeps answering reads on a cancelled handle lets a test "verify" post-cancel behaviour
// that no real client exhibits — and SimFDB exists to stand in for a real client.
func TestCancelledTransactionReadEntryPoints(t *testing.T) {
	t.Parallel()
	db := simfdb.New(nil)

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(fdb.Key("cx/a"), []byte("1"))
		tx.Set(fdb.Key("cx/b"), []byte("2"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	arms := []struct {
		name string
		read func(tx fdb.WritableTransaction) error
	}{
		// GetRange: the whole-slice consumer. The error must reach the caller through the
		// RangeResult, which is the only channel GetRange has.
		{"GetRange", func(tx fdb.WritableTransaction) error {
			_, err := tx.GetRange(fdb.KeyRange{Begin: fdb.Key("cx/"), End: fdb.Key("cx0")},
				fdb.RangeOptions{}).GetSliceWithError()
			return err
		}},
		// The iterator consumer of the same call: a cancelled scan must not stream rows either.
		{"GetRange/Iterator", func(tx fdb.WritableTransaction) error {
			it := tx.GetRange(fdb.KeyRange{Begin: fdb.Key("cx/"), End: fdb.Key("cx0")},
				fdb.RangeOptions{}).Iterator()
			advanced := it.Advance()
			kv, err := it.Get()
			if err != nil {
				return err
			}
			// No error AND no row would let the arm pass vacuously in both states, so a
			// healthy iterator has to prove it actually streamed the seeded row.
			if !advanced || string(kv.Key) != "cx/a" {
				return errors.New("iterator yielded neither a row nor an error")
			}
			return nil
		}},
		{"GetReadVersion", func(tx fdb.WritableTransaction) error {
			_, err := tx.GetReadVersion().Get()
			return err
		}},
		{"GetEstimatedRangeSizeBytes", func(tx fdb.WritableTransaction) error {
			_, err := tx.GetEstimatedRangeSizeBytes(
				fdb.KeyRange{Begin: fdb.Key("cx/"), End: fdb.Key("cx0")}).Get()
			return err
		}},
		{"GetRangeSplitPoints", func(tx fdb.WritableTransaction) error {
			_, err := tx.GetRangeSplitPoints(
				fdb.KeyRange{Begin: fdb.Key("cx/"), End: fdb.Key("cx0")}, 1000).Get()
			return err
		}},
	}

	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			tx, err := db.CreateWritableTransaction()
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			// Not cancelled yet: the arm must genuinely work, otherwise 1025 below could be
			// coming from anything at all.
			if err := arm.read(tx); err != nil {
				t.Fatalf("%s before Cancel: unexpected error %v", arm.name, err)
			}
			tx.Cancel()
			if got := codeOf(arm.read(tx)); got != 1025 {
				t.Fatalf("%s after Cancel: error code %d, want 1025 (transaction_cancelled)",
					arm.name, got)
			}
		})
	}
}

// TestCancelledTransactionMetricsReportInvertedRangeFirst pins the ORDER the two metrics entry
// points report their two errors in. libfdb_c constructs a KeyRangeRef from the C arguments
// before the op runs and that constructor throws inverted_range(2005), so an inverted range on a
// CANCELLED transaction is 2005 — not 1025 (client/metrics.go:29-38, :177-187). Gating on cancel
// first would invert the order the client reports, which is exactly the kind of divergence that
// makes a sim-certified error-handling path wrong against the real thing.
func TestCancelledTransactionMetricsReportInvertedRangeFirst(t *testing.T) {
	t.Parallel()
	db := simfdb.New(nil)
	tx, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tx.Cancel()
	inverted := fdb.KeyRange{Begin: fdb.Key("z"), End: fdb.Key("a")}

	// Errorf, not Fatalf: the two entry points gate independently, so a short-circuit here would
	// leave the split-points half unchecked whenever the metrics half regresses.
	if got := codeOf(errOf(tx.GetEstimatedRangeSizeBytes(inverted).Get())); got != 2005 {
		t.Errorf("GetEstimatedRangeSizeBytes(inverted, cancelled): code %d, want 2005", got)
	}
	_, err = tx.GetRangeSplitPoints(inverted, 1000).Get()
	if got := codeOf(err); got != 2005 {
		t.Fatalf("GetRangeSplitPoints(inverted, cancelled): code %d, want 2005", got)
	}
}

// errOf drops an int64 result and keeps the error, so a FutureInt64 can be fed to codeOf.
func errOf(_ int64, err error) error { return err }

// TestInvertedWriteConflictRangeRejected pins that an inverted EXPLICIT write conflict range is
// rejected with inverted_range(2005) rather than recorded.
//
// The harm is not that the range is meaningless — it is that rangesOverlap is the plain half-open
// predicate `a.begin < b.end && b.begin < a.end`, which an inverted [hi, lo) SATISFIES against any
// read range that straddles it. So an unvalidated inverted write conflict of ["n","c") matches a
// reader's ["a","z") and aborts it with not_committed(1020): the harness whose job is to certify
// conflict verdicts manufactures one out of a caller error. The second half of the test is the
// load-bearing half — it is what distinguishes "rejected" from "accepted and harmless".
func TestInvertedWriteConflictRangeRejected(t *testing.T) {
	t.Parallel()
	db := simfdb.New(nil)

	// 1. The call itself reports 2005 and records nothing.
	writer, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	if got := codeOf(writer.AddWriteConflictRange(
		fdb.KeyRange{Begin: fdb.Key("iw/n"), End: fdb.Key("iw/c")})); got != 2005 {
		t.Fatalf("AddWriteConflictRange(inverted): code %d, want 2005 (inverted_range)", got)
	}

	// 2. A reader whose read range STRADDLES the inverted bounds must still commit. If the
	//    inverted range had been recorded, rangesOverlap(["iw/a","iw/z"), ["iw/n","iw/c")) is
	//    true and the reader dies 1020.
	//
	//    The read conflict is taken EXPLICITLY and the read version is pinned before the writer
	//    commits. Both matter: a GetRange over an empty keyspace resolves to an empty extent and
	//    records no conflict at all, and a lazily-pinned reader pins after the writer and so is
	//    skipped by the SSI loop outright. Either one makes this half of the test pass with the
	//    inverted range fully recorded — i.e. testing nothing.
	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create reader: %v", err)
	}
	if _, err := reader.GetReadVersion().Get(); err != nil {
		t.Fatalf("reader GetReadVersion: %v", err)
	}
	if err := reader.AddReadConflictRange(
		fdb.KeyRange{Begin: fdb.Key("iw/a"), End: fdb.Key("iw/z")}); err != nil {
		t.Fatalf("reader AddReadConflictRange: %v", err)
	}
	writer.Set(fdb.Key("iw/zzz"), []byte("disjoint")) // outside the reader's range
	if err := writer.Commit().Get(); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	// The reader must WRITE something, outside its own read range. A transaction with no
	// mutations and no write conflict ranges is completed client-side and never reaches the
	// resolver at all (see the read-only fast path in commit), so a purely read-only "reader"
	// commits successfully no matter which conflict ranges anyone recorded.
	reader.Set(fdb.Key("iw/own"), []byte("x"))
	if err := reader.Commit().Get(); err != nil {
		t.Fatalf("reader commit: got %v (code %d), want success — the rejected inverted write "+
			"conflict range must not have been recorded", err, codeOf(err))
	}
}

// TestInvertedReadConflictRangeRejected is the read-side half of the same rule
// (client/transaction.go:3103-3106).
//
// The read side is NOT symmetric with the write side, and saying so is the point of this test.
// An inverted read range was already INERT before this change: addFilteredReadConflict opens
// with `if bytes.Compare(begin, end) >= 0 { return }` (ryw_conflict.go:222-224), so the range
// was silently swallowed rather than recorded. So the 2005 here is a REPORTING fix — the caller
// now learns its range was rejected instead of believing a conflict was registered — not a
// conflict-verdict fix like AddWriteConflictRange, whose addWriteConflict has no such guard.
//
// Only the first assertion below can detect this change. The commit assertion is the negative
// result: it holds because of the ryw_conflict.go guard, and TestInvertedReadRangeIsDroppedByFilter
// pins that guard directly so removing it cannot go unnoticed.
func TestInvertedReadConflictRangeRejected(t *testing.T) {
	t.Parallel()
	db := simfdb.New(nil)

	victim, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}
	// PIN THE READ VERSION FIRST. AddReadConflictRange does not fetch one, and the SSI check
	// skips every write whose commit version is <= the reader's read version — so a reader that
	// pins lazily at commit time pins AFTER the probe and cannot conflict with it no matter what
	// ranges it holds. Without this line the second half of the test passes with the fix fully
	// reverted, i.e. it would not be testing anything.
	if _, err := victim.GetReadVersion().Get(); err != nil {
		t.Fatalf("victim GetReadVersion: %v", err)
	}
	if got := codeOf(victim.AddReadConflictRange(
		fdb.KeyRange{Begin: fdb.Key("ir/n"), End: fdb.Key("ir/c")})); got != 2005 {
		t.Fatalf("AddReadConflictRange(inverted): code %d, want 2005 (inverted_range)", got)
	}

	// A concurrent write straddling the inverted bounds commits first.
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.AddWriteConflictRange(fdb.KeyRange{Begin: fdb.Key("ir/a"), End: fdb.Key("ir/z")})
		tx.Set(fdb.Key("ir/m"), []byte("probe"))
		return nil, nil
	}); err != nil {
		t.Fatalf("probe commit: %v", err)
	}

	victim.Set(fdb.Key("ir/own"), []byte("x"))
	if err := victim.Commit().Get(); err != nil {
		t.Fatalf("victim commit: got %v (code %d), want success — the rejected inverted read "+
			"conflict range must not have been recorded", err, codeOf(err))
	}
}

// TestOversizedConflictRangeEndpointClamped pins the endpoint clamp
// (client/transaction.go:3167-3175, :3115-3126): an endpoint longer than getMaxClearKeySize is
// TRUNCATED to maxSize+1 bytes rather than shipped whole, and a range the clamp collapses to
// empty is DROPPED without recording a conflict.
//
// The clamp is observable through the collapse: two keys that differ only PAST the limit are
// identical after truncation, so the range becomes empty and takes no conflict at all. Without
// the clamp the full range is recorded and a straddling reader is aborted.
func TestOversizedConflictRangeEndpointClamped(t *testing.T) {
	t.Parallel()
	db := simfdb.New(nil)

	// Both endpoints share a 10_100-byte prefix (> keySizeLimit+tenantPrefixSize == 10_008) and
	// differ only at byte 10_050, well past the clamp point. After truncation to maxSize+1 bytes
	// they are equal, so the range is empty and must be dropped.
	const over = 10_100
	begin := make([]byte, over)
	end := make([]byte, over)
	for i := range begin {
		begin[i] = 'k'
		end[i] = 'k'
	}
	end[10_050] = 'z' // the ONLY difference, past the clamp

	// The READER's conflict range is short — well under the limit — so it is recorded verbatim
	// and is not itself subject to the clamp. It brackets the oversized endpoints: every key
	// beginning with 'k' lies in ["k", "l"). Taking it explicitly rather than through GetRange
	// keeps the probe on the clamp instead of on the range-read conflict-extent machinery.
	reader, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create reader: %v", err)
	}
	// Pin the read version BEFORE the writer commits — see the note in
	// TestInvertedReadConflictRangeRejected. A lazily-pinned reader cannot conflict with a write
	// that precedes its own read version, which makes the probe pass with or without the clamp.
	if _, err := reader.GetReadVersion().Get(); err != nil {
		t.Fatalf("reader GetReadVersion: %v", err)
	}
	if err := reader.AddReadConflictRange(
		fdb.KeyRange{Begin: fdb.Key("k"), End: fdb.Key("l")}); err != nil {
		t.Fatalf("reader AddReadConflictRange: %v", err)
	}

	writer, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	if err := writer.AddWriteConflictRange(
		fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}); err != nil {
		t.Fatalf("AddWriteConflictRange(oversized): %v", err)
	}
	// A mutation outside the reader's range, so the commit is a real one (a transaction with no
	// mutations AND no write conflict ranges takes the client-side read-only fast path and would
	// never reach the resolver at all — which would make this probe pass vacuously once the
	// clamp drops the range).
	writer.Set(fdb.Key("zzz/disjoint"), []byte("x"))
	if err := writer.Commit().Get(); err != nil {
		t.Fatalf("writer commit: %v", err)
	}

	reader.Set(fdb.Key("reader/own"), []byte("y"))
	if err := reader.Commit().Get(); err != nil {
		t.Fatalf("reader commit: got %v (code %d), want success — the oversized write conflict "+
			"range collapses to empty under the endpoint clamp and must record nothing. "+
			"Unclamped, [k*10100, k*10050+z*) overlaps the reader's [k, l) and aborts it 1020",
			err, codeOf(err))
	}
}
