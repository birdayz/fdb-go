package client

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

// Expected node lists below are derived by hand from WriteMap.cpp (FDB 7.3.77);
// each case's comment walks the cited lines. Dump lines are `"key" flags` with
// flags from o (operation) c (following cleared) f (following conflict)
// i (is_conflict) u (following unreadable) v (is_unreadable), "-" for none.

func nodeMut(k string, addConflict, versionstamp bool) rywNodeOp {
	return rywNodeOp{kind: rywNodeOpMutate, begin: []byte(k), addConflict: addConflict, versionstamp: versionstamp}
}

func nodeClear(b, e string, addConflict bool) rywNodeOp {
	return rywNodeOp{kind: rywNodeOpClear, begin: []byte(b), end: []byte(e), addConflict: addConflict}
}

func nodeConflict(b, e string) rywNodeOp {
	return rywNodeOp{kind: rywNodeOpConflict, begin: []byte(b), end: []byte(e)}
}

func nodeUnreadable(b, e string) rywNodeOp {
	return rywNodeOp{kind: rywNodeOpUnreadable, begin: []byte(b), end: []byte(e)}
}

func (n *rywNodes) do(op rywNodeOp) {
	switch op.kind {
	case rywNodeOpMutate:
		n.mutate(op.begin, op.addConflict, op.versionstamp)
	case rywNodeOpClear:
		n.clear(op.begin, op.end, op.addConflict)
	case rywNodeOpConflict:
		n.addConflictRange(op.begin, op.end)
	case rywNodeOpUnreadable:
		n.addUnmodifiedAndUnreadableRange(op.begin, op.end)
	}
}

// replayNodes applies ops eagerly (build first, so every mutator applies
// directly) or lazily (all logged, one build at the end).
func replayNodes(ops []rywNodeOp, eager bool) *rywNodes {
	n := &rywNodes{}
	if eager {
		n.build()
	}
	for _, op := range ops {
		n.do(op)
	}
	return n
}

func nl(key, flags string) string { return strconv.Quote(key) + " " + flags + "\n" }

// withSentinels wraps lines in the three unflagged sentinel nodes (WriteMap.h:129-134).
func withSentinels(lines ...string) string {
	return nl("", "-") + strings.Join(lines, "") + nl("\xff\xff", "-") + nl("\xff\xff\x00", "-")
}

func checkNodes(t *testing.T, ops []rywNodeOp, want string) {
	t.Helper()
	for _, eager := range []bool{true, false} {
		if got := replayNodes(ops, eager).dump(); got != want {
			t.Fatalf("eager=%t nodes:\n%s\nwant:\n%s", eager, got, want)
		}
	}
}

func ck(i int) string { return fmt.Sprintf("c%02d", i) }

// T6: AddWriteConflictRange([c_i, c_i\0)), i = 00..59. Each call: skip(c_i)
// lands in the unflagged range of c_(i-1)\0 (or ""), so :253 holds and :257-263
// insert c_i with fconf=isc=true; endKey is \xff\xff (not < c_i\0, :266); :278
// holds for the same entry, ++ reaches \xff\xff, beginKey != c_i\0 (:284), so
// :285-286 insert an unflagged c_i\0.
func TestRYWNodesT6(t *testing.T) {
	t.Parallel()
	var ops []rywNodeOp
	var want []string
	for i := range 60 {
		ops = append(ops, nodeConflict(ck(i), ck(i)+"\x00"))
		want = append(want, nl(ck(i), "fi"), nl(ck(i)+"\x00", "-"))
	}
	checkNodes(t, ops, withSentinels(want...))
}

// T6b: AddWriteConflictRange(c00, c50) inserts c00 "fi" and c50 "-" (as T6).
// Then for i < 50 skip(c_i) lands on c00's conflicted range: :253 fails (no
// begin node), endKey c50 >= c_i\0 so the loop is empty, :278 fails for the same
// entry (no end node). For i = 50 skip(c50) lands on the unflagged node c50:
// :253 holds, keyAtBegin && beginKey == c50 (:254), so c50 is replaced by "fi";
// :278 holds (the original c50), ++ reaches \xff\xff, so c50\0 "-" is added.
// i > 50 are as T6.
func TestRYWNodesT6b(t *testing.T) {
	t.Parallel()
	ops := []rywNodeOp{nodeConflict(ck(0), ck(50))}
	for i := range 100 {
		ops = append(ops, nodeConflict(ck(i), ck(i)+"\x00"))
	}
	want := []string{nl(ck(0), "fi")}
	for i := 50; i < 100; i++ {
		want = append(want, nl(ck(i), "fi"), nl(ck(i)+"\x00", "-"))
	}
	checkNodes(t, ops, withSentinels(want...))
}

