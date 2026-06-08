package recordlayer

import (
	"context"
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// fakeRangeIterator is a deterministic rangeIterator for white-box cursor tests:
// it returns `advancesLeft` trues from Advance(), then false, and surfaces `getErr`
// from Get() once exhausted — letting a test place an FDB error at an exact scan
// position (which a concrete *fdb.RangeIterator can't be made to do deterministically).
type fakeRangeIterator struct {
	advancesLeft int
	getErr       error
}

func (f *fakeRangeIterator) Advance() bool {
	if f.advancesLeft > 0 {
		f.advancesLeft--
		return true
	}
	return false
}

func (f *fakeRangeIterator) Get() (fdb.KeyValue, error) {
	if f.advancesLeft <= 0 {
		return fdb.KeyValue{}, f.getErr
	}
	return fdb.KeyValue{}, nil
}

// TestKeyValueCursor_RowLimitBoundarySurfacesIteratorError pins that a transient FDB
// iterator error (transaction_too_old 1007, timeout) landing exactly on the
// ReturnedRowLimit boundary is SURFACED, not silently collapsed into SourceExhausted.
// Pre-fix, hasMoreKVs returned `iterator.Advance()` without checking Get()'s stored
// error, so OnNext read the error as end-of-data and ended the scan — silently losing
// every remaining row. (This dimension — an error at the limit probe — had no coverage,
// neither here nor for nextKV's twin check.)
func TestKeyValueCursor_RowLimitBoundarySurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	c := &keyValueCursor{
		iterator:       &fakeRangeIterator{advancesLeft: 0, getErr: injErr},
		recordsRead:    1, // already returned the requested row → next OnNext probes for more
		scanProperties: ScanProperties{ExecuteProperties: ExecuteProperties{ReturnedRowLimit: 1}},
	}

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("a transient iterator error at the row-limit boundary must surface, not be collapsed to SourceExhausted (silent row loss)")
	}
	if !errors.Is(err, injErr) {
		t.Fatalf("want the iterator error propagated, got %v", err)
	}
}

// TestKeyValueCursor_RowLimitBoundaryReportsMoreWhenAvailable guards the happy path:
// when the iterator genuinely has more rows at the boundary, OnNext reports
// ReturnLimitReached (not SourceExhausted) and no error.
func TestKeyValueCursor_RowLimitBoundaryReportsMoreWhenAvailable(t *testing.T) {
	t.Parallel()
	c := &keyValueCursor{
		iterator:       &fakeRangeIterator{advancesLeft: 1}, // one more KV available
		recordsRead:    1,
		continuation:   []byte{0x01}, // last-returned key suffix (ReturnLimitReached needs a non-end continuation)
		scanProperties: ScanProperties{ExecuteProperties: ExecuteProperties{ReturnedRowLimit: 1}},
	}
	res, err := c.OnNext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.GetNoNextReason() != ReturnLimitReached {
		t.Fatalf("want ReturnLimitReached, got %v", res.GetNoNextReason())
	}
}
