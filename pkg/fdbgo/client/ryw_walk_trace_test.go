package client

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// RFC-262 S1: the walk's request trace, derived by hand from the cited C++ lines.
// Keys are unprefixed; maxKey is \xff; storage replies come from memStorage, an
// in-memory NativeAPI getRange over the case's keys. Every request is asserted
// as [read_begin read_end) with requestLimit (rows, minRows, bytes) and the wire
// (limit, limitBytes) transformRangeLimits sends.

func lim(rows, minRows, bytes int32) rangeLimits {
	return rangeLimits{rows: rows, minRows: minRows, bytes: bytes}
}

func sel(key string, orEqual bool, offset int32) KeySelector {
	return KeySelector{Key: []byte(key), OrEqual: orEqual, Offset: offset}
}

func fgeS(key string) KeySelector { return sel(key, false, 1) }

// conflictRanges i=lo..hi-1 of [c<i>, c<i>\0), the T6 shape.
func addPointConflicts(c *rywCache, lo, hi int) {
	for i := lo; i < hi; i++ {
		k := fmt.Sprintf("c%02d", i)
		c.addConflictRange([]byte(k), []byte(k+"\x00"))
	}
}

type walkTrace struct {
	name       string
	storage    []string
	writes     func(c *rywCache)
	begin, end KeySelector
	limits     rangeLimits
	reverse    bool
	reqs       []string // reqString of each request, in order
	rows       string
	more       bool
	flags      [2]bool // readToBegin, readThroughEnd
	err        int     // FDB error code, 0 for none
	cache      string  // the snapshot cache after the walk, when set
}