// T7, three orders.
func TestRYWNodesT7(t *testing.T) {
	t.Parallel()
	// clearRange(c, c5): skip(c) in ""'s unmodified range: insert_begin (:171);
	// endKey \xff\xff > c5 so it stays; insert_end (:179-180, unmodified, beginKey
	// "" != c5); c "cfi", c5 "-". clear(e) = [e, e\0) likewise: e "cfi", e\0 "-".
	// clear(e\0) = [e\0, e\0\0): skip(e\0) is e\0's unmodified range: insert_begin;
	// insert_end at e\0\0; remove [e\0, e\0\0) drops the old e\0 (:189-192) and
	// :195 inserts e\0 "cfi". The node at e\0 stays.
	t.Run("e then e0", func(t *testing.T) {
		t.Parallel()
		checkNodes(t, []rywNodeOp{
			nodeClear("c", "c5", true), nodeClear("e", "e\x00", true), nodeClear("e\x00", "e\x00\x00", true),
		}, withSentinels(nl("c", "cfi"), nl("c5", "-"), nl("e", "cfi"), nl("e\x00", "cfi"), nl("e\x00\x00", "-")))
	})
	// clear(e\0) first: e\0 "cfi", e\0\0 "-". clear(e): skip(e) is c5's range
	// [c5, e\0), endKey == e\0 so ++ (:173-174) reaches the cleared conflicted
	// range of e\0: insert_end fails (:179), end_coalesce_clear holds (:181-182),
	// so remove [e, keyAfter(e\0)) drops e\0 and :195 inserts e "cfi".
	t.Run("e0 then e", func(t *testing.T) {
		t.Parallel()
		checkNodes(t, []rywNodeOp{
			nodeClear("c", "c5", true), nodeClear("e\x00", "e\x00\x00", true), nodeClear("e", "e\x00", true),
		}, withSentinels(nl("c", "cfi"), nl("c5", "-"), nl("e", "cfi"), nl("e\x00\x00", "-")))
	})
	// As above with the second clear through clearNoConflict: skip(e) is c5's
	// unmodified range: insert_begin (:534), lastConflicted = false, conflict
	// ranges = [e]. endKey == e\0 so ++ (:555-556) reaches e\0's cleared
	// conflicted range: insert_end fails (:560-563: not unmodified, e\0 is
	// following-conflicted), end_coalesce_clear fails because conflict (true) !=
	// lastConflicted (false) (:565-566). :583-584 insert e "c" (conflicted =
	// false); e\0 stays.
	t.Run("e0 then e without conflict", func(t *testing.T) {
		t.Parallel()
		checkNodes(t, []rywNodeOp{
			nodeClear("c", "c5", true), nodeClear("e\x00", "e\x00\x00", true), nodeClear("e", "e\x00", false),
		}, withSentinels(nl("c", "cfi"), nl("c5", "-"), nl("e", "c"), nl("e\x00", "cfi"), nl("e\x00\x00", "-")))
	})
}

// clearRange(a, z): a "cfi", z "-". set(k): skip(k) in a's range, new node
// (:113-122) k "ocfi" (cleared and conflicted from a, is_conflict by :95).
// clear(k) = [k, k\0): skip(k) is the operation (offset false), not a cleared
// range, so insert_begin (:171); endKey k\0 == end so ++ (:313-314) moves to
// k's following range [k\0, z), cleared and conflicted: no insert_end,
// end_coalesce_clear; remove [k, keyAfter(k\0)) drops k and :195 inserts an
// empty-stack k "cfi".
func TestRYWNodesSetThenClearInsideClear(t *testing.T) {
	t.Parallel()
	checkNodes(t, []rywNodeOp{nodeClear("a", "z", true), nodeMut("k", true, false)},
		withSentinels(nl("a", "cfi"), nl("k", "ocfi"), nl("z", "-")))
	checkNodes(t, []rywNodeOp{nodeClear("a", "z", true), nodeMut("k", true, false), nodeClear("k", "k\x00", true)},
		withSentinels(nl("a", "cfi"), nl("k", "cfi"), nl("z", "-")))
}

