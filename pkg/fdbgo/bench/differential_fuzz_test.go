package bench

import (
	"fmt"
	"os"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// Differential fuzzer vs libfdb_c — RFC-054 (RFC-010 C2 follow-up). Generates random
// SEQUENCES of operations, applies the same sequence through both the pure-Go client
// and libfdb_c (each to its own prefix on one cluster), and asserts byte-identical
// persisted state. Sequences exercise interactions single-op batteries miss: RYW
// coalescing (atomic-after-Set, atomic accumulation), clear-then-set, ClearRange
// clamp/overwrite, last-write-wins across txns.
//
// Both clients run at API version 730 (bench TestMain), so the apiVersionAtLeast(510)
// Min→MinV2 / And→AndV2 op-code upgrade (NativeAPI.actor.cpp:5990-5994) applies
// identically. Reads pin a fresh shared read version (the RFC-053 L3 GRV-skew lesson).
// Excluded: SetVersionstampedKey/Value (stamp = commit version, not byte-comparable),
// oversized keys/values (the C binding aborts the process), conflicts (control-plane).

const (
	fzSet = iota
	fzClear
	fzClearRange
	fzAdd
	fzAnd
	fzOr
	fzXor
	fzMax
	fzMin
	fzByteMin
	fzByteMax
	fzAppendIfFits
	fzCommit // transaction boundary
	fzNumKinds
)

// fuzzKeys is a tiny domain so collisions / overwrites / accumulation are frequent —
// that is where interaction bugs live. Single chars keep ClearRange bounds simple.
var fuzzKeys = []string{"a", "b", "c", "d"}

type fuzzOp struct {
	kind    int
	keyIdx  int
	key2Idx int    // ClearRange end
	operand []byte // Set value / atomic operand (may be empty — doMin edge, Atomic.h:184)
}

// decodeFuzzOps walks the byte stream strictly left-to-right (never map iteration) so
// the op sequence is fully deterministic and reproducible from the seed. Ops are
// grouped into transactions split on fzCommit (and capped so a txn stays bounded).
func decodeFuzzOps(data []byte) [][]fuzzOp {
	var txns [][]fuzzOp
	var cur []fuzzOp
	flush := func() {
		if len(cur) > 0 {
			txns = append(txns, cur)
			cur = nil
		}
	}
	i := 0
	next := func() (byte, bool) {
		if i < len(data) {
			b := data[i]
			i++
			return b, true
		}
		return 0, false
	}
	for {
		b, ok := next()
		if !ok {
			break
		}
		kind := int(b) % fzNumKinds
		if kind == fzCommit {
			flush()
			continue
		}
		kb, _ := next()
		op := fuzzOp{kind: kind, keyIdx: int(kb) % len(fuzzKeys)}
		switch kind {
		case fzClearRange:
			eb, _ := next()
			op.key2Idx = int(eb) % len(fuzzKeys)
		case fzClear:
			// no operand
		default: // Set + atomics: read a 0..8-byte operand (length-prefixed)
			lb, _ := next()
			n := int(lb) % 9
			for j := 0; j < n; j++ {
				vb, ok := next()
				if !ok {
					break
				}
				op.operand = append(op.operand, vb)
			}
		}
		cur = append(cur, op)
		if len(cur) >= 24 { // bound txn size
			flush()
		}
	}
	flush()
	return txns
}

func applyGo(tx gofdb.Transaction, ops []fuzzOp, pfx string) {
	gk := func(i int) gofdb.Key { return gofdb.Key(pfx + fuzzKeys[i]) }
	for _, op := range ops {
		switch op.kind {
		case fzSet:
			tx.Set(gk(op.keyIdx), op.operand)
		case fzClear:
			tx.Clear(gk(op.keyIdx))
		case fzClearRange:
			b, e := op.keyIdx, op.key2Idx
			if b > e {
				b, e = e, b
			}
			if b == e {
				continue // zero-width: both clients no-op; skip to keep them identical
			}
			tx.ClearRange(gofdb.KeyRange{Begin: gk(b), End: gk(e)})
		case fzAdd:
			tx.Add(gk(op.keyIdx), op.operand)
		case fzAnd:
			tx.And(gk(op.keyIdx), op.operand)
		case fzOr:
			tx.Or(gk(op.keyIdx), op.operand)
		case fzXor:
			tx.Xor(gk(op.keyIdx), op.operand)
		case fzMax:
			tx.Max(gk(op.keyIdx), op.operand)
		case fzMin:
			tx.Min(gk(op.keyIdx), op.operand)
		case fzByteMin:
			tx.ByteMin(gk(op.keyIdx), op.operand)
		case fzByteMax:
			tx.ByteMax(gk(op.keyIdx), op.operand)
		case fzAppendIfFits:
			tx.AppendIfFits(gk(op.keyIdx), op.operand)
		}
	}
}

func applyC(tx cgofdb.Transaction, ops []fuzzOp, pfx string) {
	ck := func(i int) cgofdb.Key { return cgofdb.Key(pfx + fuzzKeys[i]) }
	for _, op := range ops {
		switch op.kind {
		case fzSet:
			tx.Set(ck(op.keyIdx), op.operand)
		case fzClear:
			tx.Clear(ck(op.keyIdx))
		case fzClearRange:
			b, e := op.keyIdx, op.key2Idx
			if b > e {
				b, e = e, b
			}
			if b == e {
				continue
			}
			tx.ClearRange(cgofdb.KeyRange{Begin: ck(b), End: ck(e)})
		case fzAdd:
			tx.Add(ck(op.keyIdx), op.operand)
		case fzAnd:
			tx.And(ck(op.keyIdx), op.operand)
		case fzOr:
			tx.Or(ck(op.keyIdx), op.operand)
		case fzXor:
			tx.Xor(ck(op.keyIdx), op.operand)
		case fzMax:
			tx.Max(ck(op.keyIdx), op.operand)
		case fzMin:
			tx.Min(ck(op.keyIdx), op.operand)
		case fzByteMin:
			tx.ByteMin(ck(op.keyIdx), op.operand)
		case fzByteMax:
			tx.ByteMax(ck(op.keyIdx), op.operand)
		case fzAppendIfFits:
			tx.AppendIfFits(ck(op.keyIdx), op.operand)
		}
	}
}

// stripNorm reads all KVs under prefix at the pinned version and returns the
// (prefix-stripped key, value) pairs — directly comparable across the two clients'
// different-length prefixes.
func stripNormGo(t *testing.T, v int64, pfx string) []kvPair {
	r, err := gofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("go PrefixRange: %v", err)
	}
	kvs := goRangeAt(t, v, r, gofdb.RangeOptions{})
	out := make([]kvPair, len(kvs))
	for i, kv := range kvs {
		out[i] = kvPair{append([]byte(nil), kv.Key[len(pfx):]...), append([]byte(nil), kv.Value...)}
	}
	return out
}

