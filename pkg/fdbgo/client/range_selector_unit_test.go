package client

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"weak"
)

// Selector range reads resolve their selectors inside the range read (RFC-262
// D10): the RYW walk resolves them against the write map and snapshot cache and
// sends what is left to storage in the range request itself
// (ReadYourWrites.actor.cpp:592-813, 896-1123). These tests drive
// GetRangeSelectors over memStorage and check the rows against a model of the
// transaction's view, and the storage requests a cold read sends.

// selectorModel is the transaction's view: storage with the pending writes applied.
type selectorModel map[string]string

func (m selectorModel) keys() []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// position resolves a selector over keys (FDBTypes.h:631-700): the
// offset-th key at or after key, counting from 1, with orEqual moving key past itself.
func selectorPosition(keys []string, sel KeySelector) int {
	sel = sel.removeOrEqual()
	base := sort.SearchStrings(keys, string(sel.Key))
	return max(0, min(base+int(sel.Offset)-1, len(keys)))
}

// read is getRange over the view: [position(begin), position(end)), the first
// limit rows in scan order.
func (m selectorModel) read(begin, end KeySelector, limit int, reverse bool) []string {
	keys := m.keys()
	b, e := selectorPosition(keys, begin), selectorPosition(keys, end)
	if b >= e {
		return nil
	}
	rows := slices.Clone(keys[b:e])
	if reverse {
		slices.Reverse(rows)
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

type selectorOp struct {
	kind       string // set, clear, clearRange
	key, end   string
	applyModel func(selectorModel)
}

func selectorOps() map[string][]selectorOp {
	set := func(k string) selectorOp {
		return selectorOp{kind: "set", key: k, applyModel: func(m selectorModel) { m[k] = "w" + k }}
	}
	clear := func(k string) selectorOp {
		return selectorOp{kind: "clear", key: k, applyModel: func(m selectorModel) { delete(m, k) }}
	}
	clearRange := func(b, e string) selectorOp {
		return selectorOp{kind: "clearRange", key: b, end: e, applyModel: func(m selectorModel) {
			for k := range m {
				if k >= b && k < e {
					delete(m, k)
				}
			}
		}}
	}
	return map[string][]selectorOp{
		"none":            nil,
		"set-between":     {set("e")},
		"set-on-key":      {set("f")},
		"clear-key":       {clear("f")},
		"clear-range":     {clearRange("g", "k")},
		"clear-then-set":  {clearRange("c", "m"), set("h")},
		"sets-around":     {set("c"), set("i"), clear("d"), set("d0")},
		"clear-all-data":  {clearRange("b", "o")},
		"set-below-data":  {set("a3"), set("b0")},
		"clear-many-keys": {clear("b"), clear("d"), clear("h"), clear("j")},
	}
}

var selectorStorageKeys = []string{"a0", "a1", "a2", "b", "d", "f", "h", "j", "l", "n", "y0", "y1", "y2"}

func selectorCandidates() []KeySelector {
	var out []KeySelector
	for _, anchor := range []string{"b", "c", "d", "e", "f", "g", "h", "k", "n"} {
		for _, form := range []struct {
			orEqual bool
			offset  int32
		}{{false, 1}, {true, 1}, {true, 0}, {false, 0}, {true, 2}, {false, 2}, {false, -1}, {true, -1}, {false, 3}} {
			out = append(out, KeySelector{Key: []byte(anchor), OrEqual: form.orEqual, Offset: form.offset})
		}
	}
	return out
}

// TestSelectorRangeMatchesView checks GetRangeSelectors' rows against the
// model for random selector pairs, limits, directions, pending writes and
// snapshot isolation. The anchors and offsets keep every resolution inside the
// guard keys, so memStorage's lack of readToBegin/readThroughEnd flags at the
// database edges does not matter.
func TestSelectorRangeMatchesView(t *testing.T) {
	t.Parallel()
	cands := selectorCandidates()
	rng := rand.New(rand.NewSource(262))
	ops := selectorOps()
	names := make([]string, 0, len(ops))
	for name := range ops {
		names = append(names, name)
	}
	sort.Strings(names)
	checked, nonEmpty := 0, 0
	for i := 0; i < 4000; i++ {
		begin, end := cands[rng.Intn(len(cands))], cands[rng.Intn(len(cands))]
		limit := []int{0, 1, 2, 3, 5}[rng.Intn(5)]
		reverse, snapshot := rng.Intn(2) == 0, rng.Intn(2) == 0
		opsName := names[rng.Intn(len(names))]
		warm := rng.Intn(3) == 0 // populate the snapshot cache first
		name := fmt.Sprintf("%d:[%s,%s)/limit=%d/reverse=%t/snapshot=%t/ops=%s/warm=%t", i, selString(begin), selString(end), limit, reverse, snapshot, opsName, warm)

		m := newMemStorage(selectorStorageKeys...)
		tx := &Transaction{}
		tx.nativeRange = m.native
		model := selectorModel{}
		for _, k := range selectorStorageKeys {
			model[k] = "v" + k
		}
		for _, op := range ops[opsName] {
			switch op.kind {
			case "set":
				tx.Set([]byte(op.key), []byte("w"+op.key))
			case "clear":
				tx.Clear([]byte(op.key))
			case "clearRange":
				if err := tx.ClearRange([]byte(op.key), []byte(op.end)); err != nil {
					t.Fatal(err)
				}
			}
			op.applyModel(model)
		}
		if warm {
			if _, _, err := tx.Snapshot().GetRangeSelectors(context.Background(), firstGreaterOrEqual([]byte("c")), firstGreaterOrEqual([]byte("i")), 0, 0, false); err != nil {
				t.Fatalf("%s: warm read: %v", name, err)
			}
		}
		read := tx.GetRangeSelectors
		if snapshot {
			read = tx.Snapshot().GetRangeSelectors
		}
		kvs, more, err := read(context.Background(), begin, end, limit, 0, reverse)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := model.read(begin, end, 0, reverse)
		full := len(want)
		if limit > 0 && len(want) > limit {
			want = want[:limit]
		}
		var got []string
		for _, kv := range kvs {
			got = append(got, string(kv.Key))
			if wantV := model[string(kv.Key)]; string(kv.Value) != wantV {
				t.Fatalf("%s: value of %q = %q, want %q", name, kv.Key, kv.Value, wantV)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s: rows %v, want %v (requests %d)", name, got, want, len(m.requests))
		}
		if full > len(want) && !more {
			t.Fatalf("%s: %d of %d rows without more", name, len(want), full)
		}
		checked++
		if len(want) > 0 {
			nonEmpty++
		}
	}
	if checked != 4000 || nonEmpty < 1000 {
		t.Fatalf("checked %d reads, %d non-empty; the population is too thin", checked, nonEmpty)
	}
}

// A cold selector range read is one storage request carrying the selectors,
// not a getKey per selector and then a range: the shape BunchedMap issues for
// every put, lastLessOrEqual(k) to firstGreaterThan(k)+1 (C++ sends it as one
// getRange, ReadYourWrites.actor.cpp:705-759).
func TestSelectorRangeColdReadIsOneRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		begin, end  KeySelector
		reverse     bool
		wantRows    string
		wantRequest string
	}{
		{
			"bunched-put",
			KeySelector{Key: []byte("e"), OrEqual: true, Offset: 0},
			KeySelector{Key: []byte("e"), OrEqual: true, Offset: 2},
			false, "d,f",
			`[("e\x00",false,0) ("e\x00",false,2)) rows=-1 minRows=2 bytes=-1 wire=80000/80000`,
		},
		{
			"bunched-put-on-key",
			KeySelector{Key: []byte("f"), OrEqual: true, Offset: 0},
			KeySelector{Key: []byte("f"), OrEqual: true, Offset: 2},
			false, "f,h",
			`[("f\x00",false,0) ("f\x00",false,2)) rows=-1 minRows=2 bytes=-1 wire=80000/80000`,
		},
		{
			"lastLessThan-to-firstGreaterThan",
			KeySelector{Key: []byte("f"), Offset: 0},
			KeySelector{Key: []byte("j"), OrEqual: true, Offset: 1},
			false, "d,f,h,j",
			`[("f",false,0) ("j\x00",false,1)) rows=-1 minRows=2 bytes=-1 wire=80000/80000`,
		},
		// U3'/U5' (:1011-1045): both offsets become additionalRows, minRows 1+2.
		{
			"reverse-offsets",
			KeySelector{Key: []byte("d"), Offset: 2},
			KeySelector{Key: []byte("j"), OrEqual: true, Offset: 0},
			true, "h,f",
			`[("d",false,1) ("j\x00",false,1)) rows=-1 minRows=3 bytes=-1 wire=-80000/80000`,
		},
	}
	for _, tc := range cases {
		for _, snapshot := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/snapshot=%t", tc.name, snapshot), func(t *testing.T) {
				t.Parallel()
				m := newMemStorage(selectorStorageKeys...)
				tx := &Transaction{}
				tx.nativeRange = m.native
				read := tx.GetRangeSelectors
				if snapshot {
					read = tx.Snapshot().GetRangeSelectors
				}
				kvs, _, err := read(context.Background(), tc.begin, tc.end, 0, 0, tc.reverse)
				if err != nil {
					t.Fatal(err)
				}
				if got := keysOf(kvs); got != tc.wantRows {
					t.Fatalf("rows %s, want %s", got, tc.wantRows)
				}
				if len(m.requests) != 1 {
					var reqs []string
					for _, r := range m.requests {
						reqs = append(reqs, reqString(r, tc.reverse))
					}
					t.Fatalf("%d storage requests, want 1: %v", len(m.requests), reqs)
				}
				if got := reqString(m.requests[0], tc.reverse); got != tc.wantRequest {
					t.Fatalf("request %s, want %s", got, tc.wantRequest)
				}
			})
		}
	}
}

