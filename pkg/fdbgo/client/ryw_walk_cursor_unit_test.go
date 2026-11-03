package client

import (
	"bytes"
	"fmt"
	"math/bits"
	"math/rand"
	"sort"
	"testing"
)

// gallopSearch is sort.Search over [lo, hi) from every hint, for every answer.
func TestGallopSearchMatchesSortSearch(t *testing.T) {
	t.Parallel()
	for lo := 0; lo < 4; lo++ {
		for hi := lo; hi < lo+40; hi++ {
			for answer := lo; answer <= hi; answer++ {
				for hint := lo - 2; hint <= hi+2; hint++ {
					calls := 0
					got := gallopSearch(lo, hi, hint, func(n int) bool {
						if n < lo || n >= hi {
							t.Fatalf("[%d,%d) hint %d: pred(%d) out of range", lo, hi, hint, n)
						}
						calls++
						return n >= answer
					})
					if got != answer {
						t.Fatalf("[%d,%d) hint %d answer %d: got %d", lo, hi, hint, answer, got)
					}
					// O(log distance): at most two calls per doubling, plus the bracket.
					if d := max(answer-hint, hint-answer); hint >= lo && hint <= hi && calls > 2*bits.Len(uint(d))+4 {
						t.Fatalf("[%d,%d) hint %d answer %d: %d calls", lo, hi, hint, answer, calls)
					}
				}
			}
		}
	}
}

// insertKVsReference is snapshotCache.insertKVs as it searched before: four
// searches from the root, the oracle for the two-skip insert.
func insertKVsReference(sc *snapshotCache, begin, end []byte, kvs []KeyValue) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	newBegin, newEnd, es := begin, end, &sc.entries
	if _, e := es.find(func(e *cacheEntry) bool { return bytes.Compare(e.end, newBegin) > 0 }); e != nil && bytes.Compare(e.begin, newBegin) < 0 {
		newBegin = e.end
	}
	if j := es.search(func(e *cacheEntry) bool { return bytes.Compare(e.begin, newEnd) >= 0 }); j > 0 && bytes.Compare(es.at(j-1).end, newEnd) > 0 {
		newEnd = es.at(j - 1).begin
	}
	if bytes.Compare(newBegin, newEnd) >= 0 {
		return
	}
	lo := es.search(func(e *cacheEntry) bool { return bytes.Compare(e.begin, newBegin) >= 0 })
	hi := es.search(func(e *cacheEntry) bool { return bytes.Compare(e.end, newEnd) > 0 })
	lok := sort.Search(len(kvs), func(k int) bool { return bytes.Compare(kvs[k].Key, newBegin) >= 0 })
	hik := sort.Search(len(kvs), func(k int) bool { return bytes.Compare(kvs[k].Key, newEnd) >= 0 })
	fresh := cacheEntry{begin: newBegin, end: newEnd, kvs: kvs[lok:hik:hik]}
	switch hi - lo {
	case 0:
		es.insert(lo, fresh)
	case 1:
		es.set(lo, fresh)
	default:
		es.delete(lo, hi)
		es.insert(lo, fresh)
	}
}

func cacheEntriesString(sc *snapshotCache) string { return entriesString(sc.entryList()) }

func entriesString(es []cacheEntry) string {
	var b bytes.Buffer
	for _, e := range es {
		fmt.Fprintf(&b, "[%q,%q)%d ", e.begin, e.end, len(e.kvs))
		for _, kv := range e.kvs {
			fmt.Fprintf(&b, "%q ", kv.Key)
		}
	}
	return b.String()
}

// The two-skip insert (SnapshotCache.h:345-382) leaves the entries the four
// searches did, and snapshots taken between inserts keep their entries.
func TestSnapshotCacheInsertMatchesReference(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(2625))
	keys := []string{"", "a", "a\x00", "b", "c", "c\x00", "d", "e", "f", "g", "h", "\xff"}
	cases := map[string]int{}
	for h := 0; h < 3000; h++ {
		var got, want snapshotCache
		type held struct {
			snap pseq[cacheEntry]
			want string
		}
		var snaps []held
		for i, n := 0, 1+rng.Intn(12); i < n; i++ {
			a, z := keys[rng.Intn(len(keys))], keys[rng.Intn(len(keys))]
			if a > z {
				a, z = z, a
			}
			if a == z {
				continue // insert returns at once
			}
			for _, e := range got.entryList() {
				b, en := string(e.begin), string(e.end)
				switch {
				case b < a && a < en && b < z && z < en:
					cases["absorbed"]++
				case b < a && a < en:
					cases["trimFront"]++
				case b < z && z < en:
					cases["trimBack"]++
				case a <= b && en <= z:
					cases["supersede"]++
				}
			}
			var kvs []KeyValue
			for _, k := range keys {
				if k >= a && k < z && rng.Intn(2) == 0 {
					kvs = append(kvs, KeyValue{Key: []byte(k), Value: []byte("v")})
				}
			}
			got.insertOwned([]byte(a), []byte(z), kvs)
			insertKVsReference(&want, []byte(a), []byte(z), kvs)
			if g, w := cacheEntriesString(&got), cacheEntriesString(&want); g != w {
				t.Fatalf("history %d step %d insert [%q,%q): got %s, want %s", h, i, a, z, g, w)
			}
			if got.cur.root != nil || len(got.cur.path) != 0 {
				t.Fatalf("insert left its cursor on the entries")
			}
			for _, s := range snaps {
				if g := entriesString(s.snap.appendTo(nil)); g != s.want {
					t.Fatalf("history %d step %d: a snapshot changed: %s, taken as %s", h, i, g, s.want)
				}
				cases["snapshotChecked"]++
			}
			if rng.Intn(3) == 0 {
				s := got.snapshot()
				snaps = append(snaps, held{s, entriesString(s.appendTo(nil))})
				want.snapshot()
			}
		}
	}
	for _, k := range []string{"trimFront", "trimBack", "supersede", "absorbed", "snapshotChecked"} {
		if cases[k] == 0 {
			t.Fatalf("vacuous: no insert hit %s; %v", k, cases)
		}
	}
	t.Logf("insert cases: %v", cases)
}