// The branches of clear (:171-202), addUnmodifiedAndUnreadableRange (:210-241)
// and clearNoConflict (:534-595) at an existing node.
func TestRYWNodesRangeMutatorEdges(t *testing.T) {
	t.Parallel()
	// clearNoConflict(a, z): a "c", z "-". clear(c, d) with a conflict: skip(c) is
	// a's cleared unconflicted range, so insert_begin by !is_conflict_range
	// (:171); endKey z > d stays; insert_end by !is_conflict_range (:179), beginKey
	// a != d; end flags cleared, unconflicted: c "cfi", d "c".
	checkNodes(t, []rywNodeOp{nodeClear("a", "z", false), nodeClear("c", "d", true)},
		withSentinels(nl("a", "c"), nl("c", "cfi"), nl("d", "c"), nl("z", "-")))
	// Inside a conflicted clear: no insert_begin (:171), no insert_end (:179), no
	// coalesce (beginKey a != d). Nothing changes.
	checkNodes(t, []rywNodeOp{nodeClear("a", "z", true), nodeClear("c", "d", true)},
		withSentinels(nl("a", "cfi"), nl("z", "-")))
	// Starting at an existing node without insert_begin, the removal starts after
	// it (ExtStringRef(begin, 1), :191; :230; :576), so a survives.
	checkNodes(t, []rywNodeOp{nodeClear("a", "z", true), nodeClear("a", "c", true)},
		withSentinels(nl("a", "cfi"), nl("z", "-")))
	checkNodes(t, []rywNodeOp{nodeUnreadable("a", "z"), nodeUnreadable("a", "c")},
		withSentinels(nl("a", "uv"), nl("z", "-")))
	checkNodes(t, []rywNodeOp{nodeClear("a", "z", false), nodeClear("a", "c", false)},
		withSentinels(nl("a", "c"), nl("z", "-")))
	// Ending at an operation node: set(d) without a conflict, then clear(a, d).
	// endKey of ""'s range is d == end, ++ lands on d's operation: unconflicted, but
	// keyAtBegin and beginKey == d (:180), so no end node and d keeps its stack.
	checkNodes(t, []rywNodeOp{nodeMut("d", false, false), nodeClear("a", "d", true)},
		withSentinels(nl("a", "cfi"), nl("d", "o")))
	// The same for the SVK range: readable d, but keyAtBegin at d (:219).
	checkNodes(t, []rywNodeOp{nodeMut("d", false, false), nodeUnreadable("a", "d")},
		withSentinels(nl("a", "uv"), nl("d", "o")))
	// And for clearNoConflict at a versionstamped d "ov": unreadable (:561), but
	// keyAtBegin and beginKey == d.
	checkNodes(t, []rywNodeOp{nodeMut("d", false, true), nodeClear("a", "d", false)},
		withSentinels(nl("a", "c"), nl("d", "ov")))
}

func TestRYWNodesConflictRangeAcrossClear(t *testing.T) {
	t.Parallel()
	// clear(c, e) with a conflict: c "cfi", e "-". addConflictRange(c, e): skip(c)
	// is c's conflicted range, :253 fails; endKey e == end, no loop; :278 fails.
	// Nothing changes.
	checkNodes(t, []rywNodeOp{nodeClear("c", "e", true), nodeConflict("c", "e")},
		withSentinels(nl("c", "cfi"), nl("e", "-")))
	// clearNoConflict(c, e): insert_begin (:534), conflict ranges [c]
	// (conflicted false), insert_end at e (:561): c "c", e "-".
	// addConflictRange(c, e): :253 holds at c, keyAtBegin and beginKey == c, so c
	// becomes "cfi" (:257-263); no loop; :278 holds, ++ lands at node e, beginKey
	// == e (:284): no end node. Interior flags only, no new node.
	checkNodes(t, []rywNodeOp{nodeClear("c", "e", false)}, withSentinels(nl("c", "c"), nl("e", "-")))
	checkNodes(t, []rywNodeOp{nodeClear("c", "e", false), nodeConflict("c", "e")},
		withSentinels(nl("c", "cfi"), nl("e", "-")))
	// addConflictRange(b, f) over the same clear: b "fi" (:257-263 from ""'s
	// range); the loop (:266-275) visits c and e, both keyAtBegin and unflagged,
	// and flags them; :278 holds for e, ++ reaches \xff\xff, f "-" (:285-286).
	checkNodes(t, []rywNodeOp{nodeClear("c", "e", false), nodeConflict("b", "f")},
		withSentinels(nl("b", "fi"), nl("c", "cfi"), nl("e", "fi"), nl("f", "-")))
}

