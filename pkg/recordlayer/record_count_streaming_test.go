package recordlayer

import (
	"encoding/binary"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// These pin the STREAMING dimension of the grouped record-count roll-up
// (sumRecordCounts, the port of Java's MoreAsyncUtil.reduce over
// tr.getRange(getSubspace().range(Tuple.from(RECORD_COUNT_KEY))) at
// FDBRecordStore.java:2307-2310).
//
// The roll-up used to call GetSliceWithError and sum the resulting slice, which is
// O(number of groups) memory on a path that both GetRecordCount and the evolution
// rebuild's count selection sit on. Correctness at a few thousand groups (the
// "GetRecordCount rolls up thousands of groups exactly" spec in
// record_count_rollup_test.go) cannot see that difference at all: both shapes answer
// the same number.
//
// So the streaming property gets its own pin, and it is pinned STRUCTURALLY, in two
// independent directions:
//
//  1. The materializing entry point must never be called. streamOnlyRangeResult fails
//     the test from GetSliceWithError.
//  2. Each element must be consumed BEFORE the next Advance(), retaining nothing. The
//     iterator hands out values backed by ONE buffer that it overwrites on every
//     Advance(), so a fold that collects first and sums later reads the last value n
//     times and gets an arithmetically impossible total.
//
// Direction 2 is what makes direction 1 more than a gesture: an "iterate, but append
// to a slice" rewrite passes 1 and fails 2.
//
// WHAT THIS DOES NOT PIN: bytes. There is no assertion here on heap size, and there
// deliberately isn't one — a heap measurement taken after the fold returns sees only
// what was RETAINED, which is zero for both shapes, and a peak-memory measurement is
// not available to a test sharing a process with the rest of this package. What is
// pinned is the structural property that produces the memory bound (incremental
// consumption, O(1) live state), not the bound itself. Nothing here would notice if
// the FDB client's own per-batch buffering grew.

// streamOnlyRangeResult is an fdb.RangeResult that yields n synthetic count slots,
// generating each on demand into a shared buffer.
type streamOnlyRangeResult struct {
	t *testing.T
	n int
	// iterErrAt is the 1-based index at which Advance() stops and Get() reports
	// errInjectedRangeRead; 0 means the range is clean.
	iterErrAt int
}

var errInjectedRangeRead = errors.New("injected transaction_too_old (1007)")

func (r *streamOnlyRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) {
	r.t.Helper()
	r.t.Fatal("the grouped record-count roll-up materialized the whole count subspace: " +
		"it must stream the range (Java reduces over the AsyncIterable, FDBRecordStore.java:2307-2310). " +
		"Collecting every group first makes peak memory proportional to the cardinality of the " +
		"record-count key, so a high-cardinality grouped store can OOM GetRecordCount and the " +
		"evolution rebuild's count selection")
	return nil, nil
}

func (r *streamOnlyRangeResult) GetSliceOrPanic() []fdb.KeyValue {
	kvs, _ := r.GetSliceWithError()
	return kvs
}

func (r *streamOnlyRangeResult) Iterator() fdb.RangeIterator {
	return &streamOnlyRangeIterator{
		rr:    r,
		key:   make([]byte, 8),
		value: make([]byte, 8),
	}
}

type streamOnlyRangeIterator struct {
	rr  *streamOnlyRangeResult
	pos int
	// key and value are ONE buffer pair reused for every element: an element is
	// valid only until the next Advance(). A consumer that keeps the fdb.KeyValue
	// past that point ends up with every element aliasing the final one.
	key   []byte
	value []byte
	err   error
}

func (it *streamOnlyRangeIterator) Advance() bool {
	if it.err != nil {
		return false
	}
	if it.rr.iterErrAt > 0 && it.pos+1 == it.rr.iterErrAt {
		it.err = errInjectedRangeRead
		return false
	}
	if it.pos >= it.rr.n {
		return false
	}
	it.pos++
	binary.BigEndian.PutUint64(it.key, uint64(it.pos))
	// Each slot holds a DIFFERENT count, so summing the retained elements after the
	// drain (rather than as they arrive) is arithmetically distinguishable.
	binary.LittleEndian.PutUint64(it.value, uint64(it.pos))
	return true
}

func (it *streamOnlyRangeIterator) Get() (fdb.KeyValue, error) {
	if it.err != nil {
		return fdb.KeyValue{}, it.err
	}
	return fdb.KeyValue{Key: it.key, Value: it.value}, nil
}

func (it *streamOnlyRangeIterator) MustGet() fdb.KeyValue {
	kv, err := it.Get()
	if err != nil {
		panic(err)
	}
	return kv
}

func (it *streamOnlyRangeIterator) SetTraceLog(func(iteration, requested, returned int, more bool, err error)) {
}

// TestSumRecordCounts_StreamsInsteadOfMaterializing is the streaming pin. See the
// file comment for exactly what it does and does not establish.
func TestSumRecordCounts_StreamsInsteadOfMaterializing(t *testing.T) {
	t.Parallel()

	// Large enough that a materializing fold would allocate hundreds of megabytes
	// for the slice it is forbidden from building, and far past any single
	// range-read batch.
	const groups = 2_000_000

	total, err := sumRecordCounts(&streamOnlyRangeResult{t: t, n: groups})
	if err != nil {
		t.Fatalf("sumRecordCounts: %v", err)
	}

	// Slot i holds count i, so the streamed fold must total 1+2+...+n.
	want := int64(groups) * int64(groups+1) / 2
	if total != want {
		if total == int64(groups)*int64(groups) {
			t.Fatalf("sumRecordCounts = %d, want %d: every element decoded as the LAST one, "+
				"so the fold retained the elements and summed them after the drain instead of "+
				"consuming each as it arrived. Live state must be one accumulator, not one "+
				"entry per group — the count subspace has as many slots as the record-count "+
				"key has distinct values", total, want)
		}
		t.Fatalf("sumRecordCounts = %d, want %d", total, want)
	}
}

// TestSumRecordCounts_SurfacesIteratorError pins the other half of streaming: because
// Advance() returns false on a transient error exactly as it does on clean exhaustion,
// a fold that trusts the loop's exit answers a plausible, too-small total instead of
// failing. GetSliceWithError returned that error directly; the iterator does not.
func TestSumRecordCounts_SurfacesIteratorError(t *testing.T) {
	t.Parallel()

	rr := &streamOnlyRangeResult{t: t, n: 100, iterErrAt: 10}
	total, err := sumRecordCounts(rr)
	if !errors.Is(err, errInjectedRangeRead) {
		t.Fatalf("the roll-up must surface the iterator error, got total=%d err=%v", total, err)
	}
	if total != 0 {
		t.Fatalf("a failed roll-up must not return a partial total, got %d", total)
	}
}

// TestSumRecordCounts_EmptyRange pins the zero case: no slots is a total of 0, not an
// error. DeleteAllRecords leaves the subspace empty, and a store that has never
// counted has no slots at all.
func TestSumRecordCounts_EmptyRange(t *testing.T) {
	t.Parallel()

	total, err := sumRecordCounts(&streamOnlyRangeResult{t: t, n: 0})
	if err != nil {
		t.Fatalf("sumRecordCounts over an empty range: %v", err)
	}
	if total != 0 {
		t.Fatalf("empty count subspace must total 0, got %d", total)
	}
}