func stripNormC(t *testing.T, v int64, pfx string) []kvPair {
	r, err := cgofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("cgo PrefixRange: %v", err)
	}
	kvs := cgoRangeAt(t, v, r, cgofdb.RangeOptions{})
	out := make([]kvPair, len(kvs))
	for i, kv := range kvs {
		out[i] = kvPair{append([]byte(nil), kv.Key[len(pfx):]...), append([]byte(nil), kv.Value...)}
	}
	return out
}

// runDifferentialSequence applies the same op sequence through both clients to their
// own prefixes and asserts byte-identical persisted state. label identifies the run
// (a fuzz exec or a named corpus case) on failure.
func runDifferentialSequence(t *testing.T, label string, txns [][]fuzzOp) {
	t.Helper()
	// Per-process nonce so parallel fuzz workers (separate processes, shared
	// container) never collide; clear first so sequential execs in one worker start
	// clean. The prefix is stripped before compare, so it never affects the result.
	goPfx := fmt.Sprintf("fuzzdiff_%d_go_", os.Getpid())
	cPfx := fmt.Sprintf("fuzzdiff_%d_c_", os.Getpid())
	clearPrefix(t, goPfx)
	clearPrefix(t, cPfx)

	for _, ops := range txns {
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			applyGo(tx, ops, goPfx)
			return nil, nil
		}); err != nil {
			t.Fatalf("%s: go txn: %v", label, err)
		}
		mustCGo(t, func(tx cgofdb.Transaction) { applyC(tx, ops, cPfx) })
	}

	v := freshSharedVersion(t)
	goState := stripNormGo(t, v, goPfx)
	cState := stripNormC(t, v, cPfx)
	if len(goState) != len(cState) {
		t.Fatalf("%s: persisted KV count differs: go=%d cgo=%d\nseq=%s", label, len(goState), len(cState), fmtTxns(txns))
	}
	for i := range goState {
		if string(goState[i].k) != string(cState[i].k) || string(goState[i].v) != string(cState[i].v) {
			t.Fatalf("%s: pair %d differs: go=(%q,%x) cgo=(%q,%x)\nseq=%s",
				label, i, goState[i].k, goState[i].v, cState[i].k, cState[i].v, fmtTxns(txns))
		}
	}
}