func TestRYWNodesConflictRangeAcrossOps(t *testing.T) {
	t.Parallel()
	// set(b), set(c) without conflicts: b "o", c "o". addConflictRange(b, c\0):
	// skip(b) is b's operation, :253 holds, keyAtBegin && beginKey == b, so b is
	// replaced keeping its stack (:258) as "ofi". Loop: ++ to b's range [b\0, c)
	// (not keyAtBegin), ++ to c's operation (keyAtBegin, unflagged): c "ofi";
	// endKey c\0 == end stops. :278 holds for the original c; ++ moves to c's range
	// [c\0, \xff\xff), not keyAtBegin, so :285-286 insert c\0 "-". The boundaries
	// are unchanged: c\0 was already keyAfter(c).
	ops := []rywNodeOp{nodeMut("b", false, false), nodeMut("c", false, false)}
	before := replayNodes(ops, true)
	checkNodes(t, ops, withSentinels(nl("b", "o"), nl("c", "o")))
	ops = append(ops, nodeConflict("b", "c\x00"))
	checkNodes(t, ops, withSentinels(nl("b", "ofi"), nl("c", "ofi"), nl("c\x00", "-")))
	if got, want := nodeBoundaryList(t, replayNodes(ops, true)), nodeBoundaryList(t, before); got != want {
		t.Fatalf("boundaries changed: %s, want %s", got, want)
	}
	// Overlapping conflict ranges: (a, c) gives a "fi", c "-". (b, d): skip(b) in
	// a's conflicted range, :253 fails; the loop flags c (:268-273); :278 holds for
	// the original c, ++ reaches \xff\xff, d "-". The redundant node c stays.
	checkNodes(t, []rywNodeOp{nodeConflict("a", "c"), nodeConflict("b", "d")},
		withSentinels(nl("a", "fi"), nl("c", "fi"), nl("d", "-")))
	checkNodes(t, []rywNodeOp{nodeConflict("a", "c"), nodeConflict("b", "c")},
		withSentinels(nl("a", "fi"), nl("c", "-")))
}

// set(k) and clearRange(m, n) without conflicts, then addConflictRange(j, o).
// set(k): k "o". clearNoConflict(m, n): skip(m) is k's range [k\0, \xff\xff),
// insert_begin, conflict ranges [m] unconflicted, insert_end (not keyAtBegin):
// m "c", n "-". addConflictRange(j, o): j "fi" from ""'s range; the loop flags
// k's operation (keyAtBegin) to "ofi", skips k's range (not keyAtBegin), flags m
// "cfi" and n "fi"; :278 holds for n, o "-".
func TestRYWNodesNoConflictWritesThenConflictRange(t *testing.T) {
	t.Parallel()
	ops := []rywNodeOp{nodeMut("k", false, false), nodeClear("m", "n", false)}
	checkNodes(t, ops, withSentinels(nl("k", "o"), nl("m", "c"), nl("n", "-")))
	checkNodes(t, append(ops, nodeConflict("j", "o")),
		withSentinels(nl("j", "fi"), nl("k", "ofi"), nl("m", "cfi"), nl("n", "fi"), nl("o", "-")))
	// A later conflicting set on k only ORs is_conflict (:95, WriteMap.h:143); a
	// non-conflicting set never clears it.
	checkNodes(t, []rywNodeOp{nodeMut("k", false, false), nodeMut("k", true, false), nodeMut("k", false, false)},
		withSentinels(nl("k", "oi")))
}

func TestRYWNodesClearNoConflictToggles(t *testing.T) {
	t.Parallel()
	// addConflictRange(c, d): c "fi", d "-". clearNoConflict(b, e): skip(b) in
	// ""'s range: insert_begin, lastConflicted = conflicted = false, ranges [b].
	// The loop toggles at c (conflicted) and d (not): ranges [b, c, d]. ++ is not
	// taken (endKey \xff\xff); insert_end (:561). Remove [b, e) drops c and d;
	// :579-586 insert b "c", c "cfi", d "c"; e "-".
	checkNodes(t, []rywNodeOp{nodeConflict("c", "d"), nodeClear("b", "e", false)},
		withSentinels(nl("b", "c"), nl("c", "cfi"), nl("d", "c"), nl("e", "-")))
	// clearNoConflict(a, m): a "c", m "-"; addConflictRange(f, g): f "cfi", g "c".
	// clearNoConflict(b, z): skip(b) in a's cleared range: no insert_begin, so
	// conflicted flips to true (:544). Toggles at f and g; m is not a toggle.
	// insert_end at z; remove [b\0, z) drops f, g, m; f "cfi", g "c", z "-".
	checkNodes(t, []rywNodeOp{nodeClear("a", "m", false), nodeConflict("f", "g"), nodeClear("b", "z", false)},
		withSentinels(nl("a", "c"), nl("f", "cfi"), nl("g", "c"), nl("z", "-")))
	// clearNoConflict(a, z): a "c", z "-". set(k) with a conflict: k "oci"
	// (cleared from a, is_conflict, following not conflicted). clearNoConflict(j,
	// k\0): skip(j) in a's cleared range: conflicted flips to true; the loop
	// toggles at k's operation (conflicted), lastConflicted = true; endKey k\0 ==
	// end so ++ to k's range [k\0, z), cleared, not conflicted. :562-563 hold
	// (entry k is_conflict, not following, beginKey == end, not keyAtBegin), so
	// insert_end; no coalesce (false != lastConflicted). Remove [j\0, k\0) drops k;
	// k "cfi"; k\0 "c".
	checkNodes(t, []rywNodeOp{nodeClear("a", "z", false), nodeMut("k", true, false), nodeClear("j", "k\x00", false)},
		withSentinels(nl("a", "c"), nl("k", "cfi"), nl("k\x00", "c"), nl("z", "-")))
	// Inside a conflicted cleared range a no-conflict clear changes nothing: skip
	// in a's range, no insert_begin, no toggle, no insert_end, no coalesce.
	checkNodes(t, []rywNodeOp{nodeClear("a", "z", true), nodeClear("c", "d", false)},
		withSentinels(nl("a", "cfi"), nl("z", "-")))
	// A no-conflict clear ending at a conflicted clear of the same conflict state
	// coalesces: clearNoConflict(e\0, e\0\0), then clearNoConflict(e, e\0): ++
	// lands on e\0's cleared unconflicted range with lastConflicted false, so
	// end_coalesce_clear (:565-566) removes e\0.
	checkNodes(t, []rywNodeOp{nodeClear("e\x00", "e\x00\x00", false), nodeClear("e", "e\x00", false)},
		withSentinels(nl("e", "c"), nl("e\x00\x00", "-")))
}

