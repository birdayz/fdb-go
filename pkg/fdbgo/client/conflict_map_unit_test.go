package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// conflictMapKeys is a small keyspace whose keys include each other's
// successors ("a", "a\x00"), so keyAfter bounds meet and touch.
var conflictMapKeys = [][]byte{
	{},
	[]byte("a"), []byte("a\x00"), []byte("a\x00\x00"), []byte("ab"), []byte("b"),
	[]byte("b\x00"), []byte("c"), []byte("cd"), []byte("d"), []byte("e"), []byte("e\x00"),
}

func randomConflictRange(rng *rand.Rand) ([]byte, []byte) {
	b := conflictMapKeys[rng.IntN(len(conflictMapKeys))]
	e := conflictMapKeys[rng.IntN(len(conflictMapKeys))]
	if bytes.Compare(b, e) > 0 {
		b, e = e, b
	}
	return b, e
}

// A write map's nodes report the same write-conflict runs from their log as
// once built: what Commit ships for a write-only transaction (log) and for one
// that read (built) are the C++ WriteMap's conflict runs either way. An
// unreadable (SVK) range drops the span's conflict flags from built nodes, as
// C++ does, and the caller keeps them (addUnreadableRangeKeeping): with those,
// the runs are the union of every conflict added, which the log reports.
func TestWriteConflictsFromLogMatchBuiltNodes(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(3, 5))
	for iter := range 3000 {
		var logged, built, late rywNodes
		built.build()
		var kept []KeyRange
		var a boundaryArena
		var ops []string
		unreadable := false
		for range 1 + rng.IntN(10) {
			b, e := randomConflictRange(rng)
			switch rng.IntN(4) {
			case 0:
				conflict, vs := rng.IntN(3) > 0, rng.IntN(5) == 0
				for _, n := range []*rywNodes{&logged, &built, &late} {
					n.mutate(b, conflict, vs)
				}
				ops = append(ops, fmt.Sprintf("mutate(%q,%v,%v)", b, conflict, vs))
			case 1:
				conflict := rng.IntN(2) == 0
				for _, n := range []*rywNodes{&logged, &built, &late} {
					n.clear(b, e, conflict)
				}
				ops = append(ops, fmt.Sprintf("clear(%q,%q,%v)", b, e, conflict))
			case 2:
				for _, n := range []*rywNodes{&logged, &built, &late} {
					n.addConflictRange(b, e)
				}
				ops = append(ops, fmt.Sprintf("conflict(%q,%q)", b, e))
			case 3:
				if bytes.Compare(b, e) >= 0 {
					continue
				}
				unreadable = true
				kept = built.appendWriteConflictsIn(kept, &a, b, e)
				built.addUnmodifiedAndUnreadableRange(b, e)
				ops = append(ops, fmt.Sprintf("unreadable(%q,%q)", b, e))
			}
		}
		fromLog := logged.appendWriteConflicts(nil, &a)
		got := coalesceOwnedWriteConflicts(append(built.appendWriteConflicts(nil, &a), kept...))
		if !slices.EqualFunc(fromLog, got, equalCommitConflictRange) {
			t.Fatalf("iter %d %v: log %q, built with kept %q", iter, ops, fromLog, got)
		}
		if !unreadable {
			late.build()
			if got := late.appendWriteConflicts(nil, &a); !slices.EqualFunc(fromLog, got, equalCommitConflictRange) {
				t.Fatalf("iter %d %v: log %q, built from the log %q", iter, ops, fromLog, got)
			}
		}
	}
}

// The read-conflict map holds the coalesced union of what was inserted, as
// readConflictMap computes it from the list.
func TestReadConflictMapMatchesCoalescedList(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 9))
	for iter := range 3000 {
		tx := &Transaction{}
		var list []KeyRange
		for range 1 + rng.IntN(12) {
			b, e := randomConflictRange(rng)
			after := rng.IntN(3) == 0
			bb, eb := rywBoundary{key: b}, rywBoundary{key: e, after: after}
			list = append(list, KeyRange{Begin: bb.materialize(), End: bytes.Clone(eb.materialize())})
			tx.conflictMu.Lock()
			tx.insertReadConflictLocked(bb, eb)
			tx.conflictMu.Unlock()
		}
		want := readConflictMap(list)
		if got := tx.readConflictRanges(); !slices.EqualFunc(got, want, equalCommitConflictRange) {
			t.Fatalf("iter %d: inserted %q: map %q, want %q", iter, list, got, want)
		}
	}
}