func clearPrefix(t *testing.T, pfx string) {
	t.Helper()
	r, err := cgofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("PrefixRange: %v", err)
	}
	mustCGo(t, func(tx cgofdb.Transaction) { tx.ClearRange(r) })
}

func fmtTxns(txns [][]fuzzOp) string {
	kindName := []string{"Set", "Clear", "ClearRange", "Add", "And", "Or", "Xor", "Max", "Min", "ByteMin", "ByteMax", "AppendIfFits"}
	s := ""
	for ti, ops := range txns {
		s += fmt.Sprintf("\n  txn%d:", ti)
		for _, op := range ops {
			if op.kind == fzClearRange {
				s += fmt.Sprintf(" %s(%s,%s)", kindName[op.kind], fuzzKeys[op.keyIdx], fuzzKeys[op.key2Idx])
			} else if op.kind == fzClear {
				s += fmt.Sprintf(" %s(%s)", kindName[op.kind], fuzzKeys[op.keyIdx])
			} else {
				s += fmt.Sprintf(" %s(%s,%x)", kindName[op.kind], fuzzKeys[op.keyIdx], op.operand)
			}
		}
	}
	return s
}

func FuzzDifferential(f *testing.F) {
	// Seed corpus — generic byte slices the decoder maps to diverse sequences. The
	// targeted interaction axes (RYW coalescing etc.) are pinned deterministically by
	// TestDifferential_RYWCoalescing; these just give the fuzzer varied starting points.
	f.Add([]byte{fzSet, 0, 1, 0xaa, fzAdd, 0, 1, 0x01, fzCommit})
	f.Add([]byte{fzAdd, 0, 1, 0x05, fzAdd, 0, 1, 0x03, fzMin, 0, 1, 0x02, fzCommit})
	f.Add([]byte{fzSet, 0, 2, 0xde, 0xad, fzClearRange, 0, 3, fzSet, 1, 1, 0xff, fzCommit})
	f.Add([]byte{fzByteMax, 0, 3, 0x6d, 0x6d, 0x6d, fzByteMin, 0, 1, 0x7a, fzCommit})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	f.Fuzz(func(t *testing.T, data []byte) {
		txns := decodeFuzzOps(data)
		if len(txns) == 0 {
			return
		}
		runDifferentialSequence(t, "fuzz", txns)
	})
}

// TestDifferential_RYWCoalescing pins the intra-txn interaction axes FDB-C-dev called
// out (RFC-054 Gap B): an atomic applied after a Set in the same txn must evaluate
// against the RYW-pending Set, not storage (C++ WriteMap.cpp:366), and same-key atomic
// accumulation must fold identically (:368). Deterministic seeds, not random.
func TestDifferential_RYWCoalescing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		txns [][]fuzzOp
	}{
		{"set_then_add_same_txn", [][]fuzzOp{{
			{kind: fzSet, keyIdx: 0, operand: []byte{0x0a, 0, 0, 0, 0, 0, 0, 0}},
			{kind: fzAdd, keyIdx: 0, operand: []byte{0x01}},
		}}},
		{"add_accumulation_one_txn", [][]fuzzOp{{
			{kind: fzAdd, keyIdx: 0, operand: []byte{0x01}},
			{kind: fzAdd, keyIdx: 0, operand: []byte{0x01}},
			{kind: fzAdd, keyIdx: 0, operand: []byte{0x01}},
		}}},
		{"clear_then_set_same_txn", [][]fuzzOp{{
			{kind: fzSet, keyIdx: 1, operand: []byte("old")},
			{kind: fzClear, keyIdx: 1},
			{kind: fzSet, keyIdx: 1, operand: []byte("new")},
		}}},
		{"min_missing_then_add", [][]fuzzOp{{
			{kind: fzMin, keyIdx: 2, operand: []byte{0x08}}, // MinV2 on missing → operand
			{kind: fzAdd, keyIdx: 2, operand: []byte{0x01}},
		}}},
		{"set_then_or_xor", [][]fuzzOp{{
			{kind: fzSet, keyIdx: 3, operand: []byte{0xf0}},
			{kind: fzOr, keyIdx: 3, operand: []byte{0x0f}},
			{kind: fzXor, keyIdx: 3, operand: []byte{0xaa}},
		}}},
		{"appendiffits_accumulation", [][]fuzzOp{{
			{kind: fzAppendIfFits, keyIdx: 0, operand: []byte("ab")},
			{kind: fzAppendIfFits, keyIdx: 0, operand: []byte("cd")},
		}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: shares the per-process prefix with the fuzzer/other cases;
			// each case clears first, so sequential execution is clean.
			runDifferentialSequence(t, tc.name, tc.txns)
		})
	}
}