func TestRYWNodesUnreadableRange(t *testing.T) {
	t.Parallel()
	// set(b), set(c) with conflicts: b "oi", c "oi"; clear(d, e): d "cfi", e "-".
	// addUnmodifiedAndUnreadableRange(a, f): skip(a) in ""'s readable range:
	// insert_begin (:210); endKey b < f so skip(f) lands in e's range: insert_end
	// (:218-219: readable, beginKey e != f); no coalesce. Remove [a, f) drops b, c,
	// d, e; a "uv" (:234), f "-".
	checkNodes(t, []rywNodeOp{
		nodeMut("b", true, false), nodeMut("c", true, false), nodeClear("d", "e", true), nodeUnreadable("a", "f"),
	}, withSentinels(nl("a", "uv"), nl("f", "-")))
	// (c, d) gives c "uv", d "-". (b, c): endKey c == end, ++ lands on c's
	// unmodified unreadable unconflicted range: no insert_end,
	// end_coalesce_unmodified (:220-221) removes c; b "uv".
	checkNodes(t, []rywNodeOp{nodeUnreadable("c", "d"), nodeUnreadable("b", "c")},
		withSentinels(nl("b", "uv"), nl("d", "-")))
	// Inside an unreadable range nothing changes: no insert_begin (unmodified,
	// unconflicted, unreadable), no insert_end, no coalesce (beginKey a != c).
	checkNodes(t, []rywNodeOp{nodeUnreadable("a", "d"), nodeUnreadable("b", "c")},
		withSentinels(nl("a", "uv"), nl("d", "-")))
	// A conflicted unreadable range still inserts a begin node: addConflictRange(a,
	// d) then (b, c): skip(b) is a's conflicted range: insert_begin; skip(c)
	// stays; insert_end (conflicted) with a's flags: c "fi".
	checkNodes(t, []rywNodeOp{nodeConflict("a", "d"), nodeUnreadable("b", "c")},
		withSentinels(nl("a", "fi"), nl("b", "uv"), nl("c", "fi"), nl("d", "-")))
}

func TestRYWNodesMutateFlags(t *testing.T) {
	t.Parallel()
	// addConflictRange(k, m): k "fi", m "-". mutate(k) without a conflict: skip(k)
	// lands on k's empty stack (offset true, :346), so is_conflict_range() is
	// following_keys_conflict (:95): k "ofi". A versionstamped value at m: m's
	// range is readable and unconflicted: m "ov" (:97-98).
	checkNodes(t, []rywNodeOp{nodeConflict("k", "m"), nodeMut("k", false, false), nodeMut("m", false, true)},
		withSentinels(nl("k", "ofi"), nl("m", "ov")))
	// Inside an unreadable range: a "uv", c "-"; mutate(b) copies a's following
	// unreadable into both b flags: b "oiuv".
	checkNodes(t, []rywNodeOp{nodeUnreadable("a", "c"), nodeMut("b", true, false)},
		withSentinels(nl("a", "uv"), nl("b", "oiuv"), nl("c", "-")))
	// A dependent op inside a cleared range is still one operation node
	// (:103-111): b "ocfi".
	checkNodes(t, []rywNodeOp{nodeClear("a", "c", true), nodeMut("b", false, false)},
		withSentinels(nl("a", "cfi"), nl("b", "ocfi"), nl("c", "-")))
	// In an operation node's following range, is_conflict_range() and
	// is_unreadable() are the following_* flags (offset true, WriteMap.h:168-169):
	// set(b) with a conflict and versionstamped, b "oiv"; set(c) without one
	// copies b's unconflicted, readable range: c "o".
	checkNodes(t, []rywNodeOp{nodeMut("b", true, true), nodeMut("c", false, false)},
		withSentinels(nl("b", "oiv"), nl("c", "o")))
	// mutate at "": skip("") is the "" sentinel.
	checkNodes(t, []rywNodeOp{nodeMut("", true, false)},
		nl("", "oi")+nl("\xff\xff", "-")+nl("\xff\xff\x00", "-"))
}

