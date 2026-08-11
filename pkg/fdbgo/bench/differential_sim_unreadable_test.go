package bench

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"

	"fdb.dev/pkg/dst"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// accessed_unreadable (1036) differential SIM vs libfdb_c.
//
// TestDifferential_Unreadable already pins the pure-Go client against libfdb_c. That is not
// enough for SimFDB: the sim was argued from the pure-Go client's SOURCE, and the client is
// itself a port. C++ is the spec, so the sim's fidelity has to be measured against libfdb_c
// directly — otherwise a shared misreading of C++ would be invisible on both sides.
//
// The scenarios below run the SAME sequence against a fresh SimDB and against real FDB through
// libfdb_c and compare the resulting FDB error codes. The arms that carry the most weight:
//
//   - getrange_limited_stops_before: proves the throw is CONDITIONAL on the scan reaching the
//     pending key (C++ breaks on the row limit at ReadYourWrites.actor.cpp:685 before the throw
//     at :692). A sim that threw unconditionally would pass every other arm.
//   - getrange_iterator_batch_boundary: drives the scan through an Iterator so it crosses at
//     least one batch boundary before reaching the pending key. If the per-batch unreadable cap
//     were persisted into the scan bounds, the following batch would find no cap, return no rows
//     and report CLEAN EXHAUSTION — the 1036 silently swallowed at the boundary.
//   - svk_other_key_in_range: SetVersionstampedKey makes the entire CANDIDATE STAMP RANGE
//     unreadable (C++ writes.addUnmodifiedAndUnreadableRange, ReadYourWrites.actor.cpp:2271,
//     over getVersionstampKeyRange, Atomic.h:268-287), not just the template key. A read of a
//     DIFFERENT key inside that range throws too.
//
// Helpers (unreadableSVVOperand, unreadableSVKKey, fdbErrorCode, errAccessedUnreadable) are
// shared with differential_unreadable_test.go on purpose: both files must describe the same
// operand and key shapes or the two differentials would be probing different things.

// errIterationMeasured aborts a probe transaction once the measurement is taken. It is not an
// fdb.Error, so neither Transact retries on it.
var errIterationMeasured = errors.New("iteration measured; abort without committing")

// simErrCode runs fn in a fresh SimDB transaction and returns the FDB error code it produced —
// the sim analog of goErrCode / cgoErrCode. Each call gets its own database: a SimDB is
// in-memory and per-test, so there is no shared-prefix hygiene to observe.
func simErrCode(fn func(tx gofdb.WritableTransaction) error) int {
	env := &dst.Env{Clock: dst.NewSimClock(dst.Epoch), Random: dst.NewSeededRandomness(1036)}
	db := simfdb.New(env)
	_, err := db.Transact(func(tx gofdb.WritableTransaction) (any, error) {
		return nil, fn(tx)
	})
	return fdbErrorCode(err)
}

// newSimDB returns a fresh in-memory SimDB for one subtest.
func newSimDB(seed uint64) *simfdb.SimDB {
	env := &dst.Env{Clock: dst.NewSimClock(dst.Epoch), Random: dst.NewSeededRandomness(seed)}
	return simfdb.New(env)
}

