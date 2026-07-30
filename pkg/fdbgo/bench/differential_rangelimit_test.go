package bench

import (
	"math"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Error-CODE differentials for two client-side input-validation divergences RFC-126 closed (the
// FuzzApiCorrectness ExceptionContract audit). Both were cases where the pure-Go client SILENTLY
// ACCEPTED input that libfdb_c REJECTS — a wire-contract divergence for apps sharing a cluster across
// a Go and a C/Java client. Each asserts (a) Go and libfdb_c return the SAME code and (b) it is the
// C++-spec code. Red before the fix (Go returned 0), green after.

// TestDifferential_RangeLimitInvalid — getRange row limit. libfdb_c (api > 13, fdb_c.cpp:983 no
// negative→reverse remap) rejects a row limit < -1 with range_limits_invalid (2012), because
// GetRangeLimits::isValid (FDBTypes.h:754) accepts only rows >= 0 || rows == ROW_LIMIT_UNLIMITED(-1).
// Go used to map every limit <= 0 to "unlimited" (readpath.go:650) and silently accept -2/-100/INT_MIN.
func TestDifferential_RangeLimitInvalid(t *testing.T) {
	t.Parallel()
	gr := gofdb.KeyRange{Begin: gofdb.Key("difflim_a"), End: gofdb.Key("difflim_z")}
	cr := cgofdb.KeyRange{Begin: cgofdb.Key("difflim_a"), End: cgofdb.Key("difflim_z")}
	cases := []struct {
		limit int
		want  int // 0 = both must accept; 2012 = both must reject
	}{
		{0, 0}, {-1, 0}, {5, 0}, // -1 and 0 are unlimited in BOTH clients (not invalid)
		{-2, 2012}, {-100, 2012}, {math.MinInt32, 2012},
	}
	for _, c := range cases {
		gc := goErrCode(func(tx gofdb.Transaction) error {
			_, e := tx.GetRange(gr, gofdb.RangeOptions{Limit: c.limit}).GetSliceWithError()
			return e
		})
		cc := cgoErrCode(func(tx cgofdb.Transaction) error {
			_, e := tx.GetRange(cr, cgofdb.RangeOptions{Limit: c.limit}).GetSliceWithError()
			return e
		})
		if gc != cc {
			t.Errorf("limit=%d: DIVERGENCE go=%d cgo=%d", c.limit, gc, cc)
		}
		if gc != c.want {
			t.Errorf("limit=%d: go code=%d, want %d (libfdb_c=%d)", c.limit, gc, c.want, cc)
		}
	}
}

// TestDifferential_ExactModeWithoutLimits — the OTHER half of the range-option validation, per
// CONSUMPTION SURFACE. StreamingModeExact with no row budget is exact_mode_without_limits (2210)
// in libfdb_c (validate_and_update_parameters, bindings/c/fdb_c.cpp); the question this measures
// is which spellings of "no budget" trigger it and which of GetSlice/Iterator can reach it at all.
//
// Two things made it worth measuring rather than reading off the source:
//
//   - Limit -1 vs 0. The C entry point maps a zero limit to ROW_LIMIT_UNLIMITED(-1) before the
//     EXACT check, so 0 and -1 are the same value by the time the gate sees them — but that is a
//     two-step argument over a mutating helper, and the pure-Go client rejects both by its own
//     `Limit <= 0` spelling. Whether the two agree is a measurement.
//   - Which surface. Apple's binding rewrites the streaming mode inside GetSliceWithError (Exact
//     when a limit is set, WantAll otherwise) — but only for the batches it fetches ITSELF. The
//     FIRST batch's future is issued eagerly by getRange with the CALLER's mode
//     (transaction.go:296), so the rewrite cannot save a GetSlice whose first fetch already
//     carried Exact. The pure-Go client's GetSliceWithError genuinely never passes the mode down.
//     That is a real per-surface divergence candidate, not a symmetry to assume.
//
// SimFDB models this per surface (pkg/simfdb/range_result.go) and its differential arm
// (TestDifferential_RangeOptionValidation) is measured against the PURE-GO client, so nothing
// downstream of it can catch a Go-vs-C divergence here. This is the arm that can.
func TestDifferential_ExactModeWithoutLimits(t *testing.T) {
	t.Parallel()
	const pfx = "diffexact_"
	gr := gofdb.KeyRange{Begin: gofdb.Key(pfx + "a"), End: gofdb.Key(pfx + "z")}
	cr := cgofdb.KeyRange{Begin: cgofdb.Key(pfx + "a"), End: cgofdb.Key(pfx + "z")}

	// Seed three rows so the range is non-empty: an empty range can hide a per-batch difference
	// by never fetching a second batch.
	if c := goErrCode(func(tx gofdb.Transaction) error {
		for _, s := range []string{"b", "c", "d"} {
			tx.Set(gofdb.Key(pfx+s), []byte("v"))
		}
		return nil
	}); c != 0 {
		t.Fatalf("seed: %d", c)
	}

	// Every consumption surface of one option set, on one client. Codes only.
	//
	// The two drain loops are deliberately NOT the same shape, because the two iterators report a
	// failure differently: Apple's Advance() returns TRUE on error so the following Get() can
	// surface it (range.go:225), while the pure-Go Advance() returns FALSE (range_result.go:227)
	// and the error is only reachable from Get() afterwards. Draining the pure-Go iterator with
	// Apple's idiom reports zero rows and no error, which is how the first cut of this probe
	// mismeasured go{iter} as 0.
	goProbe := func(opts gofdb.RangeOptions) (slice, iter int) {
		slice = goErrCode(func(tx gofdb.Transaction) error {
			_, e := tx.GetRange(gr, opts).GetSliceWithError()
			return e
		})
		iter = goErrCode(func(tx gofdb.Transaction) error {
			it := tx.GetRange(gr, opts).Iterator()
			for it.Advance() {
				if _, e := it.Get(); e != nil {
					return e
				}
			}
			// Advance() went false: exhaustion or failure. Get() distinguishes them (it returns
			// a zero KeyValue and no error past the end).
			_, e := it.Get()
			return e
		})
		return slice, iter
	}
	cgoProbe := func(opts cgofdb.RangeOptions) (slice, iter int) {
		slice = cgoErrCode(func(tx cgofdb.Transaction) error {
			_, e := tx.GetRange(cr, opts).GetSliceWithError()
			return e
		})
		iter = cgoErrCode(func(tx cgofdb.Transaction) error {
			it := tx.GetRange(cr, opts).Iterator()
			for it.Advance() {
				if _, e := it.Get(); e != nil {
					return e
				}
			}
			return nil
		})
		return slice, iter
	}

	for _, c := range []struct {
		name             string
		mode             int // shared numbering: WantAll -1, Iterator 0, Exact 1
		limit            int
		wantSlice, wantI int // libfdb_c's measured code per surface
	}{
		// Both spellings of "no budget" are the same value by the time C's gate sees them: the
		// entry point maps a zero limit to ROW_LIMIT_UNLIMITED(-1) first.
		{"exact, limit 0", 1, 0, 2210, 2210},
		{"exact, limit -1", 1, -1, 2210, 2210},
		// Below ROW_LIMIT_UNLIMITED the limit is invalid rather than unlimited, so 2012 wins over
		// 2210 even though the mode is EXACT. This is the ordering a "Limit <= 0" test gets wrong.
		{"exact, limit -2", 1, -2, 2012, 2012},
		{"exact, limit -7", 1, -7, 2012, 2012},
		// Positive controls: a budget makes EXACT legal, and no other mode is gated at all.
		{"exact, limit 1", 1, 1, 0, 0},
		{"iterator, limit 0", 0, 0, 0, 0},
		{"iterator, limit -1", 0, -1, 0, 0},
		{"wantall, limit 0", -1, 0, 0, 0},
	} {
		c := c
		gs, gi := goProbe(gofdb.RangeOptions{Mode: gofdb.StreamingMode(c.mode), Limit: c.limit})
		cs, ci := cgoProbe(cgofdb.RangeOptions{Mode: cgofdb.StreamingMode(c.mode), Limit: c.limit})
		t.Logf("MEASURED %-16s go{slice:%d iter:%d} cgo{slice:%d iter:%d}", c.name, gs, gi, cs, ci)
		if gs != cs {
			t.Errorf("%s GetSlice: DIVERGENCE go=%d cgo=%d", c.name, gs, cs)
		}
		if gi != ci {
			t.Errorf("%s Iterator: DIVERGENCE go=%d cgo=%d", c.name, gi, ci)
		}
		if gs != c.wantSlice {
			t.Errorf("%s GetSlice: go code=%d, want %d (libfdb_c=%d)", c.name, gs, c.wantSlice, cs)
		}
		if gi != c.wantI {
			t.Errorf("%s Iterator: go code=%d, want %d (libfdb_c=%d)", c.name, gi, c.wantI, ci)
		}
	}
}

// TestDifferential_ConflictRangeMaxKey — addReadConflictRange/addWriteConflictRange reject an endpoint
// past getMaxReadKey/getMaxWriteKey with key_outside_legal_range (2004) (ReadYourWrites.actor.cpp:1954
// read / :2466 write). Go used to check only inverted (begin>end). Crucially this exercises the
// read/write ASYMMETRY: addReadConflictRange uses getMaxReadKey() + a metadataVersionKey exception;
// addWriteConflictRange uses getMaxWriteKey() with NO exception — they diverge when only READ_SYSTEM_KEYS
// is set (maxReadKey=\xff\xff, maxWriteKey=\xff), which a symmetric (flattened) fix would have hidden.
func TestDifferential_ConflictRangeMaxKey(t *testing.T) {
	t.Parallel()
	type krSpec struct{ begin, end string }
	read := func(opt func(any), kr krSpec) (int, int) {
		gc := goErrCode(func(tx gofdb.Transaction) error {
			if opt != nil {
				opt(tx)
			}
			return tx.AddReadConflictRange(gofdb.KeyRange{Begin: gofdb.Key(kr.begin), End: gofdb.Key(kr.end)})
		})
		cc := cgoErrCode(func(tx cgofdb.Transaction) error {
			if opt != nil {
				opt(tx)
			}
			return tx.AddReadConflictRange(cgofdb.KeyRange{Begin: cgofdb.Key(kr.begin), End: cgofdb.Key(kr.end)})
		})
		return gc, cc
	}
	write := func(opt func(any), kr krSpec) (int, int) {
		gc := goErrCode(func(tx gofdb.Transaction) error {
			if opt != nil {
				opt(tx)
			}
			return tx.AddWriteConflictRange(gofdb.KeyRange{Begin: gofdb.Key(kr.begin), End: gofdb.Key(kr.end)})
		})
		cc := cgoErrCode(func(tx cgofdb.Transaction) error {
			if opt != nil {
				opt(tx)
			}
			return tx.AddWriteConflictRange(cgofdb.KeyRange{Begin: cgofdb.Key(kr.begin), End: cgofdb.Key(kr.end)})
		})
		return gc, cc
	}
	// opt setters that work on either client's typed Transaction.
	readSysKeys := func(tx any) {
		switch v := tx.(type) {
		case gofdb.Transaction:
			_ = v.Options().SetReadSystemKeys()
		case cgofdb.Transaction:
			_ = v.Options().SetReadSystemKeys()
		}
	}

	check := func(name string, gc, cc, want int) {
		if gc != cc {
			t.Errorf("%s: DIVERGENCE go=%d cgo=%d", name, gc, cc)
		}
		if gc != want {
			t.Errorf("%s: go code=%d, want %d (libfdb_c=%d)", name, gc, want, cc)
		}
	}

	// Default txn (maxReadKey == maxWriteKey == \xff): endpoint past \xff\xff → 2004 on both methods.
	gc, cc := read(nil, krSpec{"a", "\xff\xff\xff"})
	check("read>maxKey", gc, cc, 2004)
	gc, cc = write(nil, krSpec{"a", "\xff\xff\xff"})
	check("write>maxKey", gc, cc, 2004)
	// In-range: accepted by both.
	gc, cc = read(nil, krSpec{"a", "m"})
	check("read in-range", gc, cc, 0)
	gc, cc = write(nil, krSpec{"a", "m"})
	check("write in-range", gc, cc, 0)
	// metadataVersionKey range — exempt on the READ path only (begin==MVK && end==MVK\x00).
	gc, cc = read(nil, krSpec{"\xff/metadataVersion", "\xff/metadataVersion\x00"})
	check("read MVK exception", gc, cc, 0)

	// READ_SYSTEM_KEYS asymmetry: maxReadKey=\xff\xff, maxWriteKey=\xff. An endpoint in (\xff, \xff\xff]
	// is in range for the READ method but past the WRITE method's max → read accepts, write rejects.
	gc, cc = read(readSysKeys, krSpec{"a", "\xff\x05"})
	check("read sysKeys allows", gc, cc, 0)
	gc, cc = write(readSysKeys, krSpec{"a", "\xff\x05"})
	check("write sysKeys rejects", gc, cc, 2004)
}

// TestDifferential_RangeSplitPointsMaxKey — getRangeSplitPoints also rejects an endpoint past
// getMaxReadKey() with key_outside_legal_range (2004) (ReadYourWrites.actor.cpp:1875-1877), the sibling
// read-path entry the first RFC-126 cut missed (FDB-C-dev review). Go used to silently accept it.
func TestDifferential_RangeSplitPointsMaxKey(t *testing.T) {
	t.Parallel()
	splitErr := func(begin, end string) (int, int) {
		gc := goErrCode(func(tx gofdb.Transaction) error {
			_, e := tx.GetRangeSplitPoints(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, 1000).Get()
			return e
		})
		cc := cgoErrCode(func(tx cgofdb.Transaction) error {
			_, e := tx.GetRangeSplitPoints(cgofdb.KeyRange{Begin: cgofdb.Key(begin), End: cgofdb.Key(end)}, 1000).Get()
			return e
		})
		return gc, cc
	}
	if gc, cc := splitErr("dsp_a", "dsp_z"); gc != cc || gc != 0 {
		t.Errorf("in-range: go=%d cgo=%d, want both 0", gc, cc)
	}
	if gc, cc := splitErr("a", "\xff\xff\xff"); gc != cc || gc != 2004 {
		t.Errorf(">maxKey: go=%d cgo=%d, want both 2004", gc, cc)
	}
	// Inverted (begin>end) must report inverted_range (2005) — libfdb_c checks inversion FIRST (the
	// KeyRangeRef ctor throws before the RYW maxKey check), so an inverted-AND-out-of-range range is
	// 2005, NOT 2004.
	if gc, cc := splitErr("z", "a"); gc != cc || gc != 2005 {
		t.Errorf("inverted in-range: go=%d cgo=%d, want both 2005", gc, cc)
	}
	if gc, cc := splitErr("\xff\xff\xff\xff", "a"); gc != cc || gc != 2005 {
		t.Errorf("inverted+>maxKey: go=%d cgo=%d, want both 2005 (inverted wins)", gc, cc)
	}

	// GetEstimatedRangeSizeBytes is the same KeyRangeRef-construction class: an inverted range is
	// inverted_range (2005) on both clients (libfdb_c's C-API range construction throws first).
	sizeErr := func(begin, end string) (int, int) {
		gc := goErrCode(func(tx gofdb.Transaction) error {
			_, e := tx.GetEstimatedRangeSizeBytes(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}).Get()
			return e
		})
		cc := cgoErrCode(func(tx cgofdb.Transaction) error {
			_, e := tx.GetEstimatedRangeSizeBytes(cgofdb.KeyRange{Begin: cgofdb.Key(begin), End: cgofdb.Key(end)}).Get()
			return e
		})
		return gc, cc
	}
	if gc, cc := sizeErr("z", "a"); gc != cc || gc != 2005 {
		t.Errorf("estimatedSize inverted: go=%d cgo=%d, want both 2005", gc, cc)
	}
}