// The read conflict of a selector range read is addConflictRange(GetRangeReq)
// (ReadYourWrites.actor.cpp:245-319), filtered through the write map. Derived
// by hand from the C++ for storage b,d,f,h,j,l,n:
//
//   - forward lastLessOrEqual(e) .. firstGreaterThan(e)+1 is
//     [("e\x00",0), ("e\x00",2)) after removeOrEqual. The keys are equal, so the
//     span starts as [e\x00, e\x00); begin.offset <= 0 lowers it to the first
//     row d, and the end rises to keyAfter(f), the last row: [d, f\x00).
//   - reverse firstGreaterOrEqual(d) .. lastLessOrEqual(j), limit 2, returns
//     h,f with more. begin (d,1) < end (j\x00,0); more is set and
//     begin.offset <= 1, so the span begins at end.key: [j\x00, j\x00), lowered
//     to the last (smallest) row f; end.offset is 0, so the end is not raised:
//     [f, j\x00).
//   - the same read with begin firstGreaterOrEqual(d)+1, (d,2): begin.offset > 1
//     keeps the span at begin.key, and the rows do not lower it: [d, j\x00).
//   - with set(f) pending, the forward read's [d, f\x00) loses f, whose
//     segment is an independent write (updateConflictMap, :335-351).
func TestSelectorRangeConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		begin, end KeySelector
		limit      int
		reverse    bool
		set        string
		want       [][2]string
	}{
		{
			"forward",
			KeySelector{Key: []byte("e"), OrEqual: true, Offset: 0},
			KeySelector{Key: []byte("e"), OrEqual: true, Offset: 2},
			0, false, "",
			[][2]string{{"d", "f\x00"}},
		},
		{
			"reverse-more",
			KeySelector{Key: []byte("d"), Offset: 1},
			KeySelector{Key: []byte("j"), OrEqual: true, Offset: 0},
			2, true, "",
			[][2]string{{"f", "j\x00"}},
		},
		{
			"reverse-more-begin-offset",
			KeySelector{Key: []byte("d"), Offset: 2},
			KeySelector{Key: []byte("j"), OrEqual: true, Offset: 0},
			2, true, "",
			[][2]string{{"d", "j\x00"}},
		},
		{
			"forward-own-write",
			KeySelector{Key: []byte("e"), OrEqual: true, Offset: 0},
			KeySelector{Key: []byte("e"), OrEqual: true, Offset: 2},
			0, false, "f",
			[][2]string{{"d", "f"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMemStorage(selectorStorageKeys...)
			tx := &Transaction{}
			tx.nativeRange = m.native
			if tc.set != "" {
				tx.Set([]byte(tc.set), []byte("w"))
			}
			if _, _, err := tx.GetRangeSelectors(context.Background(), tc.begin, tc.end, tc.limit, 0, tc.reverse); err != nil {
				t.Fatal(err)
			}
			var got [][2]string
			for _, r := range tx.readConflictRanges() {
				got = append(got, [2]string{string(r.Begin), string(r.End)})
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("read conflicts %q, want %q", got, tc.want)
			}
			// A snapshot read of the same range adds none (RYW:387).
			snap := &Transaction{}
			snap.nativeRange = newMemStorage(selectorStorageKeys...).native
			if _, _, err := snap.Snapshot().GetRangeSelectors(context.Background(), tc.begin, tc.end, tc.limit, 0, tc.reverse); err != nil {
				t.Fatal(err)
			}
			if reads := snap.readConflictRanges(); len(reads) != 0 {
				t.Fatalf("snapshot read added conflicts %v", reads)
			}
		})
	}
}