func TestDifferential_SimUnreadable(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("differ_simunread_%d_", os.Getpid())
	clearPrefix(t, pfx)

	t.Run("svv_get", func(t *testing.T) {
		t.Parallel()
		k := pfx + "svv_get"
		simCode := simErrCode(func(tx gofdb.WritableTransaction) error {
			tx.SetVersionstampedValue(gofdb.Key(k), unreadableSVVOperand())
			_, err := tx.Get(gofdb.Key(k)).Get()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedValue(cgofdb.Key(k), unreadableSVVOperand())
			_, err := tx.Get(cgofdb.Key(k)).Get()
			return err
		})
		if simCode != cCode || simCode != errAccessedUnreadable {
			t.Fatalf("same-txn Get of pending SVV: sim=%d cgo=%d, want both %d", simCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("getrange_unlimited_reaches", func(t *testing.T) {
		t.Parallel()
		rPfx := pfx + "unlim/"
		simR, cR := mustPrefixRanges(t, rPfx)
		z := rPfx + "z"
		simDB := newSimDB(1)
		_, simErr := simDB.Transact(func(tx gofdb.WritableTransaction) (any, error) {
			tx.Set(gofdb.Key(rPfx+"a"), []byte("va"))
			tx.SetVersionstampedValue(gofdb.Key(z), unreadableSVVOperand())
			_, err := tx.GetRange(simR, gofdb.RangeOptions{}).GetSliceWithError()
			return nil, err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.Set(cgofdb.Key(rPfx+"a"), []byte("va"))
			tx.SetVersionstampedValue(cgofdb.Key(z), unreadableSVVOperand())
			_, err := tx.GetRange(cR, cgofdb.RangeOptions{Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			return err
		})
		if simCode := fdbErrorCode(simErr); simCode != cCode || simCode != errAccessedUnreadable {
			t.Fatalf("unlimited scan reaching the stamp: sim=%d cgo=%d, want both %d", simCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("getrange_limited_stops_before", func(t *testing.T) {
		// The arm that proves the throw is not unconditional: a limit filled strictly BEFORE
		// the pending key returns its rows with no error on both engines.
		t.Parallel()
		rPfx := pfx + "lim/"
		simR, cR := mustPrefixRanges(t, rPfx)
		a, b, z := rPfx+"a", rPfx+"b", rPfx+"z"

		var simKeys, cKeys [][]byte
		simDB := newSimDB(2)
		_, simErr := simDB.Transact(func(tx gofdb.WritableTransaction) (any, error) {
			tx.Set(gofdb.Key(a), []byte("va"))
			tx.Set(gofdb.Key(b), []byte("vb"))
			tx.SetVersionstampedValue(gofdb.Key(z), unreadableSVVOperand())
			kvs, err := tx.GetRange(simR, gofdb.RangeOptions{Limit: 2}).GetSliceWithError()
			for _, kv := range kvs {
				simKeys = append(simKeys, kv.Key)
			}
			return nil, err
		})
		_, cErr := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.Set(cgofdb.Key(a), []byte("va"))
			tx.Set(cgofdb.Key(b), []byte("vb"))
			tx.SetVersionstampedValue(cgofdb.Key(z), unreadableSVVOperand())
			kvs, err := tx.GetRange(cR, cgofdb.RangeOptions{Limit: 2, Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			for _, kv := range kvs {
				cKeys = append(cKeys, kv.Key)
			}
			return nil, err
		})
		if simErr != nil || cErr != nil {
			t.Fatalf("limited scan stopping before the stamp: simErr=%v cErr=%v", simErr, cErr)
		}
		if len(simKeys) != 2 || len(cKeys) != 2 ||
			!bytes.Equal(simKeys[0], cKeys[0]) || !bytes.Equal(simKeys[1], cKeys[1]) ||
			string(simKeys[0]) != a || string(simKeys[1]) != b {
			t.Fatalf("limited scan rows: sim=%q cgo=%q, want both [%q %q]", simKeys, cKeys, a, b)
		}
	})

	t.Run("getrange_reverse_limit1_hits_stamp", func(t *testing.T) {
		t.Parallel()
		rPfx := pfx + "rev/"
		simR, cR := mustPrefixRanges(t, rPfx)
		a, z := rPfx+"a", rPfx+"z"
		simDB := newSimDB(3)
		_, simErr := simDB.Transact(func(tx gofdb.WritableTransaction) (any, error) {
			tx.Set(gofdb.Key(a), []byte("va"))
			tx.SetVersionstampedValue(gofdb.Key(z), unreadableSVVOperand())
			_, err := tx.GetRange(simR, gofdb.RangeOptions{Limit: 1, Reverse: true}).GetSliceWithError()
			return nil, err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.Set(cgofdb.Key(a), []byte("va"))
			tx.SetVersionstampedValue(cgofdb.Key(z), unreadableSVVOperand())
			_, err := tx.GetRange(cR, cgofdb.RangeOptions{Limit: 1, Reverse: true, Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			return err
		})
		if simCode := fdbErrorCode(simErr); simCode != cCode || simCode != errAccessedUnreadable {
			t.Fatalf("reverse limit-1 scan (stamp first): sim=%d cgo=%d, want both %d", simCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("getrange_iterator_batch_boundary", func(t *testing.T) {
		// The swallow-prone arm. Enough rows precede the pending key that a SMALL streaming
		// batch cannot cover them in one fetch, so the scan MUST cross a batch boundary before
		// it reaches the stamp. Both engines have to raise 1036 rather than report clean
		// exhaustion after the rows they did return.
		t.Parallel()
		rPfx := pfx + "iter/"
		simR, cR := mustPrefixRanges(t, rPfx)
		const nSeed = 25 // > the sim/Go SMALL batch of 10 rows and > libfdb_c's 256-byte target
		z := rPfx + "z"

		// drain reports what the iteration ended with: how many rows it yielded, the error code
		// it raised (0 = none), and whether it reported clean exhaustion instead of an error.
		type drained struct {
			rows int
			code int
		}
		var simD, cD drained

		simDB := newSimDB(4)
		if _, err := simDB.Transact(func(tx gofdb.WritableTransaction) (any, error) {
			for i := 0; i < nSeed; i++ {
				tx.Set(gofdb.Key(fmt.Sprintf("%sk%03d", rPfx, i)), []byte("payload-payload-payload"))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("sim seed: %v", err)
		}
		if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			for i := 0; i < nSeed; i++ {
				tx.Set(cgofdb.Key(fmt.Sprintf("%sk%03d", rPfx, i)), []byte("payload-payload-payload"))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("cgo seed: %v", err)
		}

		// Both closures end by returning errIterationMeasured so the transaction is ABORTED
		// rather than committed. A swallowed 1036 read poisons the read set (its errored future
		// stays in ryw->reading, which commit() waits on), so a committing closure would report
		// 1036 from the COMMIT whatever the iteration did — masking exactly the swallow this arm
		// exists to detect.
		if _, simErr := simDB.Transact(func(tx gofdb.WritableTransaction) (any, error) {
			tx.SetVersionstampedValue(gofdb.Key(z), unreadableSVVOperand())
			it := tx.GetRange(simR, gofdb.RangeOptions{Mode: gofdb.StreamingModeSmall}).Iterator()
			for it.Advance() {
				if _, err := it.Get(); err != nil {
					simD.code = fdbErrorCode(err)
					return nil, errIterationMeasured
				}
				simD.rows++
			}
			// Advance() returned false. That is EITHER the error surfacing through the
			// iterator's error channel or genuine exhaustion — Get() separates the two, and
			// conflating them is exactly the swallow this arm hunts.
			if _, err := it.Get(); err != nil {
				simD.code = fdbErrorCode(err)
			}
			return nil, errIterationMeasured
		}); !errors.Is(simErr, errIterationMeasured) {
			t.Fatalf("sim iterator txn: %v", simErr)
		}
		if _, cErr := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedValue(cgofdb.Key(z), unreadableSVVOperand())
			it := tx.GetRange(cR, cgofdb.RangeOptions{Mode: cgofdb.StreamingModeSmall}).Iterator()
			for it.Advance() {
				if _, err := it.Get(); err != nil {
					cD.code = fdbErrorCode(err)
					return nil, errIterationMeasured
				}
				cD.rows++
			}
			if _, err := it.Get(); err != nil {
				cD.code = fdbErrorCode(err)
			}
			return nil, errIterationMeasured
		}); !errors.Is(cErr, errIterationMeasured) {
			t.Fatalf("cgo iterator txn: %v", cErr)
		}

		if simD.code != cD.code || simD.code != errAccessedUnreadable {
			t.Fatalf("iterator crossing a batch boundary into the stamp: sim=%d cgo=%d, want both %d "+
				"(a 0 here is the 1036 swallowed at the boundary — the iteration reported clean exhaustion)",
				simD.code, cD.code, errAccessedUnreadable)
		}
		// The code check alone does not prove a boundary was crossed: a scan that threw on its
		// FIRST batch would satisfy it while never reaching the case this arm exists for. The
		// two engines are NOT asked to yield the same count — SimFDB and the pure-Go client
		// budget a batch in ROWS where libfdb_c budgets it in BYTES (fdb/range_result.go's
		// batchSize documents that divergence), so the exact stop position differs by
		// construction. What both must show is a partial drain: some rows yielded (not thrown
		// on batch one) and fewer than all of them (the erroring fetch's rows are discarded, as
		// C++ discards the prefix it read alongside the throw).
		if simD.rows == 0 || simD.rows >= nSeed {
			t.Fatalf("sim rows before the stamp = %d, want 0 < rows < %d — 0 means the throw came "+
				"before any boundary was crossed", simD.rows, nSeed)
		}
		if cD.rows == 0 || cD.rows >= nSeed {
			t.Fatalf("cgo rows before the stamp = %d, want 0 < rows < %d", cD.rows, nSeed)
		}
		// Sharper on the sim side: more rows than a single SMALL fetch can hold means at least
		// one boundary was genuinely crossed before the 1036 was raised.
		//
		// The bound is derived from SMALL's own BYTE target rather than from a row page. SMALL
		// is 256 bytes (fdb_c.cpp:1002) and a row costs key+value+24 against it, so with these
		// seeded rows — a ~30-byte key and a 23-byte value — one fetch holds at most
		// 256/(30+23+24) ≈ 4 rows. Using the seeded value length keeps this honest if the
		// fixture changes; the previous form called fdb.BatchSize, which returned SMALL's
		// old per-mode ROW page and no longer describes how anything batches.
		const smallTargetBytes = 256
		minRowCost := len(rPfx) + len("k000") + len("payload-payload-payload") + 24
		smallBatch := smallTargetBytes/minRowCost + 1
		if simD.rows <= smallBatch {
			t.Fatalf("sim rows before the stamp = %d, want > one SMALL fetch (%d rows at this row "+
				"size) — the scan never crossed a batch boundary, so this arm proved nothing "+
				"about the boundary", simD.rows, smallBatch)
		}
	})

	t.Run("svk_other_key_in_range", func(t *testing.T) {
		// SetVersionstampedKey marks the whole CANDIDATE STAMP RANGE unreadable, not just the
		// template key: a Get of a DIFFERENT key inside it throws.
		t.Parallel()
		svkPfx := []byte(pfx + "svk/")
		other := append(append([]byte(nil), svkPfx...), bytes.Repeat([]byte{0x7f}, 10)...)
		simCode := simErrCode(func(tx gofdb.WritableTransaction) error {
			tx.SetVersionstampedKey(gofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.Get(gofdb.Key(other)).Get()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedKey(cgofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.Get(cgofdb.Key(other)).Get()
			return err
		})
		if simCode != cCode || simCode != errAccessedUnreadable {
			t.Fatalf("Get of another key inside the pending SVK candidate range: sim=%d cgo=%d, "+
				"want both %d (sim=0 means it marks only the exact template key and UNDER-THROWS)",
				simCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("svk_range_getrange_reaches", func(t *testing.T) {
		// The range-read face of the same rule: a scan that reaches into the candidate range
		// throws even though no write-map key sits at the position it stopped on.
		t.Parallel()
		rPfx := pfx + "svkrange/"
		simR, cR := mustPrefixRanges(t, rPfx)
		svkPfx := []byte(rPfx + "b")
		simCode := simErrCode(func(tx gofdb.WritableTransaction) error {
			tx.Set(gofdb.Key(rPfx+"a"), []byte("va"))
			tx.SetVersionstampedKey(gofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.GetRange(simR, gofdb.RangeOptions{}).GetSliceWithError()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.Set(cgofdb.Key(rPfx+"a"), []byte("va"))
			tx.SetVersionstampedKey(cgofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.GetRange(cR, cgofdb.RangeOptions{Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			return err
		})
		if simCode != cCode || simCode != errAccessedUnreadable {
			t.Fatalf("GetRange reaching the pending SVK candidate range: sim=%d cgo=%d, want both %d",
				simCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("svk_range_scan_above_the_entry", func(t *testing.T) {
		// The arm with teeth for the SCAN path. The window sits strictly INSIDE the candidate
		// range and strictly ABOVE the pending entry (which lands at template@min-stamp, near
		// the range's begin), so no write-map key intersects it at all. Only the RANGE half of
		// the unreadable state can cap this scan; an engine that tracks unreadable positions as
		// a set of entry keys finds nothing here and reports clean, empty success.
		t.Parallel()
		svkPfx := []byte(pfx + "svkabove/")
		scanBegin := append(append([]byte(nil), svkPfx...), 0x01)
		scanEnd := append(append([]byte(nil), svkPfx...), 0xfe)
		simCode := simErrCode(func(tx gofdb.WritableTransaction) error {
			tx.SetVersionstampedKey(gofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(scanBegin), End: gofdb.Key(scanEnd)},
				gofdb.RangeOptions{}).GetSliceWithError()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedKey(cgofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(scanBegin), End: cgofdb.Key(scanEnd)},
				cgofdb.RangeOptions{Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			return err
		})
		if simCode != cCode || simCode != errAccessedUnreadable {
			t.Fatalf("scan of a window inside the candidate range but above the pending entry: "+
				"sim=%d cgo=%d, want both %d (sim=0 means the scan cap consults only entry keys)",
				simCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("svk_range_reverse_scan_above_the_entry", func(t *testing.T) {
		// Same window, walked downward — the reverse cap is derived from the LAST intersecting
		// range's exclusive end, a separate code path from the forward first-intersection.
		t.Parallel()
		svkPfx := []byte(pfx + "svkaboverev/")
		scanBegin := append(append([]byte(nil), svkPfx...), 0x01)
		scanEnd := append(append([]byte(nil), svkPfx...), 0xfe)
		simCode := simErrCode(func(tx gofdb.WritableTransaction) error {
			tx.SetVersionstampedKey(gofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(scanBegin), End: gofdb.Key(scanEnd)},
				gofdb.RangeOptions{Reverse: true}).GetSliceWithError()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedKey(cgofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(scanBegin), End: cgofdb.Key(scanEnd)},
				cgofdb.RangeOptions{Reverse: true, Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			return err
		})
		if simCode != cCode || simCode != errAccessedUnreadable {
			t.Fatalf("reverse scan of a window inside the candidate range but above the pending "+
				"entry: sim=%d cgo=%d, want both %d", simCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("svk_clear_lifts_the_range", func(t *testing.T) {
		// A ClearRange over the candidate range makes it readable again — the span is then
		// KNOWN empty, so there is nothing unknowable left (C++ clear() inserts readable
		// entries over the span, WriteMap.cpp:195). Without this the range fix would be a
		// one-way latch that no later operation could release.
		t.Parallel()
		cPfx := pfx + "svkclear/"
		svkPfx := []byte(cPfx + "b")
		other := append(append([]byte(nil), svkPfx...), bytes.Repeat([]byte{0x7f}, 10)...)
		clearBegin := []byte(cPfx)
		clearEnd := []byte(cPfx + "\xff")
		simCode := simErrCode(func(tx gofdb.WritableTransaction) error {
			tx.SetVersionstampedKey(gofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			tx.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(clearBegin), End: gofdb.Key(clearEnd)})
			_, err := tx.Get(gofdb.Key(other)).Get()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedKey(cgofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			tx.ClearRange(cgofdb.KeyRange{Begin: cgofdb.Key(clearBegin), End: cgofdb.Key(clearEnd)})
			_, err := tx.Get(cgofdb.Key(other)).Get()
			return err
		})
		if simCode != cCode || simCode != 0 {
			t.Fatalf("Get inside a CLEARED SVK candidate range: sim=%d cgo=%d, want both 0 — "+
				"a cleared span is known empty and readable again", simCode, cCode)
		}
	})
}

// mustPrefixRanges builds the same prefix range for both engines, failing the test rather than
// returning an error the caller would have to thread through every arm.
func mustPrefixRanges(t *testing.T, pfx string) (gofdb.Range, cgofdb.Range) {
	t.Helper()
	gr, err := gofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("go PrefixRange(%q): %v", pfx, err)
	}
	cr, err := cgofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("cgo PrefixRange(%q): %v", pfx, err)
	}
	return gr, cr
}