// The boundary rule: an operation node k contributes keyAfter(k) unless the next
// node is keyAfter(k). set(k), clear(k\0): k "o", k\0 "cfi", k\0\0 "-".
func TestRYWNodesOpBoundaryRule(t *testing.T) {
	t.Parallel()
	n := replayNodes([]rywNodeOp{nodeMut("k", true, false), nodeClear("k\x00", "k\x00\x00", true)}, false)
	if got, want := n.dump(), withSentinels(nl("k", "oi"), nl("k\x00", "cfi"), nl("k\x00\x00", "-")); got != want {
		t.Fatalf("nodes:\n%s\nwant:\n%s", got, want)
	}
	if got, want := nodeBoundaryList(t, n), `"" "k" "k\x00" "k\x00\x00" "\xff\xff" "\xff\xff\x00"`; got != want {
		t.Fatalf("boundaries %s, want %s", got, want)
	}
	m := replayNodes([]rywNodeOp{nodeMut("k", true, false)}, false)
	if got, want := nodeBoundaryList(t, m), `"" "k" "k\x00" "\xff\xff" "\xff\xff\x00"`; got != want {
		t.Fatalf("boundaries %s, want %s", got, want)
	}
	seg := m.segmentAt([]byte("k"))
	if !seg.op || !seg.conflict || seg.cleared || seg.begin.compare(rywBoundary{key: []byte("k")}) != 0 ||
		seg.end.compare(rywBoundary{key: []byte("k\x00")}) != 0 {
		t.Fatalf("segmentAt(k) = %+v", seg)
	}
	seg = m.segmentAt([]byte("k\x00"))
	if seg.op || seg.conflict || seg.begin.compare(rywBoundary{key: []byte("k\x00")}) != 0 ||
		seg.end.compare(rywBoundary{key: allKeysEnd}) != 0 {
		t.Fatalf("segmentAt(k\\0) = %+v", seg)
	}
	if b := m.prevBoundary([]byte("k\x00\x00")); b.compare(rywBoundary{key: []byte("k\x00")}) != 0 {
		t.Fatalf("prevBoundary(k\\0\\0) = %q+%t", b.key, b.after)
	}
	if b := m.prevBoundary([]byte("k\x00")); b.compare(rywBoundary{key: []byte("k")}) != 0 {
		t.Fatalf("prevBoundary(k\\0) = %q+%t", b.key, b.after)
	}
	// The sentinel segment.
	seg = m.segmentAt(allKeysEnd)
	if seg.begin.compare(rywBoundary{key: allKeysEnd}) != 0 || seg.end.compare(rywBoundary{key: afterAllKeysBytes}) != 0 {
		t.Fatalf("segmentAt(\\xff\\xff) = %+v", seg)
	}
}

// --- brute force from dump ------------------------------------------------------

type dumpedNode struct {
	key                                       []byte
	op, cleared, fconf, isc, funread, iunread bool
}

func parseNodeDump(t testing.TB, d string) []dumpedNode {
	t.Helper()
	var out []dumpedNode
	for _, line := range strings.Split(strings.TrimSuffix(d, "\n"), "\n") {
		sp := strings.LastIndexByte(line, ' ')
		k, err := strconv.Unquote(line[:sp])
		if err != nil {
			t.Fatalf("bad dump line %q: %v", line, err)
		}
		f := line[sp+1:]
		out = append(out, dumpedNode{
			key: []byte(k), op: strings.Contains(f, "o"), cleared: strings.Contains(f, "c"),
			fconf: strings.Contains(f, "f"), isc: strings.Contains(f, "i"),
			funread: strings.Contains(f, "u"), iunread: strings.Contains(f, "v"),
		})
	}
	return out
}

// bruteBoundaries derives the write-side boundaries from the dump text: every
// node key, plus key\0 for an operation node unless the next node is key\0.
func bruteBoundaries(t testing.TB, n *rywNodes) [][]byte {
	t.Helper()
	nodes := parseNodeDump(t, n.dump())
	var out [][]byte
	for i, nd := range nodes {
		out = append(out, nd.key)
		ka := append(bytes.Clone(nd.key), 0)
		if nd.op && (i+1 == len(nodes) || !bytes.Equal(nodes[i+1].key, ka)) {
			out = append(out, ka)
		}
	}
	return out
}

