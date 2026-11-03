package client

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
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
