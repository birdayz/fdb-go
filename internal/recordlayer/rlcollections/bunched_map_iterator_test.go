package rlcollections

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

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
	if f.getErr != nil {
		return fdb.KeyValue{}, f.getErr
	}
	return fdb.KeyValue{}, nil
}

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

func TestBunchedMapIterator_SurfacesIteratorError(t *testing.T) {
	t.Parallel()
	injErr := errors.New("injected transaction_too_old (1007)")
	it := &BunchedMapIterator{rangeIter: &fakeRangeIterator{getErr: injErr}}
	it.advance()
	if !errors.Is(it.Err(), injErr) {
		t.Fatalf("single-map scan must surface the iterator error via Err(), got %v", it.Err())
	}
}
