package fdb_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// GetRange sends batch 1 at the call (Apple bindings/go/src/fdb/transaction.go:295-296):
// N range results are N requests in flight before any is read.
func TestGetRange_RequestsInFlightBeforeFirstWait(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	prefix := []byte(t.Name() + "/")
	key := func(i int) fdb.Key { return fdb.Key(append(bytes.Clone(prefix), []byte(fmt.Sprintf("%02d", i))...)) }
	const n = 5
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := 0; i < n; i++ {
			tr.Set(key(i), []byte{byte(i)})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		// Pin the read version and warm the location cache, outside the ranges under test
		// (a range this transaction already read is answered from its RYW cache).
		if _, err := tr.GetRange(fdb.KeyRange{Begin: key(n), End: key(n + 1)}, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
			return nil, err
		}
		before := db.Metrics().TransactionGetRangeRequests
		results := make([]fdb.RangeResult, n)
		for i := range results {
			results[i] = tr.GetRange(fdb.KeyRange{Begin: key(i), End: key(i + 1)}, fdb.RangeOptions{})
		}
		if got := db.Metrics().TransactionGetRangeRequests - before; got != n {
			return nil, fmt.Errorf("getKeyValues requests sent before the first wait = %d, want %d", got, n)
		}
		for i, rr := range results {
			it := rr.Iterator()
			if !it.Advance() {
				_, err := it.Get()
				return nil, fmt.Errorf("range %d empty: %v", i, err)
			}
			if kv := it.MustGet(); !bytes.Equal(kv.Key, key(i)) || it.Advance() {
				return nil, fmt.Errorf("range %d = %q, want only %q", i, kv.Key, key(i))
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Batch 1 is shared by every consumer of one RangeResult (Apple range.go:173-176),
// each getting its own copy of the rows.
func TestGetRange_ConsumersShareFirstBatch(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	prefix := []byte(t.Name() + "/")
	k := fdb.Key(append(bytes.Clone(prefix), 'a'))
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(k, []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(append(bytes.Clone(prefix), 0xff))}, fdb.RangeOptions{})
		tr.Clear(k) // after the issue: neither consumer may see it
		first, err := rr.GetSliceWithError()
		if err != nil || len(first) != 1 {
			return nil, fmt.Errorf("first consumer = %v, %v", first, err)
		}
		first[0].Value[0] = 'X'
		it := rr.Iterator()
		if !it.Advance() || string(it.MustGet().Value) != "v" {
			return nil, errors.New("second consumer lost batch 1 or shares the first consumer's bytes")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The rows of batch 1 are the write map as of GetRange (ReadYourWrites.actor.cpp:382), and a
// never-consumed GetRange still conflicts: commit waits for it (:1359-1360, :1767).
func TestGetRange_IssueTimeViewAndUnconsumedConflict(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	prefix := []byte(t.Name() + "/")
	k := func(c byte) fdb.Key { return fdb.Key(append(bytes.Clone(prefix), c)) }
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(k('0'), []byte("a"))
		tr.Set(k('1'), []byte("b"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cancel()
	rr := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(prefix), End: k(0xff)}, fdb.RangeOptions{})
	tr.Set(k('5'), []byte("late"))
	tr.Clear(k('0'))
	rows, err := rr.GetSliceWithError()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, kv := range rows {
		got = append(got, string(kv.Value))
	}
	if fmt.Sprint(got) != "[a b]" {
		t.Fatalf("rows = %q, want the issue-time view [a b]", got)
	}

	tr2, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr2.Cancel()
	tr2.GetRange(fdb.KeyRange{Begin: k('1'), End: k('2')}, fdb.RangeOptions{}) // never consumed
	if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
		w.Set(k('1'), []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	tr2.Set(k('w'), []byte("x"))
	var fe fdb.Error
	if err := tr2.Commit().Get(); !errors.As(err, &fe) || fe.Code != 1020 {
		t.Fatalf("commit = %v, want not_committed: the unconsumed read's conflict must count", err)
	}
}
