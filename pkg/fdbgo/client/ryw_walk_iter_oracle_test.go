package client

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// oracleIter is the walk iterator before it composed a write-map and a cache
// iterator: every step re-derives its segment from boundary candidates (the
// write map's node boundaries, the snapshot cache's entry bounds, keys and key
// successors, "" and \xff\xff) and classifies it with segTypeAtLocked. It is the
// equivalence oracle for rywIter, whose segments, types, values and contiguous
// runs must be identical.
type oracleIter struct {
	c          *rywCache
	w          *rywWrites
	begin, end []byte
	typ        rywSegType
	sentinel   bool
}

func (c *rywCache) oracleCandidatesLocked(p []byte, w *rywWrites, visit func(rywBoundary)) {
	hi := rywBoundary{key: allKeysEnd}
	add := func(b rywBoundary) {
		if b.compare(hi) <= 0 {
			visit(b)
		}
	}
	add(rywBoundary{key: allKeysBegin})
	add(hi)
	if w != nil {
		f, ce := w.nodes.floorCeil(p)
		add(f)
		add(ce)
		add(w.nodes.prevBoundary(p))
	}
	c.cacheCandidatesLocked(p, add)
}

func (it *oracleIter) skip(key []byte) {
	if bytes.Compare(key, allKeysEnd) >= 0 {
		it.begin, it.end, it.typ, it.sentinel = allKeysEnd, sentinelEnd, segEmpty, true
		return
	}
	it.sentinel = false
	lo, up := rywBoundary{key: allKeysBegin}, rywBoundary{key: allKeysEnd}
	probe := rywBoundary{key: key}
	it.c.oracleCandidatesLocked(key, it.w, func(b rywBoundary) {
		if b.compare(probe) <= 0 {
			if b.compare(lo) > 0 {
				lo = b
			}
		} else if b.compare(up) < 0 {
			up = b
		}
	})
	it.begin, it.end = lo.materialize(), up.materialize()
	it.typ = it.c.segTypeAtLocked(it.w, it.begin)
}

func (it *oracleIter) prev() {
	best, probe := rywBoundary{key: allKeysBegin}, rywBoundary{key: it.begin}
	it.c.oracleCandidatesLocked(it.begin, it.w, func(b rywBoundary) {
		if b.compare(probe) < 0 && b.compare(best) > 0 {
			best = b
		}
	})
	it.skip(best.materialize())
}

func (it *oracleIter) kv() (KeyValue, bool) {
	if it.typ != segKV {
		return KeyValue{}, false
	}
	return KeyValue{Key: it.begin, Value: it.c.segmentValueLocked(it.w, it.begin)}, true
}

// writeSegment is the write map's unmodified segment holding the current key,
// from the stores: no operation at it and not cleared.
func (it *oracleIter) writeSegment() (unmodified bool, begin, end []byte) {
	if it.w == nil {
		return true, nil, nil
	}
	if _, op := it.w.entry(string(it.begin)); op || it.w.isCleared(it.begin) {
		return false, nil, nil
	}
	f, c := it.w.nodes.floorCeil(it.begin)
	return true, f.materialize(), c.materialize()
}

func (it *oracleIter) cacheRun() ([]KeyValue, int, bool) {
	_, e := it.c.serverCache.entries.find(func(e *cacheEntry) bool { return bytes.Compare(e.end, it.begin) > 0 })
	if e == nil || bytes.Compare(e.begin, it.begin) > 0 {
		return nil, 0, false
	}
	j := sort.Search(len(e.kvs), func(j int) bool { return bytes.Compare(e.kvs[j].Key, it.begin) >= 0 })
	if j == len(e.kvs) || !bytes.Equal(e.kvs[j].Key, it.begin) {
		return nil, 0, false
	}
	return e.kvs, j, true
}

func (it *oracleIter) skipContiguous(bound []byte) []KeyValue {
	kv, _ := it.kv()
	unmodified, _, wEnd := it.writeSegment()
	if !unmodified {
		return []KeyValue{kv}
	}
	if wEnd != nil && bytes.Compare(wEnd, bound) < 0 {
		bound = wEnd
	}
	kvs, i, ok := it.cacheRun()
	if !ok {
		return []KeyValue{kv}
	}
	j := i + 1 + sort.Search(len(kvs)-i-1, func(n int) bool { return bytes.Compare(kvs[i+1+n].Key, bound) >= 0 })
	if j-1 > i {
		it.skip(kvs[j-1].Key)
	}
	return kvs[i:j]
}

func (it *oracleIter) skipContiguousBack(bound []byte) []KeyValue {
	kv, _ := it.kv()
	unmodified, wBegin, _ := it.writeSegment()
	if !unmodified {
		return []KeyValue{kv}
	}
	if wBegin != nil {
		if a := keyAfterBytes(wBegin); bytes.Compare(a, bound) > 0 {
			bound = a
		}
	}
	kvs, i, ok := it.cacheRun()
	if !ok {
		return []KeyValue{kv}
	}
	j := sort.Search(i, func(n int) bool { return bytes.Compare(kvs[n].Key, bound) >= 0 })
	if j < i {
		it.skip(kvs[j].Key)
	}
	return kvs[j : i+1]
}