func nodeBoundaryList(t testing.TB, n *rywNodes) string {
	t.Helper()
	var parts []string
	for _, b := range bruteBoundaries(t, n) {
		parts = append(parts, strconv.Quote(string(b)))
	}
	return strings.Join(parts, " ")
}

func checkNodesInvariants(t *testing.T, n *rywNodes) {
	t.Helper()
	s := n.sorted
	if len(s) < 3 || len(s[0].key) != 0 || !bytes.Equal(s[len(s)-2].key, allKeysEnd) ||
		!bytes.Equal(s[len(s)-1].key, afterAllKeysBytes) {
		t.Fatalf("sentinels missing:\n%s", n.dump())
	}
	for i := 1; i < len(s); i++ {
		if bytes.Compare(s[i-1].key, s[i].key) >= 0 {
			t.Fatalf("unsorted at %d:\n%s", i, n.dump())
		}
	}
}

// --- random histories -----------------------------------------------------------

func randNodeKey(r *rand.Rand) []byte {
	k := []byte{"abcdef"[r.IntN(6)]}
	for range r.IntN(3) {
		k = append(k, "ab\x00"[r.IntN(3)])
	}
	return k
}

func randNodeRange(r *rand.Rand) ([]byte, []byte) {
	for {
		a, b := randNodeKey(r), randNodeKey(r)
		if r.IntN(4) == 0 {
			b = append(bytes.Clone(a), 0)
		}
		switch c := bytes.Compare(a, b); {
		case c < 0:
			return a, b
		case c > 0:
			return b, a
		}
	}
}

func randNodeHistory(r *rand.Rand, n int) []rywNodeOp {
	var ops []rywNodeOp
	for len(ops) < n {
		switch x := r.IntN(10); {
		case x < 5: // runs of mutates exercise mergeMutates
			for range 1 + r.IntN(6) {
				ops = append(ops, rywNodeOp{
					kind: rywNodeOpMutate, begin: randNodeKey(r), addConflict: r.IntN(2) == 0, versionstamp: r.IntN(6) == 0,
				})
			}
		case x < 7:
			b, e := randNodeRange(r)
			ops = append(ops, rywNodeOp{kind: rywNodeOpClear, begin: b, end: e, addConflict: r.IntN(2) == 0})
		case x < 9:
			b, e := randNodeRange(r)
			ops = append(ops, rywNodeOp{kind: rywNodeOpConflict, begin: b, end: e})
		default:
			b, e := randNodeRange(r)
			ops = append(ops, rywNodeOp{kind: rywNodeOpUnreadable, begin: b, end: e})
		}
	}
	return ops
}

// Lazy build equals eager application: the same random history built before
// every op (direct), logged and replayed once, and built at a random midpoint.
// A clone taken midway and driven differently never changes the original.
func TestRYWNodesLazyBuildEquivalence(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(262, 2))
	for iter := range 3000 {
		ops := randNodeHistory(r, 1+r.IntN(40))
		eager := &rywNodes{}
		for _, op := range ops {
			eager.build()
			eager.do(op)
		}
		want := eager.dump()
		checkNodesInvariants(t, eager)
		if got := replayNodes(ops, false).dump(); got != want {
			t.Fatalf("iter %d: lazy\n%s\neager\n%s\nops %+v", iter, got, want, ops)
		}
		mid := r.IntN(len(ops) + 1)
		m := &rywNodes{}
		for _, op := range ops[:mid] {
			m.do(op)
		}
		fork := m.clone()
		if r.IntN(2) == 0 {
			m.build()
		}
		for _, op := range randNodeHistory(r, 10) {
			fork.do(op)
		}
		fork.build()
		for _, op := range ops[mid:] {
			m.do(op)
		}
		if got := m.dump(); got != want {
			t.Fatalf("iter %d: midway build at %d\n%s\neager\n%s", iter, mid, got, want)
		}
		m.build() // idempotent
		if got := m.dump(); got != want {
			t.Fatalf("iter %d: second build changed nodes", iter)
		}
	}
}

func randNodeProbe(r *rand.Rand) []byte {
	switch r.IntN(8) {
	case 0:
		return nil
	case 1:
		return bytes.Clone(allKeysEnd)
	case 2:
		return []byte{0xff, 0xfe}
	default:
		return randNodeKey(r)
	}
}

