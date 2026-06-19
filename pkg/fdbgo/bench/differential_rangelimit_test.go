package bench

import (
	"math"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
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
}