func TestSelectorRangeChecks(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	m := newMemStorage(selectorStorageKeys...)
	tx.nativeRange = m.native
	ctx := context.Background()
	fge := func(k string) KeySelector { return firstGreaterOrEqual([]byte(k)) }
	for _, tc := range []struct {
		name       string
		begin, end KeySelector
		limit      int
		want       int
	}{
		{"begin-past-max", fge("\xff\x01"), fge("a"), 0, 2004},
		{"end-past-max", fge("a"), KeySelector{Key: []byte("\xff\x01"), Offset: 0}, -2, 2004},
		{"limit", fge("a"), KeySelector{Key: []byte("z"), Offset: 0}, -2, ErrRangeLimitsInvalid},
		{"inverted", KeySelector{Key: []byte("n"), Offset: 3}, KeySelector{Key: []byte("b"), Offset: 1}, 0, 0},
	} {
		kvs, more, err := tx.GetRangeSelectors(ctx, tc.begin, tc.end, tc.limit, 0, false)
		code, _ := fdbErrorCode(err)
		if code != tc.want || len(kvs) != 0 || more {
			t.Errorf("%s: rows %d more %t err %v, want code %d and no rows", tc.name, len(kvs), more, err, tc.want)
		}
	}
	if reads := tx.readConflictRanges(); len(m.requests) != 0 || len(reads) != 0 {
		t.Fatalf("rejected reads sent %d requests and added %d conflicts", len(m.requests), len(reads))
	}
}