// floorCeil, prevBoundary and segmentAt agree with the boundary list derived
// from dump(), and the iterator's ++ / -- walk visits exactly those boundaries.
func TestRYWNodesBoundaryQueries(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(262, 3))
	for iter := range 1500 {
		n := replayNodes(randNodeHistory(r, r.IntN(30)), r.IntN(2) == 0)
		bounds := bruteBoundaries(t, n)
		nodes := parseNodeDump(t, n.dump())
		for range 30 {
			p := randNodeProbe(r)
			var floor, ceil, prev []byte
			haveCeil := false
			for _, b := range bounds {
				if bytes.Compare(b, p) <= 0 {
					floor = b
				} else if !haveCeil {
					ceil, haveCeil = b, true
				}
				if bytes.Compare(b, p) < 0 {
					prev = b
				}
			}
			gf, gc := n.floorCeil(p)
			if !bytes.Equal(gf.materialize(), floor) || !bytes.Equal(gc.materialize(), ceil) {
				t.Fatalf("iter %d: floorCeil(%q) = %q, %q; want %q, %q\n%s", iter, p, gf.materialize(), gc.materialize(), floor, ceil, n.dump())
			}
			if gp := n.prevBoundary(p); !bytes.Equal(gp.materialize(), prev) {
				t.Fatalf("iter %d: prevBoundary(%q) = %q, want %q\n%s", iter, p, gp.materialize(), prev, n.dump())
			}
			// The owning node is the largest node <= p; an operation node owns the
			// one-key segment at its key, else its following range.
			var own dumpedNode
			for _, nd := range nodes {
				if bytes.Compare(nd.key, p) <= 0 {
					own = nd
				}
			}
			onOp := own.op && bytes.Equal(own.key, p)
			want := rywNodeSeg{
				op: onOp, cleared: !onOp && own.cleared,
				conflict:   (onOp && own.isc) || (!onOp && own.fconf),
				unreadable: (onOp && own.iunread) || (!onOp && own.funread),
			}
			got := n.segmentAt(p)
			if !bytes.Equal(got.begin.materialize(), floor) || !bytes.Equal(got.end.materialize(), ceil) ||
				got.op != want.op || got.cleared != want.cleared || got.conflict != want.conflict || got.unreadable != want.unreadable {
				t.Fatalf("iter %d: segmentAt(%q) = %+v, want [%q, %q) %+v\n%s", iter, p, got, floor, ceil, want, n.dump())
			}
		}
		// Forward walk from "" to the sentinel, then back.
		it := n.iter()
		it.skip(allKeysBegin)
		var walk []rywNodeSeg
		for it.beginKey().compare(rywBoundary{key: allKeysEnd}) < 0 {
			walk = append(walk, it.seg())
			it.inc()
		}
		if len(walk)+2 != len(bounds) { // the walk stops before \xff\xff and \xff\xff\0
			t.Fatalf("iter %d: walk has %d segments, %d boundaries\n%s", iter, len(walk), len(bounds), n.dump())
		}
		for i, s := range walk {
			if !bytes.Equal(s.begin.materialize(), bounds[i]) || !bytes.Equal(s.end.materialize(), bounds[i+1]) {
				t.Fatalf("iter %d: segment %d = [%q, %q), want [%q, %q)", iter, i, s.begin.materialize(), s.end.materialize(), bounds[i], bounds[i+1])
			}
		}
		for i := len(walk) - 1; i >= 0; i-- {
			it.dec()
			if s := it.seg(); s.begin.compare(walk[i].begin) != 0 || s.op != walk[i].op || s.conflict != walk[i].conflict {
				t.Fatalf("iter %d: backward segment %d = %+v, want %+v", iter, i, s, walk[i])
			}
		}
	}
}

func TestRYWNodesCloneIsolation(t *testing.T) {
	t.Parallel()
	n := &rywNodes{}
	n.mutate([]byte("b"), true, false)
	unbuilt := n.clone()
	n.build()
	built := n.clone()
	want := n.dump()
	built.clear([]byte("a"), []byte("c"), true)
	unbuilt.addConflictRange([]byte("a"), []byte("z"))
	unbuilt.build()
	if got := n.dump(); got != want {
		t.Fatalf("clone mutation leaked:\n%s\nwant\n%s", got, want)
	}
	if built.dump() == want || unbuilt.dump() == want {
		t.Fatal("clones did not change")
	}
}

// D1b build benchmark: 10k out-of-order mutates logged then one build, against
// the same mutates applied directly after an up-front build.
func BenchmarkRYWNodesBuild(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 2))
	keys := make([][]byte, 10000)
	for i := range keys {
		keys[i] = fmt.Appendf(nil, "k%08d", i)
	}
	r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	b.Run("lazy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var n rywNodes
			for _, k := range keys {
				n.mutate(k, true, false)
			}
			n.build()
		}
	})
	b.Run("eager", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var n rywNodes
			n.build()
			for _, k := range keys {
				n.mutate(k, true, false)
			}
		}
	})
}