func iterState(begin, end []byte, typ rywSegType, sentinel bool) string {
	return fmt.Sprintf("[%q,%q) type=%d sentinel=%t", begin, end, typ, sentinel)
}

func kvsString(kvs []KeyValue) string {
	var parts []string
	for _, kv := range kvs {
		parts = append(parts, fmt.Sprintf("%q=%q", kv.Key, kv.Value))
	}
	return strings.Join(parts, " ")
}

// RFC-262 D2: after random histories of every write-map mutator and random
// snapshot-cache fragments, rywIter yields the oracle's partition, types, values
// and contiguous runs, skipping to every probe and stepping through the whole
// keyspace in both directions, over the live version and the cache alone.
func TestRYWIterMatchesCandidateOracle(t *testing.T) {
	t.Parallel()
	keys := []string{"", "\x00", "a", "a\x00", "b", "b\x00", "b5", "c", "c\x00", "c\x00\x00", "d", "e", "\xff", "\xff\x00", "\xff\xfe"}
	var probes [][]byte
	for _, k := range keys {
		probes = append(probes, []byte(k), []byte(k+"\x00"))
	}
	probes = append(probes, allKeysEnd, sentinelEnd)
	rng := rand.New(rand.NewSource(2620))
	sameRng := rand.New(rand.NewSource(2621)) // leaves rng's histories as they were
	counts := map[string]int{}
	for h := 0; h < 1500; h++ {
		c := &rywCache{}
		var log []string
		key := func() []byte { return []byte(keys[rng.Intn(len(keys))]) }
		rangeOf := func() ([]byte, []byte) {
			a, b := key(), key()
			if bytes.Compare(a, b) > 0 {
				a, b = b, a
			}
			if bytes.Equal(a, b) {
				b = append(bytes.Clone(a), 0)
			}
			return a, b
		}
		for i, n := 0, rng.Intn(14); i < n; i++ {
			conflict := rng.Intn(3) != 0
			switch rng.Intn(10) {
			case 0, 1:
				k := key()
				c.setWith(k, []byte("v"+string(k)), conflict)
				log = append(log, fmt.Sprintf("set(%q)", k))
			case 2:
				k := key()
				c.clearWith(k, conflict)
				log = append(log, fmt.Sprintf("clear(%q)", k))
			case 3:
				b, e := rangeOf()
				c.clearRangeWith(b, e, conflict)
				log = append(log, fmt.Sprintf("clearRange(%q,%q)", b, e))
			case 4:
				k := key()
				c.atomicWith(MutAddValue, k, []byte{1}, conflict)
				log = append(log, fmt.Sprintf("add(%q)", k))
			case 5:
				k := key()
				c.atomicWith(MutSetVersionstampedValue, k, make([]byte, 14), conflict)
				log = append(log, fmt.Sprintf("svv(%q)", k))
			case 6:
				b, e := rangeOf()
				c.addUnreadableRange(b, e)
				c.atomicWith(MutSetVersionstampedKey, b, make([]byte, 4), false)
				log = append(log, fmt.Sprintf("svk(%q,%q)", b, e))
			case 7:
				b, e := rangeOf()
				c.addConflictRange(b, e)
				log = append(log, fmt.Sprintf("conflict(%q,%q)", b, e))
			case 8:
				k := key()
				c.atomicWith(MutCompareAndClear, k, []byte("v"+string(k)), conflict)
				log = append(log, fmt.Sprintf("cac(%q)", k))
			case 9:
				b, e := rangeOf()
				var kvs []KeyValue
				for _, k := range keys {
					if k >= string(b) && k < string(e) && rng.Intn(2) == 0 {
						kvs = append(kvs, KeyValue{Key: []byte(k), Value: []byte("s" + k)})
					}
				}
				c.mu.Lock()
				c.serverCache.insert(b, e, kvs)
				c.mu.Unlock()
				log = append(log, fmt.Sprintf("cache(%q,%q,%d)", b, e, len(kvs)))
			}
		}
		c.mu.Lock()
		for _, w := range []*rywWrites{nil, c.liveLocked()} {
			fail := func(format string, args ...any) {
				c.mu.Unlock()
				t.Fatalf("history %s writes=%t: %s\ncache %s", strings.Join(log, " "), w != nil, fmt.Sprintf(format, args...), c.cacheDump())
			}
			it := c.newIterLocked(w)
			or := &oracleIter{c: c, w: w}
			check := func(step string) {
				// The walk reads the type through these before kv classifies a pending
				// write: they must already agree with the oracle.
				if it.isKV() != (or.typ == segKV || or.typ == segPhantom) || it.unknown() != (or.typ == segUnknown) ||
					it.unreadable() != (or.typ == segUnreadable) || it.empty() != (or.typ == segEmpty) {
					fail("%s: iterator type %d (pending %t), oracle %d", step, it.typ, it.opPending, or.typ)
				}
				if it.opPending {
					counts["pending"]++
					if or.typ == segPhantom {
						counts["pendingPhantom"]++
					}
				}
				// kv first, as the walk calls it: it classifies a pending write itself.
				gkv, gok := it.kv()
				wkv, wok := or.kv()
				if gok != wok || !bytes.Equal(gkv.Key, wkv.Key) || !bytes.Equal(gkv.Value, wkv.Value) || (gkv.Value == nil) != (wkv.Value == nil) {
					fail("%s: kv %q=%q (%t), oracle %q=%q (%t)", step, gkv.Key, gkv.Value, gok, wkv.Key, wkv.Value, wok)
				}
				got := iterState(it.beginKey(), it.endKey(), it.segType(), it.sentinel)
				want := iterState(or.begin, or.end, or.typ, or.sentinel)
				if got != want {
					fail("%s: iterator %s, oracle %s", step, got, want)
				}
				counts[fmt.Sprintf("type%d", it.typ)]++
			}
			// sameAs checks it == o (C++ compares positions) against a key comparison:
			// iterators on one version are at the same segment iff they begin alike.
			sameAs := func(step string, a, o *rywIter) {
				want := bytes.Equal(a.beginKey(), o.beginKey()) && a.sentinel == o.sentinel
				if got := a.same(o); got != want || o.same(a) != want {
					fail("%s: same %t for segments at %q and %q", step, got, a.beginKey(), o.beginKey())
				}
				counts[fmt.Sprintf("same=%t", want)]++
			}
			other := c.newIterLocked(w)
			for _, p := range probes {
				it.skip(p)
				or.skip(p)
				check(fmt.Sprintf("skip(%q)", p))
				other.skip(probes[sameRng.Intn(len(probes))])
				sameAs(fmt.Sprintf("skip(%q)", p), it, other)
			}
			// A stepped iterator is at the position a skip to its segment lands on.
			stepped := func(step string) {
				fresh := c.newIterLocked(w)
				fresh.skip(it.beginKey())
				sameAs(step, it, fresh)
				sameAs(step, it, other)
			}
			// Forward from "" to the sentinel, then backward to "".
			it.skip(allKeysBegin)
			or.skip(allKeysBegin)
			for steps := 0; !or.sentinel; steps++ {
				if steps > 200 {
					fail("forward walk does not reach the sentinel")
				}
				it.next()
				or.skip(or.end)
				check("next")
				stepped("next")
			}
			for steps := 0; len(or.begin) > 0; steps++ {
				if steps > 200 {
					fail("backward walk does not reach the first segment")
				}
				it.prev()
				or.prev()
				check("prev")
				stepped("prev")
				counts["prev"]++
			}
			// Contiguous runs from every KV, toward every probe bound.
			for _, p := range probes {
				it.skip(p)
				or.skip(p)
				if or.typ != segKV { // it stays pending: skipContiguous's kv classifies it
					continue
				}
				bound := probes[rng.Intn(len(probes))]
				if bytes.Compare(bound, allKeysEnd) > 0 {
					bound = allKeysEnd
				}
				itB, orB := it.clone(), &oracleIter{c: c, w: w}
				orB.skip(p)
				got, want := it.skipContiguous(bound), or.skipContiguous(bound)
				if kvsString(got) != kvsString(want) {
					fail("skipContiguous(%q) from %q: %s, oracle %s", bound, p, kvsString(got), kvsString(want))
				}
				check(fmt.Sprintf("after skipContiguous(%q) from %q", bound, p))
				if len(got) > 1 {
					counts["contiguous"]++
				}
				gotB, wantB := itB.skipContiguousBack(bound), orB.skipContiguousBack(bound)
				if kvsString(gotB) != kvsString(wantB) {
					fail("skipContiguousBack(%q) from %q: %s, oracle %s", bound, p, kvsString(gotB), kvsString(wantB))
				}
				if s, o := iterState(itB.beginKey(), itB.endKey(), itB.segType(), itB.sentinel), iterState(orB.begin, orB.end, orB.typ, orB.sentinel); s != o {
					fail("after skipContiguousBack(%q) from %q: iterator %s, oracle %s", bound, p, s, o)
				}
				if len(gotB) > 1 {
					counts["contiguousBack"]++
				}
			}
		}
		c.mu.Unlock()
	}
	for _, k := range []string{"type0", "type1", "type2", "type3", "type4", "prev", "contiguous", "contiguousBack", "same=true", "same=false", "pending", "pendingPhantom"} {
		if counts[k] == 0 {
			t.Fatalf("vacuous oracle comparison: %s never seen; counts %v", k, counts)
		}
	}
	t.Logf("compared segments by type and steps: %v", counts)
}