// removeOrEqualPair is removeOrEqual of each selector; successors of one key
// are one slice, of two keys disjoint slices.
func TestRemoveOrEqualPairMatchesRemoveOrEqual(t *testing.T) {
	t.Parallel()
	sels := []KeySelector{
		{Key: []byte("k"), OrEqual: true, Offset: 0},
		{Key: []byte("k"), OrEqual: true, Offset: 2},
		{Key: []byte("k"), Offset: 1},
		{Key: []byte("m"), OrEqual: true, Offset: 1},
		{Key: []byte{}, OrEqual: true, Offset: 0},
		{Key: allKeysEnd, OrEqual: true, Offset: 1},
	}
	for _, b := range sels {
		for _, e := range sels {
			gb, ge := removeOrEqualPair(b, e)
			wb, we := b.removeOrEqual(), e.removeOrEqual()
			if !bytes.Equal(gb.Key, wb.Key) || gb.OrEqual || gb.Offset != wb.Offset || !bytes.Equal(ge.Key, we.Key) || ge.OrEqual || ge.Offset != we.Offset {
				t.Fatalf("%v %v: got %v %v, want %v %v", b, e, gb, ge, wb, we)
			}
			// The begin key must not reach into the end key's bytes.
			if len(gb.Key) > 0 && len(ge.Key) > 0 && cap(gb.Key) > len(gb.Key) && &gb.Key[:cap(gb.Key)][len(gb.Key)] == &ge.Key[0] {
				t.Fatalf("%v %v: begin's capacity overlaps end's key", b, e)
			}
		}
	}
}

// A storage reply's rows go into the cache uncopied, and a warm cached walk
// reuses its pooled iterator storage: it allocates only its result.
// Not parallel: AllocsPerRun counts the process's allocations.
func TestRYWWalkKeepsReplyRowsAndPoolsItsStorage(t *testing.T) {
	c := &rywCache{}
	m := newMemStorage("a", "b", "c", "d")
	var reply []KeyValue
	m.reply = func(_ int, req walkRequest) (nativeRangeResult, error) {
		r := m.answer(req, false)
		reply = r.kvs
		return r, nil
	}
	limits, maxKey := newRangeLimits(0, 0), []byte("\xff")
	begin, end := firstGreaterOrEqual([]byte("a")), firstGreaterOrEqual([]byte("z"))
	w, err := runWalk(c, nil, begin, end, limits, false, maxKey, m)
	if err != nil || keysOf(w.rangeResult().kvs) != "a,b,c,d" {
		t.Fatalf("cold walk: %v, %v", w.rangeResult().kvs, err)
	}
	w.release()
	es := c.serverCache.entryList()
	if len(es) != 1 || len(es[0].kvs) != 4 || &es[0].kvs[0] != &reply[0] || &es[0].kvs[3].Value[0] != &m.kvs[3].Value[0] {
		t.Fatalf("the cache holds copies of the reply's rows: %s", c.cacheDump())
	}
	end = firstGreaterOrEqual([]byte("c"))
	allocs := testing.AllocsPerRun(200, func() {
		c.mu.Lock()
		w := c.newRangeWalkLocked(nil, begin, end, limits, false, maxKey)
		c.mu.Unlock()
		w.start()
		if !w.done() || len(w.rangeResult().kvs) != 2 {
			panic("the cached walk did not finish at issue")
		}
		w.release()
	})
	if allocs > 1 {
		t.Fatalf("a warm cached walk allocates %v times, want 1 (its result)", allocs)
	}
	if len(m.requests) != 1 {
		t.Fatalf("%d storage requests, want 1", len(m.requests))
	}
}