// A selector read's rows are the caller's: writing them changes no cache, write
// map, reply or other read's rows, on every read route and for a second Get.
func TestSelectorRangeRowsAreTheCallers(t *testing.T) {
	t.Parallel()
	m := newMemStorage(selectorStorageKeys...)
	tx := &Transaction{}
	tx.nativeRange = m.native
	tx.Set([]byte("e"), []byte("ve-written"))
	ctx := context.Background()
	begin := KeySelector{Key: []byte("e"), OrEqual: true, Offset: 0} // lastLessOrEqual(e)
	end := KeySelector{Key: []byte("e"), OrEqual: true, Offset: 2}   // firstGreaterThan(e)+1
	want := "e=ve-written f=vf"                                      // the written e, then storage's f
	render := func(kvs []KeyValue) string {
		var parts []string
		for _, kv := range kvs {
			parts = append(parts, string(kv.Key)+"="+string(kv.Value))
		}
		return strings.Join(parts, " ")
	}
	scribble := func(kvs []KeyValue) {
		for i := range kvs {
			for j := range kvs[i].Key {
				kvs[i].Key[j] = '#'
			}
			for j := range kvs[i].Value {
				kvs[i].Value[j] = '#'
			}
			kvs[i] = KeyValue{Key: []byte("x"), Value: []byte("x")}
		}
	}
	reads := []func() ([]KeyValue, bool, error){
		func() ([]KeyValue, bool, error) { return tx.Snapshot().GetRangeSelectors(ctx, begin, end, 0, 0, false) }, // storage
		func() ([]KeyValue, bool, error) { return tx.Snapshot().GetRangeSelectors(ctx, begin, end, 0, 0, false) }, // cache
		func() ([]KeyValue, bool, error) { return tx.GetRangeSelectors(ctx, begin, end, 0, 0, false) },
		func() ([]KeyValue, bool, error) {
			return tx.Snapshot().StartGetRangeSelectors(ctx, begin, end, 0, 0, false).Get()
		},
	}
	var held [][]KeyValue
	for i, read := range reads {
		kvs, _, err := read()
		if err != nil || render(kvs) != want {
			t.Fatalf("read %d: rows %s, err %v; want %s", i, render(kvs), err, want)
		}
		held = append(held, kvs)
	}
	p := tx.Snapshot().StartGetRangeSelectors(ctx, begin, end, 0, 0, false)
	first, _, err1 := p.Get()
	second, _, err2 := p.Get()
	if err1 != nil || err2 != nil || render(first) != want || render(second) != want {
		t.Fatalf("Get twice: %s (%v), %s (%v)", render(first), err1, render(second), err2)
	}
	held = append(held, first)
	for _, kvs := range held {
		scribble(kvs)
	}
	if render(second) != want {
		t.Fatalf("a second Get shares the first's rows: %s", render(second))
	}
	if kvs, _, err := tx.Snapshot().GetRangeSelectors(ctx, begin, end, 0, 0, false); err != nil || render(kvs) != want {
		t.Fatalf("after writing returned rows: %s, err %v; want %s", render(kvs), err, want)
	}
	if got := render(m.kvs[4:6]); got != "d=vd f=vf" {
		t.Fatalf("storage rows written through a result: %s", got)
	}
	if len(m.requests) != 1 {
		t.Fatalf("%d storage requests, want 1 (the rest from the cache)", len(m.requests))
	}
}