func (tc walkTrace) run(t *testing.T) {
	t.Helper()
	c := &rywCache{}
	if tc.writes != nil {
		tc.writes(c)
	}
	m := newMemStorage(tc.storage...)
	w, err := runWalk(c, c.liveLocked(), tc.begin, tc.end, tc.limits, tc.reverse, []byte("\xff"), m)
	var got []string
	for _, r := range m.requests {
		got = append(got, reqString(r, tc.reverse))
	}
	if strings.Join(got, "\n") != strings.Join(tc.reqs, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.reqs, "\n"))
	}
	if tc.err != 0 {
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != tc.err {
			t.Fatalf("err %v, want %d", err, tc.err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	r := w.rangeResult()
	if keysOf(r.kvs) != tc.rows || r.more != tc.more || [2]bool{r.readToBegin, r.readThroughEnd} != tc.flags {
		t.Fatalf("rows %q more %t flags %v, want %q more %t flags %v", keysOf(r.kvs), r.more,
			[2]bool{r.readToBegin, r.readThroughEnd}, tc.rows, tc.more, tc.flags)
	}
	if tc.cache != "" && c.cacheDump() != tc.cache {
		t.Fatalf("cache %s\nwant  %s", c.cacheDump(), tc.cache)
	}
}

var abcde = []string{"a", "b", "c", "d", "e"}

func TestRYWWalkTraces(t *testing.T) {
	t.Parallel()
	for _, tc := range []walkTrace{
		{
			// T1. Request 1: skipUncached crosses ["",b) U, [b,b\0) E (1), [b\0,c) U,
			// [c,c\0) E (2), [c\0,c1) U, [c1,c1\0) KV to itEnd [c1\0,\xff\xff); clearLimit 0
			// counts none (RYW:701-703). Reply a,b more → cache [a,b\0) (A8). K emits a
			// (skipContiguous stops at the write node b), passes cleared b, stops at
			// [b\0,c) U. Request 2: clearLimit 2, crossed [c,c\0) = 1 (U4), begin moved to
			// FGE(b\0) (U5), setRequestLimits(rows 1, +1, offset 1) → rows 2, minRows 2.
			// Reply c,d → begin resolves over [b\0,c) E, cleared c, [c\0,c1) E to c1; K
			// emits c1; L5 breaks.
			name:    "T1-first-request-crosses-counts-none",
			storage: abcde,
			writes: func(c *rywCache) {
				c.clear([]byte("b"))
				c.clear([]byte("c"))
				c.set([]byte("c1"), []byte("w"))
			},
			begin: fgeS("a"), end: fgeS("z"), limits: lim(2, 1, -1),
			reqs: []string{
				`[("a",false,1) ("z",false,1)) rows=2 minRows=1 bytes=-1 wire=2/80000`,
				`[("b\x00",false,1) ("z",false,1)) rows=2 minRows=2 bytes=-1 wire=2/80000`,
			},
			rows: "a,c1", more: true,
			cache: `["a","b\x00"){a,b} ["b\x00","d\x00"){c,d}`,
		},
		{
			// T2. The multi-key empty [b,c) is a barrier (RYW:520-524): request 1 ends at
			// FGE(b). Reply a !more → cache [a,b). Request 2 from FGE(c) (U5) with rows 4,
			// the caller's limit after a (A5 input is the decremented limits, :748).
			// Reply c,d !more; L6 breaks at itEnd since end.key == it.beginKey (:688-690).
			name:    "T2-multi-key-barrier",
			storage: []string{"a", "b", "b5", "c", "d"},
			writes:  func(c *rywCache) { c.clearRange([]byte("b"), []byte("c")) },
			begin:   fgeS("a"), end: fgeS("z"), limits: lim(5, 1, -1),
			reqs: []string{
				`[("a",false,1) ("b",false,1)) rows=5 minRows=1 bytes=-1 wire=5/80000`,
				`[("c",false,1) ("z",false,1)) rows=4 minRows=1 bytes=-1 wire=4/80000`,
			},
			rows: "a,c,d",
		},
		{
			// T3, rows 2. The unreadable [m,m\0) stops skipUncached (:520): request
			// [FGE(a),FGE(m)); a,b fill the limit; L5 breaks before reaching m.
			name:    "T3-unreadable-barrier-rows-2",
			storage: []string{"a", "b", "n"},
			writes: func(c *rywCache) {
				c.atomic(MutSetVersionstampedValue, []byte("m"), make([]byte, 14))
			},
			begin: fgeS("a"), end: fgeS("z"), limits: lim(2, 1, -1),
			reqs: []string{`[("a",false,1) ("m",false,1)) rows=2 minRows=1 bytes=-1 wire=2/80000`},
			rows: "a,b", more: true,
		},
		{
			// T3, rows 3: the walk reaches [m,m\0) at L7 and type() throws (:692).
			name:    "T3-unreadable-barrier-rows-3",
			storage: []string{"a", "b", "n"},
			writes: func(c *rywCache) {
				c.atomic(MutSetVersionstampedValue, []byte("m"), make([]byte, 14))
			},
			begin: fgeS("a"), end: fgeS("z"), limits: lim(3, 1, -1),
			reqs: []string{`[("a",false,1) ("m",false,1)) rows=3 minRows=1 bytes=-1 wire=3/80000`},
			err:  ErrAccessedUnreadable,
		},
		{
			// T4. Backward: skipUncachedBack crosses cleared y and x, counts min(2,0)=0
			// (:1005-1009); U5' end = FGE(min(\xff\xff, z)). Reply y,x more → known [x,z)
			// (getKnownKeyRangeBack, :816-839). The walk descends through the cleared and
			// cached-empty segments to ["",x) U; U5' sets end = FGE(x). Request 2 rows 2.
			// Reply w,v → K' takes v,w in one skipContiguousBack run.
			name:    "T4-reverse-with-clears",
			storage: []string{"v", "w", "x", "y"},
			writes: func(c *rywCache) {
				c.clear([]byte("x"))
				c.clear([]byte("y"))
			},
			begin: fgeS("a"), end: fgeS("z"), limits: lim(2, 1, -1), reverse: true,
			reqs: []string{
				`[("a",false,1) ("z",false,1)) rows=2 minRows=1 bytes=-1 wire=-2/80000`,
				`[("a",false,1) ("x",false,1)) rows=2 minRows=1 bytes=-1 wire=-2/80000`,
			},
			rows: "w,v", more: true,
			cache: `["v","x"){v,w} ["x","z"){x,y}`,
		},
		{
			// T5. end = lastLessThan(m): U3's end.offset < 1 branch reads to FGE(m) with
			// additionalRows 1 (:713-716): rows -1, minRows 2, bytes 1 → wire 80000/1. The
			// reply's a,b come from NativeAPI's minRows continuation; cache [a,b\0). The walk
			// consumes a (bytes 0, minRows 0: reached), passes cleared b, and at itEnd still
			// needs one item past the end (L5 fails since itemsPastEnd 0 < 1 - 0). Request 2
			// takes A5's bytes == 0 branch: bytes -1, rows max(max(1,-1)+1, 1) = 2,
			// minRows 2 → wire 2/80000. The end then resolves to FGE(c); L3 breaks; the
			// epilogue trims to [a], more from isReached.
			name:    "T5-bytes-zero-minrows",
			storage: []string{"a", "b", "c"},
			writes:  func(c *rywCache) { c.clear([]byte("b")) },
			begin:   fgeS("a"), end: sel("m", false, 0), limits: lim(-1, 1, 1),
			reqs: []string{
				`[("a",false,1) ("m",false,1)) rows=-1 minRows=2 bytes=1 wire=80000/1`,
				`[("b\x00",false,1) ("m",false,1)) rows=2 minRows=2 bytes=-1 wire=2/80000`,
			},
			rows: "a", more: true,
		},
		{
			// T5 with caller rows MaxInt32 instead of unlimited: request 2 keeps a row
			// limit and sends 80000 (the D10 reason).
			name:    "T5-rows-maxint32",
			storage: []string{"a", "b", "c"},
			writes:  func(c *rywCache) { c.clear([]byte("b")) },
			begin:   fgeS("a"), end: sel("m", false, 0), limits: lim(math.MaxInt32, 1, 1),
			reqs: []string{
				`[("a",false,1) ("m",false,1)) rows=2147483647 minRows=2 bytes=1 wire=80000/1`,
				`[("b\x00",false,1) ("m",false,1)) rows=2147483647 minRows=2 bytes=-1 wire=80000/80000`,
			},
			rows: "a", more: true,
		},
		{
			// T6. 60 conflict-only ranges: empty-stack nodes at every c<i> and c<i>\0 (A1),
			// so seg0 = ["",c00), seg(2i+1) = [c<i>,c<i>\0), seg(2i+2) = [c<i>\0,c<i+1>).
			// skipUncached's budget clearLimit+100 = 100 (:703) ends on seg100 =
			// [c49\0,c50): request 1 is [FGE(a),FGE(c50)). Request 2 crosses the rest.
			name:    "T6-conflict-nodes-exhaust-budget",
			storage: abcde,
			writes:  func(c *rywCache) { addPointConflicts(c, 0, 60) },
			begin:   fgeS("a"), end: fgeS("z"), limits: lim(5, 1, -1),
			reqs: []string{
				`[("a",false,1) ("c50",false,1)) rows=5 minRows=1 bytes=-1 wire=5/80000`,
				`[("c50",false,1) ("z",false,1)) rows=2 minRows=1 bytes=-1 wire=2/80000`,
			},
			rows: "a,b,c,d,e", more: true,
		},
		{
			// T6b. AddWriteConflictRange(c00,c50) first: the enclosed ranges add no node
			// (WriteMap.cpp:253, :278), so seg1 = [c00,c50) and seg100 = [c99,c99\0);
			// request 1 is [FGE(a),FGE(c99\0)).
			name:    "T6b-conflict-flags-decide-nodes",
			storage: abcde,
			writes: func(c *rywCache) {
				c.addConflictRange([]byte("c00"), []byte("c50"))
				addPointConflicts(c, 0, 100)
			},
			begin: fgeS("a"), end: fgeS("z"), limits: lim(5, 1, -1),
			reqs: []string{
				`[("a",false,1) ("c99\x00",false,1)) rows=5 minRows=1 bytes=-1 wire=5/80000`,
				`[("c99\x00",false,1) ("z",false,1)) rows=2 minRows=1 bytes=-1 wire=2/80000`,
			},
			rows: "a,b,c,d,e", more: true,
		},
		{
			// T7. clear(e) then clear(e\0): the second finds [e\0,…) unmodified and keeps a
			// node at e\0 (WriteMap.cpp:168-196). Request 1 stops at the [c,c5) barrier.
			// Request 2 (clearLimit 2): skipUncached counts [e,e\0) and finds [e\0,e\0\0)
			// extends the empty run past one key, backs up to [c5,e), returns 1: rows
			// max(3+1,1) = 4, minRows 2. Request 3 reads the rest.
			name:    "T7-adjacent-clears",
			storage: []string{"a", "b", "d", "f"},
			writes: func(c *rywCache) {
				c.clearRange([]byte("c"), []byte("c5"))
				c.clear([]byte("e"))
				c.clear([]byte("e\x00"))
			},
			begin: fgeS("a"), end: fgeS("z"), limits: lim(5, 1, -1),
			reqs: []string{
				`[("a",false,1) ("c",false,1)) rows=5 minRows=1 bytes=-1 wire=5/80000`,
				`[("c5",false,1) ("e",false,1)) rows=4 minRows=2 bytes=-1 wire=4/80000`,
				`[("e\x00\x00",false,1) ("z",false,1)) rows=2 minRows=1 bytes=-1 wire=2/80000`,
			},
			rows: "a,b,d,f",
		},
		{
			// T7 reversed order: end_coalesce_clear (:180-191) removes the node at e\0, the
			// empty run is one segment [e,e\0\0), the count is 0: request 2 rows 3, minRows 1.
			name:    "T7-adjacent-clears-reversed",
			storage: []string{"a", "b", "d", "f"},
			writes: func(c *rywCache) {
				c.clearRange([]byte("c"), []byte("c5"))
				c.clear([]byte("e\x00"))
				c.clear([]byte("e"))
			},
			begin: fgeS("a"), end: fgeS("z"), limits: lim(5, 1, -1),
			reqs: []string{
				`[("a",false,1) ("c",false,1)) rows=5 minRows=1 bytes=-1 wire=5/80000`,
				`[("c5",false,1) ("e",false,1)) rows=3 minRows=1 bytes=-1 wire=3/80000`,
				`[("e\x00\x00",false,1) ("z",false,1)) rows=2 minRows=1 bytes=-1 wire=2/80000`,
			},
			rows: "a,b,d,f",
		},
		{
			// T7 reversed with NEXT_WRITE_NO_WRITE_CONFLICT_RANGE on the second clear:
			// clearNoConflict's end_coalesce_clear requires equal conflict flags (:565-566),
			// so the node stays and request 2 has rows 4 again.
			name:    "T7-adjacent-clears-no-conflict",
			storage: []string{"a", "b", "d", "f"},
			writes: func(c *rywCache) {
				c.clearRange([]byte("c"), []byte("c5"))
				c.clear([]byte("e\x00"))
				c.clearWith([]byte("e"), false)
			},
			begin: fgeS("a"), end: fgeS("z"), limits: lim(5, 1, -1),
			reqs: []string{
				`[("a",false,1) ("c",false,1)) rows=5 minRows=1 bytes=-1 wire=5/80000`,
				`[("c5",false,1) ("e",false,1)) rows=4 minRows=2 bytes=-1 wire=4/80000`,
				`[("e\x00\x00",false,1) ("z",false,1)) rows=2 minRows=1 bytes=-1 wire=2/80000`,
			},
			rows: "a,b,d,f",
		},
		{
			// Case 7, end offset > 1 backward: U5' keeps read_end = end and counts nothing
			// (clearLimit 0); U7' passes read_end.offset = 3 to setRequestLimits (RYW:1052-1061),
			// so minRows = rows = 3 → wire -3. Reply d,c,b more → cache [b,d\0); end resolves
			// to FGE(d\0); K' takes the run b,c,d and counts only d (rows 1).
			name:    "reverse-end-offset-gt-1",
			storage: abcde,
			begin:   fgeS("a"), end: sel("c", false, 3), limits: lim(1, 1, -1), reverse: true,
			reqs: []string{`[("a",false,1) ("c",false,3)) rows=3 minRows=3 bytes=-1 wire=-3/80000`},
			rows: "d", more: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

// keyTrace is an S1 getKey case (A9): read(GetKeyReq) as a limit-1 walk.
type keyTrace struct {
	name     string
	storage  []string
	writes   func(c *rywCache)
	snapshot bool // the snapshot-cache route (view = nil, A2.2)
	sel      KeySelector
	reqs     []string
	key      string
}

func (tc keyTrace) run(t *testing.T) {
	t.Helper()
	c := &rywCache{}
	if tc.writes != nil {
		tc.writes(c)
	}
	m := newMemStorage(tc.storage...)
	c.mu.Lock()
	w := c.newGetKeyWalkLocked(c.viewLocked(!tc.snapshot), tc.sel, []byte("\xff"))
	w.startLocked()
	c.mu.Unlock()
	if err := w.run(context.Background(), m.native); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range m.requests {
		got = append(got, reqString(r, w.reverse))
	}
	if strings.Join(got, "\n") != strings.Join(tc.reqs, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.reqs, "\n"))
	}
	if string(w.keyResult()) != tc.key {
		t.Fatalf("key %q, want %q", w.keyResult(), tc.key)
	}
}

// wgSeed is the wire-golden scenarios' storage (wgscenarios.Run), unprefixed.
var wgSeed = []string{"a", "b", "c", "d", "e", "m0", "m5"}

func setW(k string) func(c *rywCache) { return func(c *rywCache) { c.set([]byte(k), []byte("w")) } }

func TestRYWGetKeyTraces(t *testing.T) {
	t.Parallel()
	adjacent := func(first, second string, secondConflict bool) func(c *rywCache) {
		return func(c *rywCache) {
			c.clearRange([]byte("a5"), []byte("b5"))
			c.clear([]byte(first))
			c.clearWith([]byte(second), secondConflict)
		}
	}
	for _, tc := range []keyTrace{
		{
			// getkey_offset (A11): (b, true, 2) → A3 → (b\0, false, 2), unknown. U3 read_end =
			// FGE(\xff); U5 offset > 1 → FGE(b\0), additionalRows 1; A5 with offset 1:
			// minRows 2, rows 2. Reply c,d; begin resolves over c to d.
			name: "getkey_offset", storage: wgSeed, sel: sel("b", true, 2),
			reqs: []string{`[("b\x00",false,1) ("\xff",false,1)) rows=2 minRows=2 bytes=-1 wire=2/80000`},
			key:  "d",
		},
		{
			name: "getkey_first_greater_than", storage: wgSeed, sel: sel("b", true, 1),
			reqs: []string{`[("b\x00",false,1) ("\xff",false,1)) rows=1 minRows=1 bytes=-1 wire=1/80000`},
			key:  "c",
		},
		{
			// lastLessThan(c): A9 offset++ → FGE(c); backward U5' FGE branch,
			// k = min(\xff\xff, c): [FGE(""), FGE(c)) reverse, rows 1 → limit -1.
			name: "getkey_last_less_than", storage: wgSeed, sel: sel("c", false, 0),
			reqs: []string{`[("",false,1) ("c",false,1)) rows=1 minRows=1 bytes=-1 wire=-1/80000`},
			key:  "b",
		},
		{
			// lastLessOrEqual(c) → (c, true, 1) → A3 removeOrEqual → FGE(c\0).
			name: "getkey_last_less_or_equal", storage: wgSeed, sel: sel("c", true, 0),
			reqs: []string{`[("",false,1) ("c\x00",false,1)) rows=1 minRows=1 bytes=-1 wire=-1/80000`},
			key:  "c",
		},
		{
			// (d, false, -2) → (d, false, -1): U5' end.offset < 1 → FGE(d), additionalRows 2;
			// A5 with offset read_end.offset = 1: rows = minRows = 3 → limit -3.
			name: "getkey_negative_offset", storage: wgSeed, sel: sel("d", false, -2),
			reqs: []string{`[("",false,1) ("d",false,1)) rows=3 minRows=3 bytes=-1 wire=-3/80000`},
			key:  "a",
		},
		{
			// getkey_after_set: skipUncached crosses [b1,b1\0) KV with clearLimit 0 to itEnd
			// [b1\0,\xff\xff); read_end = FGE(\xff), rows 1. Reply c → [b\0,c\0) cached; begin
			// resolves across [b\0,b1) E to b1; K returns b1.
			name: "getkey_after_set", storage: wgSeed, writes: setW("b1"), sel: sel("b", true, 1),
			reqs: []string{`[("b\x00",false,1) ("\xff",false,1)) rows=1 minRows=1 bytes=-1 wire=1/80000`},
			key:  "b1",
		},
		{
			// snapshot_ryw_disabled_getkey: the snapshot cache alone (A2.2) ignores b1.
			name: "snapshot_ryw_disabled_getkey", storage: wgSeed, writes: setW("b1"), snapshot: true, sel: sel("b", true, 1),
			reqs: []string{`[("b\x00",false,1) ("\xff",false,1)) rows=1 minRows=1 bytes=-1 wire=1/80000`},
			key:  "c",
		},
		{
			// getkey_across_clears (T1 shape): request 1 crosses the clears of b, c, d and the
			// write c1, counting none (clearLimit 0): rows 1+2 (offset 3). Reply a,b,c; begin
			// resolves to (c\0, false, 2). Request 2 (clearLimit 2) crosses c1 and counts
			// [d,d\0): additionalRows 1 + 1 → rows 3, minRows 3. The 3rd key >= a is e.
			name: "getkey_across_clears", storage: wgSeed, sel: sel("a", false, 3),
			writes: func(c *rywCache) {
				c.clear([]byte("b"))
				c.clear([]byte("c"))
				c.set([]byte("c1"), []byte("w"))
				c.clear([]byte("d"))
			},
			reqs: []string{
				`[("a",false,1) ("\xff",false,1)) rows=3 minRows=3 bytes=-1 wire=3/80000`,
				`[("c\x00",false,1) ("\xff",false,1)) rows=3 minRows=3 bytes=-1 wire=3/80000`,
			},
			key: "e",
		},
		{
			// T6 for getKey: FGT(a) with 60 conflict ranges: request [FGE(a\0), FGE(c50)), rows 1.
			name: "getkey_across_write_conflict_ranges", storage: wgSeed, sel: sel("a", true, 1),
			writes: func(c *rywCache) { addPointConflicts(c, 0, 60) },
			reqs:   []string{`[("a\x00",false,1) ("c50",false,1)) rows=1 minRows=1 bytes=-1 wire=1/80000`},
			key:    "b",
		},
		{
			// T6b for getKey: [FGE(a\0), FGE(c99\0)).
			name: "getkey_enclosed_write_conflict_ranges", storage: wgSeed, sel: sel("a", true, 1),
			writes: func(c *rywCache) {
				c.addConflictRange([]byte("c00"), []byte("c50"))
				addPointConflicts(c, 0, 100)
			},
			reqs: []string{`[("a\x00",false,1) ("c99\x00",false,1)) rows=1 minRows=1 bytes=-1 wire=1/80000`},
			key:  "b",
		},
		{
			// T7 for getKey: (a, false, 4) with the barrier [a5,b5) and clear(c), clear(c\0).
			// Request 1: [FGE(a), FGE(a5)) rows 4 (additionalRows 3). Reply a; begin resolves
			// to (b5, false, 3). Request 2 (clearLimit 2): skipUncached counts [c,c\0), finds
			// the run extends at [c\0,c\0\0), backs up, returns 1: additionalRows 2 + 1 →
			// rows 4, minRows 4.
			name: "getkey_adjacent_clears", storage: wgSeed, sel: sel("a", false, 4),
			writes: adjacent("c", "c\x00", true),
			reqs: []string{
				`[("a",false,1) ("a5",false,1)) rows=4 minRows=4 bytes=-1 wire=4/80000`,
				`[("b5",false,1) ("c",false,1)) rows=4 minRows=4 bytes=-1 wire=4/80000`,
				`[("c\x00\x00",false,1) ("\xff",false,1)) rows=3 minRows=3 bytes=-1 wire=3/80000`,
			},
			key: "m0",
		},
		{
			// Reversed: end_coalesce_clear removes the node at c\0; the count is 0: rows 3.
			name: "getkey_adjacent_clears_reversed", storage: wgSeed, sel: sel("a", false, 4),
			writes: adjacent("c\x00", "c", true),
			reqs: []string{
				`[("a",false,1) ("a5",false,1)) rows=4 minRows=4 bytes=-1 wire=4/80000`,
				`[("b5",false,1) ("c",false,1)) rows=3 minRows=3 bytes=-1 wire=3/80000`,
				`[("c\x00\x00",false,1) ("\xff",false,1)) rows=3 minRows=3 bytes=-1 wire=3/80000`,
			},
			key: "m0",
		},
		{
			// Reversed with the second clear adding no conflict: the node stays, rows 4.
			name: "getkey_adjacent_clears_no_conflict", storage: wgSeed, sel: sel("a", false, 4),
			writes: adjacent("c\x00", "c", false),
			reqs: []string{
				`[("a",false,1) ("a5",false,1)) rows=4 minRows=4 bytes=-1 wire=4/80000`,
				`[("b5",false,1) ("c",false,1)) rows=4 minRows=4 bytes=-1 wire=4/80000`,
				`[("c\x00\x00",false,1) ("\xff",false,1)) rows=3 minRows=3 bytes=-1 wire=3/80000`,
			},
			key: "m0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

// U1 / U1' asymmetry (A7): the backward soft byte stop has no hasSatisfiedMinRows
// term (RYW:1000-1003). With minRows 2 and one cached row, the backward walk stops
// with zero requests; the forward one, whose U1 requires minRows satisfied
// (:693-697), reads storage.
func TestRYWWalkSoftByteStopAsymmetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reverse bool
		reqs    int
		rows    string
		more    bool
	}{{reverse: true, reqs: 0, rows: "y", more: true}, {reverse: false, reqs: 1, rows: "a,b,y"}} {
		c := &rywCache{}
		if tc.reverse {
			c.serverCache.insert([]byte("y"), []byte("z"), []KeyValue{{Key: []byte("y"), Value: []byte("vy")}})
		} else {
			c.serverCache.insert([]byte("a"), []byte("b"), []KeyValue{{Key: []byte("a"), Value: []byte("va")}})
		}
		m := newMemStorage("a", "b", "y")
		w, err := runWalk(c, c.liveLocked(), fgeS("a"), fgeS("z"), lim(-1, 2, 1000), tc.reverse, []byte("\xff"), m)
		if err != nil {
			t.Fatal(err)
		}
		r := w.rangeResult()
		if len(m.requests) != tc.reqs || keysOf(r.kvs) != tc.rows || r.more != tc.more {
			t.Fatalf("reverse=%t: %d requests, rows %q more %t; want %d, %q, %t", tc.reverse, len(m.requests), keysOf(r.kvs), r.more, tc.reqs, tc.rows, tc.more)
		}
	}
}

// clearLimit is A4's counted-clear cap; requestCount is not capped, only the shift.
func TestRYWClearLimit(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]int{0: 0, 1: 2, 19: 1 << 19, 20: 1 << 20, 21: 1 << 20, 64: 1 << 20} {
		if got := clearLimit(n); got != want {
			t.Errorf("clearLimit(%d) = %d, want %d", n, got, want)
		}
	}
}