// appendConflictSpans is updateConflictMap's segment walk: it must yield the
// sub-ranges the boundary-sorting implementation it replaced did.
func TestConflictSpansMatchBoundarySort(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(11, 13))
	for iter := range 3000 {
		c := &rywCache{}
		var ops []string
		for range rng.IntN(8) {
			b, e := randomConflictRange(rng)
			switch rng.IntN(4) {
			case 0:
				c.set(b, []byte("v"))
				ops = append(ops, fmt.Sprintf("set(%q)", b))
			case 1:
				c.clear(b)
				ops = append(ops, fmt.Sprintf("clear(%q)", b))
			case 2:
				c.clearRange(b, e)
				ops = append(ops, fmt.Sprintf("clearRange(%q,%q)", b, e))
			case 3:
				c.atomic(MutAddValue, b, []byte{1})
				ops = append(ops, fmt.Sprintf("add(%q)", b))
			}
		}
		b, e := randomConflictRange(rng)
		w := c.liveLocked()
		var got [][2][]byte
		for _, s := range w.appendConflictSpans(nil, b, e) {
			got = append(got, [2][]byte{s.begin.materialize(), s.end.materialize()})
		}
		want := conflictRangesBySort(w, b, e)
		if !slices.EqualFunc(got, want, func(x, y [2][]byte) bool { return bytes.Equal(x[0], y[0]) && bytes.Equal(x[1], y[1]) }) {
			t.Fatalf("iter %d %v over [%q,%q): spans %q, want %q", iter, ops, b, e, got, want)
		}
	}
}

// conflictRangesBySort is the implementation appendConflictSpans replaced: it
// collects every boundary in the window, sorts them and classifies each segment.
func conflictRangesBySort(w *rywWrites, begin, end []byte) [][2][]byte {
	if bytes.Compare(begin, end) >= 0 {
		return nil
	}
	w.ensureSorted()

	// Collect the segment boundaries within [begin, end]: begin, end, every write key in
	// range AND its keyAfter (so an operation occupies its own single-key segment [k,
	// keyAfter(k)) and the following gap is classified separately), and every cleared-range
	// bound clamped into the window. Each write key's segment carries its entry.
	type bound struct {
		key   []byte
		entry *rywEntry // the operation at key, when [key, keyAfter(key)) is its segment
	}
	bounds := []bound{{key: append([]byte(nil), begin...)}, {key: append([]byte(nil), end...)}}
	addBound := func(b []byte, e *rywEntry) {
		if bytes.Compare(b, begin) > 0 && bytes.Compare(b, end) < 0 {
			bounds = append(bounds, bound{key: append([]byte(nil), b...), entry: e})
		}
	}
	for cur := w.sorted.cursor(w.writeIndexOf(begin)); cur.valid() && cur.item().key < string(end); cur.next() {
		x := cur.item()
		k := []byte(x.key)
		if bytes.Equal(k, begin) {
			bounds[0].entry = &x.e
		}
		addBound(k, &x.e)
		addBound(keyAfterBytes(k), nil)
	}
	first := rangeIndex(&w.cleared, func(r *rywRange) bool { return bytes.Compare(r.end, begin) > 0 })
	for cur := w.cleared.cursor(first); cur.valid() && bytes.Compare(cur.item().begin, end) < 0; cur.next() {
		r := cur.item()
		addBound(r.begin, nil)
		addBound(r.end, nil)
	}
	slices.SortFunc(bounds, func(a, b bound) int { return bytes.Compare(a.key, b.key) })

	var out [][2][]byte
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i].key, bounds[i+1].key
		if bytes.Equal(lo, hi) {
			continue // deduped boundary
		}
		entry := bounds[i].entry
		for j := i - 1; entry == nil && j >= 0 && bytes.Equal(bounds[j].key, lo); j-- {
			entry = bounds[j].entry // a duplicate of lo that carries its entry
		}
		conflict := false
		if entry != nil && bytes.Equal(hi, keyAfterBytes(lo)) {
			// Operation key occupying the single-key segment [lo, keyAfter(lo)).
			conflict = entry.isDependentLocked()
		} else if w.isCleared(lo) {
			conflict = false // CLEARED range — no DB read.
		} else {
			conflict = true // UNMODIFIED gap — a DB read resolved it.
		}
		if !conflict {
			continue
		}
		// Coalesce with the previous sub-range if adjacent.
		if n := len(out); n > 0 && bytes.Equal(out[n-1][1], lo) {
			out[n-1][1] = append([]byte(nil), hi...)
		} else {
			out = append(out, [2][]byte{append([]byte(nil), lo...), append([]byte(nil), hi...)})
		}
	}
	return out
}