// A started getKey's key is the caller's, each Get its own copy, whether it
// resolved to a stored key, a written key or one storage returned.
func TestStartedGetKeyIsTheCallers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, snapshot := range []bool{false, true} {
		m := newMemStorage(selectorStorageKeys...)
		tx := &Transaction{}
		tx.nativeRange = m.native
		tx.Set([]byte("e"), []byte("ve"))
		for _, tc := range []struct {
			sel  KeySelector
			want string
		}{
			{KeySelector{Key: []byte("e"), OrEqual: true, Offset: 0}, "e"}, // the written key
			{KeySelector{Key: []byte("e"), OrEqual: true, Offset: 1}, "f"}, // from storage
			{KeySelector{Key: []byte("e"), OrEqual: true, Offset: 1}, "f"}, // from the cache
			{KeySelector{Key: []byte("e"), Offset: 0}, "d"},
		} {
			var p *PendingKey
			if snapshot {
				p = tx.Snapshot().StartGetKey(ctx, tc.sel)
			} else {
				p = tx.StartGetKey(ctx, tc.sel)
			}
			first, err1 := p.Get()
			second, err2 := p.Get()
			if err1 != nil || err2 != nil || string(first) != tc.want || string(second) != tc.want {
				t.Fatalf("snapshot=%t %v: Get %q (%v), %q (%v); want %q", snapshot, tc.sel, first, err1, second, err2, tc.want)
			}
			for i := range first {
				first[i] = '#'
			}
			if string(second) != tc.want {
				t.Fatalf("snapshot=%t %v: two Gets share a key", snapshot, tc.sel)
			}
		}
		if len(m.requests) == 0 {
			t.Fatalf("snapshot=%t: no getKey reached storage", snapshot)
		}
		for _, tc := range []struct{ sel, want string }{{"e", "e"}, {"f", "f"}, {"d", "d"}} {
			if k, err := tx.GetKey(ctx, []byte(tc.sel), true, 0); err != nil || string(k) != tc.want {
				t.Fatalf("snapshot=%t: after writing returned keys: GetKey(%q) = %q, %v", snapshot, tc.sel, k, err)
			}
		}
		if got := keysOf(m.kvs); !strings.Contains(got, "d,f") {
			t.Fatalf("snapshot=%t: storage keys written through a result: %s", snapshot, got)
		}
	}
}

