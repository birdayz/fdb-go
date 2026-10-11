package fdb_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

// GetRange starts batch 1 at the call (Apple bindings/go/src/fdb/transaction.go:295-296):
// N range results are N reads under way before any is consumed.
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
		// Every read starts without any consumer waiting on it.
		deadline := time.Now().Add(20 * time.Second)
		for db.Metrics().TransactionGetRangeRequests-before < n {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("range reads started before the first wait = %d, want %d",
					db.Metrics().TransactionGetRangeRequests-before, n)
			}
			time.Sleep(time.Millisecond)
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
// GetRange that is never consumed is still issued and still conflicts (:1767, :388).
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
	// Pin the read version: GetRange's GRV runs in the background, as libfdb_c's does.
	if _, err := tr2.GetReadVersion().Get(); err != nil {
		t.Fatal(err)
	}
	tr2.GetRange(fdb.KeyRange{Begin: k('1'), End: k('2')}, fdb.RangeOptions{}) // issued, never consumed
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

// GetRange's errors arrive in libfdb_c's order: 2210 (fdb_c.cpp:996-998), then cancelled, 2004
// and 2012 (ReadYourWrites.actor.cpp:1740, :1749), on both consumption paths.
func TestGetRange_ErrorOrder(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	code := func(err error) int {
		var fe fdb.Error
		if errors.As(err, &fe) {
			return fe.Code
		}
		return 0
	}
	iterCode := func(rr fdb.RangeResult) int {
		it := rr.Iterator()
		it.Advance()
		_, err := it.Get()
		return code(err)
	}
	ok := fdb.KeyRange{Begin: fdb.Key("a"), End: fdb.Key("b")}
	illegal := fdb.KeyRange{Begin: fdb.Key{0xff, 1}, End: fdb.Key{0xff, 2}}
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		r    fdb.KeyRange
		opts fdb.RangeOptions
		want int
	}{
		{"limit<-1", ok, fdb.RangeOptions{Limit: -2}, 2012},
		{"illegal key before limit", illegal, fdb.RangeOptions{Limit: -2}, 2004},
		{"exact without limit", illegal, fdb.RangeOptions{Mode: fdb.StreamingModeExact}, 2210},
	}
	for _, c := range cases {
		if got := code(func() error { _, err := tr.GetRange(c.r, c.opts).GetSliceWithError(); return err }()); got != c.want {
			t.Errorf("%s: GetSliceWithError = %d, want %d", c.name, got, c.want)
		}
		if got := iterCode(tr.GetRange(c.r, c.opts)); got != c.want {
			t.Errorf("%s: Iterator = %d, want %d", c.name, got, c.want)
		}
	}
	tr.Cancel()
	for _, c := range []struct {
		opts fdb.RangeOptions
		want int
	}{{fdb.RangeOptions{Limit: -2}, 1025}, {fdb.RangeOptions{Mode: fdb.StreamingModeExact}, 2210}} {
		if got := iterCode(tr.GetRange(illegal, c.opts)); got != c.want {
			t.Errorf("cancelled, %+v: %d, want %d", c.opts, got, c.want)
		}
	}
	// Option errors precede selector resolution, which would read (and fail with 1025 here).
	sel := fdb.SelectorRange{Begin: fdb.LastLessThan(fdb.Key("a")), End: fdb.FirstGreaterThan(fdb.Key("b"))}
	for _, c := range []struct {
		opts fdb.RangeOptions
		want int
	}{{fdb.RangeOptions{Mode: fdb.StreamingModeExact}, 2210}, {fdb.RangeOptions{Mode: fdb.StreamingMode(99)}, 2000}} {
		if got := iterCode(tr.GetRange(sel, c.opts)); got != c.want {
			t.Errorf("cancelled selector range, %+v: %d, want %d", c.opts, got, c.want)
		}
	}
}

// A selector range is rejected on its raw selector keys before resolution reads anything:
// C++ checks cancellation, 2004, then 2012 before the read starts (ReadYourWrites.actor.cpp:1736-1749).
func TestGetRange_SelectorChecksPrecedeResolution(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cancel()
	before := db.Metrics().TransactionReadVersionsCompleted
	code := func(rr fdb.RangeResult) int {
		_, err := rr.GetSliceWithError()
		var fe fdb.Error
		if errors.As(err, &fe) {
			return fe.Code
		}
		return 0
	}
	sel := fdb.SelectorRange{Begin: fdb.LastLessThan(fdb.Key("a")), End: fdb.FirstGreaterThan(fdb.Key("b"))}
	if got := code(tr.GetRange(sel, fdb.RangeOptions{Limit: -2})); got != 2012 {
		t.Errorf("limit < -1 = %d, want 2012", got)
	}
	pastMax := fdb.SelectorRange{Begin: fdb.LastLessThan(fdb.Key("a")), End: fdb.FirstGreaterOrEqual(fdb.Key{0xff, 1})}
	if got := code(tr.GetRange(pastMax, fdb.RangeOptions{})); got != 2004 {
		t.Errorf("end past the max key = %d, want 2004", got)
	}
	if n := db.Metrics().TransactionReadVersionsCompleted - before; n != 0 {
		t.Fatalf("rejected selector ranges read %d read versions; they must do no I/O", n)
	}
}
