package recordlayer

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// fakeRangeIterator is a deterministic rangeIterator for white-box cursor tests:
// it returns `advancesLeft` trues from Advance(), then false, and surfaces `getErr`
// from Get() once exhausted — letting a test place an FDB error at an exact scan
// position (which a concrete *fdb.RangeIterator can't be made to do deterministically).
type fakeRangeIterator struct {
	advancesLeft int
	getErr       error
	advances     int
}

func (f *fakeRangeIterator) Advance() bool {
	f.advances++
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

// A transient iterator error (transaction_too_old 1007) must surface, never be
// read as end-of-data, which would silently end the scan.
func TestKeyValueCursor_IteratorErrorSurfaces(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	c := &keyValueCursor{iterator: &fakeRangeIterator{getErr: injErr}}
	_, err := c.OnNext(context.Background())
	if !errors.Is(err, injErr) {
		t.Fatalf("want the iterator error propagated, got %v", err)
	}
}

// Java's RowLimitedCursor stops at the limit without reading ahead, so the
// iterator is untouched and the token is the last returned record's.
func TestKeyValueCursor_RowLimitDoesNotReadAhead(t *testing.T) {
	t.Parallel()
	iterator := &fakeRangeIterator{advancesLeft: 1}
	last := &keyValueContinuation{suffix: []byte{0x01}}
	c := &keyValueCursor{
		iterator:       iterator,
		returned:       1,
		lastCont:       last,
		scanProperties: ScanProperties{ExecuteProperties: ExecuteProperties{ReturnedRowLimit: 1}},
	}
	for range 2 {
		res, err := c.OnNext(context.Background())
		if err != nil || res.GetNoNextReason() != ReturnLimitReached || res.GetContinuation() != last {
			t.Fatalf("got reason=%v continuation=%v err=%v, want ReturnLimitReached with the last record's token", res.GetNoNextReason(), res.GetContinuation(), err)
		}
	}
	if iterator.advances != 0 {
		t.Fatalf("row limit read ahead %d times", iterator.advances)
	}
}

// A stop token that cannot be built must fail the scan: a substitute start or
// end token would resume from the beginning or silently truncate.
func TestKeyValueCursor_StopTokenErrorSurfaces(t *testing.T) {
	t.Parallel()
	unsplitter := &keyValueCursor{
		iterator: &fakeRangeIterator{}, halted: true, haltReason: ScanLimitReached,
		contKey: fdb.Key{0x01}, prefixLength: 5,
	}
	if res, err := unsplitter.OnNext(context.Background()); err == nil {
		t.Fatalf("unsplitter stop with an unbuildable token returned %v", res.GetContinuation())
	}
	state := NewScanLimiterState()
	state.AddRecordScanned()
	bare := &keyValueCursor{
		iterator: &fakeRangeIterator{}, omitUnsplitRecordSuffix: true, initialPass: true,
		lastKey: fdb.Key{0x01}, prefixLength: 5, scanState: state, clock: state,
		scanProperties: ScanProperties{ExecuteProperties: ExecuteProperties{ScannedRecordsLimit: 1}},
	}
	if res, err := bare.OnNext(context.Background()); err == nil {
		t.Fatalf("bare stop with an unbuildable token returned %v", res.GetContinuation())
	}
}