// Building the three sentinel nodes must preserve a full-keyspace conflict
// (WriteMap.cpp:245-294), even though no extra node boundary is needed.
func TestSelectorReadKeepsFullKeyspaceWriteConflict(t *testing.T) {
	t.Parallel()
	tx := &Transaction{writeSystemKeys: true, readSystemKeys: true}
	tx.nativeRange = newMemStorage().native
	if err := tx.AddWriteConflictRange(allKeysBegin, allKeysEnd); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tx.Snapshot().GetRangeSelectors(context.Background(), firstGreaterOrEqual([]byte("a")), firstGreaterOrEqual([]byte("b")), 0, 0, false); err != nil {
		t.Fatal(err)
	}
	tx.ryw.mu.Lock()
	nodes := tx.ryw.liveLocked().nodes.seq.Len()
	empty := tx.ryw.writesEmptyLocked()
	tx.ryw.mu.Unlock()
	if nodes != 3 || empty {
		t.Fatalf("built map: %d nodes, empty=%t; want three flagged sentinels", nodes, empty)
	}
	tx.conflictMu.Lock()
	s := tx.snapshotConflictsLocked()
	tx.conflictMu.Unlock()
	defer s.release(tx)
	ranges, _ := s.writeConflicts()
	if len(ranges) != 1 || !bytes.Equal(ranges[0].Begin, allKeysBegin) || !bytes.Equal(ranges[0].End, allKeysEnd) {
		t.Fatalf("commit write conflicts = %q, want the full keyspace", ranges)
	}
}

// A completed walk no longer roots old cache storage after reset, while an
// outstanding snapshot owns its old allocation independently of the cache.
func TestResetDetachesCompletedWalkCacheStorage(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	m := newMemStorage("a", "b", "c")
	w, err := runWalk(c, nil, firstGreaterOrEqual([]byte("a")), firstGreaterOrEqual([]byte("z")), newRangeLimits(0, 0), false, allKeysEnd, m)
	if err != nil || len(w.rangeResult().kvs) != 3 {
		t.Fatalf("walk: %v, %v", w.rangeResult().kvs, err)
	}
	w.release()
	if c.serverCache.snapshotCacheStorage == nil || c.serverCache.node0[0].item.kvs == nil {
		t.Fatal("positive control: the completed walk did not populate small-cache storage")
	}
	retired := weak.Make(c.serverCache.snapshotCacheStorage)
	c.reset()
	if c.serverCache.snapshotCacheStorage != nil || c.serverCache.entries.root != nil {
		t.Fatal("reset retained the completed walk's cache allocation")
	}
	for i := 0; i < 20 && retired.Value() != nil; i++ {
		runtime.GC()
	}
	if retired.Value() != nil {
		t.Fatal("a completed walk's storage is still strongly reachable after reset")
	}
	c.serverCache.insertPoint([]byte("a"), []byte("va"))
	old := c.serverCache.snapshot()
	c.reset()
	c.serverCache.insertPoint([]byte("x"), []byte("vx"))
	if es := old.appendTo(nil); len(es) != 1 || len(es[0].kvs) != 1 || string(es[0].kvs[0].Key) != "a" || string(es[0].kvs[0].Value) != "va" {
		t.Fatalf("a held snapshot changed after fresh cache allocation: %q", es)
	}
	c.reset()
	if c.serverCache.snapshotCacheStorage != nil {
		t.Fatal("a later reset retained its cache allocation")
	}
	runtime.KeepAlive(c)
}

