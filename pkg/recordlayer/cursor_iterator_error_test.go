package recordlayer

import (
	"context"
	"errors"
	"testing"
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
	// The scan-loop path dereferences c.store; the row-limit boundary does not, so it
	// is the store-free site to pin (hasMore()'s Advance() false → Get() error surfaces).
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