// The write map's arena hands out capped, never-nil slices whose bytes are
// never handed out again, whatever their sizes, so a kept key or value cannot
// change under the version or read holding it.
func TestRYWKeepArenaNeverReuses(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	rng := rand.New(rand.NewPCG(1, 2))
	type kept struct {
		b    []byte
		want byte
	}
	var all []kept
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.keepLocked(0); s == nil || len(s) != 0 {
		t.Fatal("an empty kept slice must be non-nil and empty")
	}
	for i := range 5000 {
		n := rng.IntN(64)
		switch rng.IntN(50) {
		case 0:
			n = keepChunkMax/4 - rng.IntN(2) // at the own-allocation bound
		case 1:
			n = 3000 + rng.IntN(6000) // larger than a young chunk
		}
		s := c.keepLocked(n)
		if len(s) != n || cap(s) != n {
			t.Fatalf("keep %d: len %d cap %d", n, len(s), cap(s))
		}
		for _, b := range s {
			if b != 0 {
				t.Fatalf("keep %d: not zeroed", n)
			}
		}
		fill := byte(i)
		for j := range s {
			s[j] = fill
		}
		all = append(all, kept{s, fill})
	}
	for i, k := range all {
		for _, b := range k.b {
			if b != k.want {
				t.Fatalf("kept slice %d was overwritten", i)
			}
		}
	}
}

// An explicit write conflict does not consume NEXT_WRITE_NO_WRITE_CONFLICT_RANGE:
// only writes do (RYW addWriteConflictRange, ReadYourWrites.actor.cpp:2461-2500).
func TestExplicitWriteConflictKeepsNextWriteFlag(t *testing.T) {
	t.Parallel()
	for _, rywDisabled := range []bool{false, true} {
		tx := &Transaction{rywDisabled: rywDisabled}
		tx.SetNextWriteNoWriteConflictRange()
		tx.AddWriteConflictKey([]byte("k9"))
		tx.Set([]byte("k10"), []byte("v"))
		tx.Set([]byte("k11"), []byte("v"))
		got := fmt.Sprintf("%q", tx.writeConflictRanges())
		want := fmt.Sprintf("%q", []KeyRange{{Begin: []byte("k11"), End: []byte("k11\x00")}, {Begin: []byte("k9"), End: []byte("k9\x00")}})
		if rywDisabled {
			want = fmt.Sprintf("%q", []KeyRange{{Begin: []byte("k9"), End: []byte("k9\x00")}, {Begin: []byte("k11"), End: []byte("k11\x00")}})
		}
		if got != want {
			t.Fatalf("rywDisabled=%v: write conflicts %s, want %s (k9 kept, k10's skipped)", rywDisabled, got, want)
		}
	}
}

// An illegal write's error fails every later operation, RYW on and off, first one winning: RYW throws it
// in the call (RYW:2228-2241, 2301-2348, 2372, 2420) and libfdb_c defers it (ThreadSafeTransaction.cpp:589-640).
func TestIllegalWriteIsDeferred(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("v"), 100001)
	for name, tc := range map[string]struct {
		write func(tx *Transaction)
		code  int
	}{
		"set value":           {func(tx *Transaction) { tx.Set([]byte("k"), big) }, 2103},
		"append value":        {func(tx *Transaction) { tx.Atomic(MutAppendIfFits, []byte("k"), big) }, 2103},
		"set key size":        {func(tx *Transaction) { tx.Set(bytes.Repeat([]byte("k"), 10001), []byte("v")) }, 2102},
		"set system key":      {func(tx *Transaction) { tx.Set([]byte("\xff\x05"), []byte("v")) }, 2004},
		"clear system key":    {func(tx *Transaction) { tx.Clear([]byte("\xff\x05")) }, 2004},
		"clear range end":     {func(tx *Transaction) { _ = tx.ClearRange([]byte("a"), []byte("\xff\x01")) }, 2004},
		"clear range empty":   {func(tx *Transaction) { _ = tx.ClearRange([]byte("\xff\x01"), []byte("\xff\x01")) }, 2004},
		"clear range invert":  {func(tx *Transaction) { _ = tx.ClearRange([]byte("b"), []byte("a")) }, ErrInvertedRange},
		"first of two writes": {func(tx *Transaction) { tx.Clear([]byte("\xff\x05")); tx.Set([]byte("k"), big) }, 2004},
	} {
		for _, rywDisabled := range []bool{false, true} {
			tx := &Transaction{rywDisabled: rywDisabled}
			tx.Set([]byte("k"), []byte("v"))
			tc.write(tx)
			ctx := context.Background()
			_, getErr := tx.Get(ctx, []byte("k"))
			_, _, rangeErr := tx.GetRange(ctx, []byte("a"), []byte("z"), 0)
			commitErr := tx.Commit(ctx)
			for op, err := range map[string]error{"Get": getErr, "GetRange": rangeErr, "Commit": commitErr} {
				var fe *wire.FDBError
				if !errors.As(err, &fe) || fe.Code != tc.code {
					t.Errorf("%s, rywDisabled=%v: %s = %v, want %d", name, rywDisabled, op, err, tc.code)
				}
			}
		}
	}
}