// Reads finished at issue from small-cache entries keep their bytes across Reset,
// even when new point reads allocate fresh cache storage.
func TestLocalReadsKeepInlineCacheBytesAcrossReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tx := &Transaction{}
	tx.nativeRange = newMemStorage().native
	c := &tx.ryw
	c.mu.Lock()
	c.serverCache.insertPoint([]byte("a"), []byte("va"))
	c.serverCache.insert([]byte("a\x00"), []byte("b"), nil)
	c.serverCache.insertPoint([]byte("b"), []byte("vb"))
	inline := c.serverCache.kvUsed == 2 && c.serverCache.keyBufUsed > 0 && c.serverCache.node0[0].item.begin != nil
	c.mu.Unlock()
	if !inline {
		t.Fatal("positive control: the points are not in the inline storage")
	}
	pr := tx.Snapshot().StartGetRangeSelectors(ctx, firstGreaterOrEqual([]byte("a")), firstGreaterOrEqual([]byte("b\x00")), 0, 0, false)
	pk := tx.Snapshot().StartGetKey(ctx, KeySelector{Key: []byte("a"), OrEqual: true, Offset: 1})
	tx.Reset()
	c.mu.Lock()
	c.serverCache.insertPoint([]byte("x"), []byte("vx"))
	c.serverCache.insertPoint([]byte("y"), []byte("vy"))
	c.mu.Unlock()
	if kvs, _, err := pr.Get(); err != nil || len(kvs) != 2 || string(kvs[0].Key) != "a" || string(kvs[0].Value) != "va" || string(kvs[1].Key) != "b" || string(kvs[1].Value) != "vb" {
		t.Fatalf("range read after Reset: %q, %v; want a=va b=vb", kvs, err)
	}
	if k, err := pk.Get(); err != nil || string(k) != "b" {
		t.Fatalf("getKey after Reset: %q, %v; want b", k, err)
	}
}

// GetKey copies its selector: a caller rewriting its key buffer afterwards must
// not move the bound of the cache entry the read's reply made known.
func TestGetKeyOwnsItsSelectorKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		orEqual         bool
		offset          int32
		resolved        string
		rewrite         byte
		begin, end, got string
	}{
		{"forward", false, 1, "n", 'a', "a1", "c", "a1,a2,b"}, // FGE(m): entry [m, n\x00)
		{"reverse", false, 0, "l", 'z', "n", "y1", "n,y0"},    // LLT(m): entry [l, m)
	} {
		for _, snapshot := range []bool{false, true} {
			m := newMemStorage(selectorStorageKeys...)
			tx := &Transaction{}
			tx.nativeRange = m.native
			buf := []byte("m")
			getKey := tx.GetKey
			if snapshot {
				getKey = tx.Snapshot().GetKey
			}
			if k, err := getKey(ctx, buf, tc.orEqual, tc.offset); err != nil || string(k) != tc.resolved {
				t.Fatalf("%s snapshot=%t: GetKey = %q, %v; want %q", tc.name, snapshot, k, err, tc.resolved)
			}
			buf[0] = tc.rewrite
			kvs, _, err := tx.Snapshot().GetRangeSelectors(ctx, firstGreaterOrEqual([]byte(tc.begin)), firstGreaterOrEqual([]byte(tc.end)), 0, 0, false)
			if err != nil || keysOf(kvs) != tc.got {
				t.Fatalf("%s snapshot=%t: after the caller rewrote its key, rows %s, %v; want %s", tc.name, snapshot, keysOf(kvs), err, tc.got)
			}
		}
	}
}

// The facade passes a selector's key to the read, which must own it.
func TestSelectorRangeOwnsSelectorKeys(t *testing.T) {
	t.Parallel()
	m := newMemStorage(selectorStorageKeys...)
	tx := &Transaction{}
	tx.nativeRange = m.native
	// lastLessThan(e) .. firstGreaterOrEqual(e)+1: no orEqual, so removeOrEqual
	// would keep the caller's slices.
	bk, ek := []byte("e"), []byte("e")
	p := tx.StartGetRangeSelectors(context.Background(), KeySelector{Key: bk}, KeySelector{Key: ek, Offset: 2}, 0, 0, false)
	bk[0], ek[0] = 'z', 'z'
	kvs, _, err := p.Get()
	if err != nil || keysOf(kvs) != "d,f" {
		t.Fatalf("rows %s, err %v; want d,f", keysOf(kvs), err)
	}
	if len(m.requests) != 1 || !bytes.Equal(m.requests[0].begin.Key, []byte("e")) {
		t.Fatalf("requests %v, want one beginning at the key as issued", m.requests)
	}
}
