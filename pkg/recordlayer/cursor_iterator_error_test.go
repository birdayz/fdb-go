package recordlayer

import (
	"context"
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
)

// These pin the same silent-row-loss fix as key_value_cursor_unit_test.go, but for
// the index/count/bitmap/record-key cursors: at a scan boundary the underlying
// *fdb.RangeIterator's Advance() returns false on a transient FDB error
// (transaction_too_old 1007, timeout) just as it does on clean exhaustion, so a
// cursor that reports SourceExhausted on Advance()==false WITHOUT checking Get()
// silently truncates the scan. Each cursor must now surface the stored Get() error.
// The fake injects Advance()==false + a stored Get() error at the exact site.

func TestIndexCursor_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")

	t.Run("scan-path", func(t *testing.T) {
		t.Parallel()
		c := &indexCursor{iterator: &fakeRangeIterator{getErr: injErr}} // no limits → reaches the scan Advance()
		_, err := c.OnNext(context.Background())
		if !errors.Is(err, injErr) {
			t.Fatalf("index scan must surface the iterator error, got %v", err)
		}
	})

	t.Run("row-limit-boundary", func(t *testing.T) {
		t.Parallel()
		c := &indexCursor{
			iterator:    &fakeRangeIterator{getErr: injErr},
			recordsRead: 1,
			scanProps:   ScanProperties{ExecuteProperties: ExecuteProperties{ReturnedRowLimit: 1}},
		}
		_, err := c.OnNext(context.Background())
		if !errors.Is(err, injErr) {
			t.Fatalf("index scan must surface the error at the row-limit boundary, got %v", err)
		}
	})
}

func TestCountKVCursor_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")

	t.Run("scan-path", func(t *testing.T) {
		t.Parallel()
		c := &countKVCursor{iterator: &fakeRangeIterator{getErr: injErr}}
		_, err := c.OnNext(context.Background())
		if !errors.Is(err, injErr) {
			t.Fatalf("count index scan must surface the iterator error, got %v", err)
		}
	})

	t.Run("row-limit-boundary", func(t *testing.T) {
		t.Parallel()
		c := &countKVCursor{
			iterator:  &fakeRangeIterator{getErr: injErr},
			returned:  1,
			scanProps: ScanProperties{ExecuteProperties: ExecuteProperties{ReturnedRowLimit: 1}},
		}
		_, err := c.OnNext(context.Background())
		if !errors.Is(err, injErr) {
			t.Fatalf("count index scan must surface the error at the row-limit boundary, got %v", err)
		}
	})
}

func TestBitmapKVCursor_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")

	t.Run("scan-path", func(t *testing.T) {
		t.Parallel()
		c := &bitmapKVCursor{iterator: &fakeRangeIterator{getErr: injErr}}
		_, err := c.OnNext(context.Background())
		if !errors.Is(err, injErr) {
			t.Fatalf("bitmap index scan must surface the iterator error, got %v", err)
		}
	})

	t.Run("row-limit-boundary", func(t *testing.T) {
		t.Parallel()
		c := &bitmapKVCursor{
			iterator:    &fakeRangeIterator{getErr: injErr},
			recordsRead: 1,
			scanProps:   ScanProperties{ExecuteProperties: ExecuteProperties{ReturnedRowLimit: 1}},
		}
		_, err := c.OnNext(context.Background())
		if !errors.Is(err, injErr) {
			t.Fatalf("bitmap index scan must surface the error at the row-limit boundary, got %v", err)
		}
	})
}

func TestRecordKeyCursor_SurfacesIteratorErrorAtBoundary(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	// Row-limit boundary site (hasMore()'s Advance() false → Get() error surfaces).
	c := &recordKeyCursor{
		iterator:       &fakeRangeIterator{getErr: injErr},
		keysReturned:   1,
		scanProperties: ScanProperties{ExecuteProperties: ExecuteProperties{ReturnedRowLimit: 1}},
	}
	_, err := c.OnNext(context.Background())
	if !errors.Is(err, injErr) {
		t.Fatalf("record-key scan must surface the error at the row-limit boundary, got %v", err)
	}
}

func TestRecordKeyCursor_SurfacesIteratorErrorInScanLoop(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	// Scan-loop site (no limits → past the boundary into the loop). This path derefs
	// c.store.subspace, so a minimal store is supplied.
	c := &recordKeyCursor{
		iterator: &fakeRangeIterator{getErr: injErr},
		store:    &FDBRecordStore{subspace: subspace.Sub("t")},
	}
	_, err := c.OnNext(context.Background())
	if !errors.Is(err, injErr) {
		t.Fatalf("record-key scan loop must surface the iterator error, got %v", err)
	}
}

// TestBunchedMapMultiIterator_SurfacesIteratorError pins the same fix for the
// BunchedMap multi-iterator that backs the LIVE text-index scan (textCursor.Err()):
// a transient FDB error landing on nextKV's Advance() must be recorded in iterErr and
// surfaced by Err(), not swallowed as clean end-of-data (which truncated the scan).
func TestBunchedMapMultiIterator_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	it := &BunchedMapMultiIterator{rangeIter: &fakeRangeIterator{getErr: injErr}}
	if _, ok := it.nextKV(); ok {
		t.Fatal("nextKV must report not-ok on an iterator error")
	}
	if !errors.Is(it.Err(), injErr) {
		t.Fatalf("text-index scan must surface the iterator error via Err(), got %v", it.Err())
	}
}

// TestBunchedMapIterator_SurfacesIteratorError pins the single-map iterator's
// advance() — same Advance()/Get() swallow.
func TestBunchedMapIterator_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	it := &BunchedMapIterator{rangeIter: &fakeRangeIterator{getErr: injErr}}
	it.advance()
	if !errors.Is(it.Err(), injErr) {
		t.Fatalf("single-map scan must surface the iterator error via Err(), got %v", it.Err())
	}
}
