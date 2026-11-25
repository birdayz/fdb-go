package client

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
)

// TestAppendIfFitsOwnedMatchesDoAppendIfFits pins appendIfFitsOwned to C++
// doAppendIfFits (Atomic.h:114-137) on every arm, including VALUE_SIZE_LIMIT,
// and checks it never writes inside the input's length and returns a capped slice.
func TestAppendIfFitsOwnedMatchesDoAppendIfFits(t *testing.T) {
	t.Parallel()
	grown, owned := appendIfFitsOwned([]byte("ab"), 0, []byte("c")) // owns spare capacity
	if owned < 64 || !bytes.Equal(grown, []byte("abc")) {
		t.Fatalf("grow: %q owned=%d", grown, owned)
	}
	full := bytes.Repeat([]byte{'f'}, valueSizeLimit-10)
	cases := []struct {
		name    string
		v       []byte
		owned   int32
		param   []byte
		inPlace bool
	}{
		{"absent-base", nil, 0, []byte("p"), false},
		{"empty-base", []byte{}, 0, []byte("p"), false},
		{"empty-both", []byte{}, 0, []byte{}, false},
		{"nil-both", nil, 0, nil, false},
		{"empty-operand", []byte("v"), 0, []byte{}, false},
		{"empty-operand-owned", grown, owned, nil, false},
		{"unowned", []byte("v"), 0, []byte("p"), false},
		{"owned", grown, owned, []byte("de"), true},
		{"owned-too-small", grown, owned, bytes.Repeat([]byte{'x'}, int(owned)), false},
		{"exactly-limit", full, 0, bytes.Repeat([]byte{'p'}, 10), false},
		{"over-limit", full, 0, bytes.Repeat([]byte{'p'}, 11), false},
		{"empty-base-over-limit", nil, 0, bytes.Repeat([]byte{'p'}, valueSizeLimit+1), false},
	}
	for _, tc := range cases {
		before := bytes.Clone(tc.v)
		want := doAppendIfFits(tc.v, tc.param)
		got, gotOwned := appendIfFitsOwned(tc.v, tc.owned, tc.param)
		if !bytes.Equal(got, want) || (got == nil) != (want == nil) {
			t.Errorf("%s: got %d bytes (nil=%t), want %d (nil=%t)", tc.name, len(got), got == nil, len(want), want == nil)
		}
		if !bytes.Equal(tc.v, before) {
			t.Errorf("%s: input changed", tc.name)
		}
		if gotOwned > 0 && cap(got) != len(got) {
			t.Errorf("%s: owned result has cap %d > len %d", tc.name, cap(got), len(got))
		}
		if shared := len(got) > 0 && len(tc.v) > 0 && &got[0] == &tc.v[0]; shared && len(got) != len(tc.v) && !tc.inPlace {
			t.Errorf("%s: extended an unowned array", tc.name)
		} else if tc.inPlace && !shared {
			t.Errorf("%s: owned capacity not reused", tc.name)
		}
	}
}

// TestRYWAppendIfFitsFoldsInPlace pins the write map and its commit vector for
// repeated appends: over a Set (C++ SetValue+AppendIfFits → SetValue,
// WriteMap.cpp:388-391) and over storage (AppendIfFits+AppendIfFits →
// AppendIfFits, :392-394). A pinned version and earlier read results keep the
// pre-append value, a caller's append to a read result cannot reach the write
// map, and an append past VALUE_SIZE_LIMIT leaves the value unchanged.
func TestRYWAppendIfFitsFoldsInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tx := &Transaction{}
	set, chain := []byte("bunch/set"), []byte("bunch/stored")
	tx.Set(set, []byte("v0"))
	wantSet, wantChain := []byte("v0"), []byte(nil)
	var reads [][]byte
	var readWant [][]byte
	var pinned []*rywWrites
	var pinnedSet, pinnedChain [][]byte
	for i := 0; i < 40; i++ {
		p := fmt.Appendf(nil, "<%02d:%s>", i, bytes.Repeat([]byte{'e'}, 40))
		tx.Atomic(MutAppendIfFits, set, p)
		tx.Atomic(MutAppendIfFits, chain, p)
		wantSet, wantChain = append(wantSet, p...), append(wantChain, p...)
		v, err := tx.Snapshot().Get(ctx, set)
		if err != nil || !bytes.Equal(v, wantSet) {
			t.Fatalf("append %d: Get = %q, %v", i, v, err)
		}
		reads, readWant = append(reads, v), append(readWant, bytes.Clone(v))
		tx.ryw.mu.Lock()
		e, _ := tx.ryw.liveLocked().entryOf(set)
		scribbled := append(e.value, "caller scribble"...) // as a caller appending to a result would
		scribbleCallerBuf(scribbled[len(e.value):])
		if i%3 == 0 {
			pinned = append(pinned, tx.ryw.pinWritesLocked())
			pinnedSet, pinnedChain = append(pinnedSet, bytes.Clone(wantSet)), append(pinnedChain, bytes.Clone(wantChain))
		}
		tx.ryw.mu.Unlock()
	}
	tx.ryw.mu.Lock()
	live := tx.ryw.liveLocked()
	e, _ := live.entryOf(set)
	if e.hasAtomics || !bytes.Equal(e.value, wantSet) || e.valueOwned == 0 || cap(e.value) != len(e.value) {
		t.Fatalf("set entry: atomics=%t value=%q owned=%d cap=%d", e.hasAtomics, e.value, e.valueOwned, cap(e.value))
	}
	c, _ := live.entryOf(chain)
	if len(c.atomics) != 1 || c.atomics[0].typ != MutAppendIfFits || !bytes.Equal(c.atomics[0].param, wantChain) || c.atomics[0].owned == 0 {
		t.Fatalf("chain entry: %+v", c.atomics)
	}
	if got, _, _ := resolveAtomics([]byte("stored"), c.atomics); !bytes.Equal(got, append([]byte("stored"), wantChain...)) {
		t.Fatalf("chain over storage = %q", got)
	}
	for i, w := range pinned {
		pe, _ := w.entryOf(set)
		pc, _ := w.entryOf(chain)
		if !bytes.Equal(pe.value, pinnedSet[i]) || len(pc.atomics) != 1 || !bytes.Equal(pc.atomics[0].param, pinnedChain[i]) {
			t.Fatalf("pinned version %d sees %q / %+v", i, pe.value, pc.atomics)
		}
		tx.ryw.unpinWritesLocked(w)
	}
	tx.ryw.mu.Unlock()
	for i := range reads {
		if !bytes.Equal(reads[i], readWant[i]) {
			t.Fatalf("read result %d changed to %q", i, reads[i])
		}
	}

	// Commit's vector comes from the frozen write map itself, so it shares the
	// values the appends below extend: they must write only past its bytes.
	tx.conflictMu.Lock()
	snap := tx.snapshotConflictsLocked()
	muts := tx.mutations
	tx.conflictMu.Unlock()
	ship, _, _, _, coalesced := tx.commitVectors(muts, &snap)
	snap.release(tx)
	want := []Mutation{
		{Type: MutSetValue, Key: set, Value: wantSet},
		{Type: MutAppendIfFits, Key: chain, Value: wantChain},
	}
	if !coalesced {
		t.Fatal("not coalesced")
	}
	assertCoalesceMutationsEqual(t, ship, want)
	shipped := cloneCoalesceMutationsForTest(ship)
	tx.Atomic(MutAppendIfFits, set, []byte("after the snapshot"))
	tx.Atomic(MutAppendIfFits, chain, []byte("after the snapshot"))
	assertCoalesceMutationsEqual(t, ship, shipped)

	// VALUE_SIZE_LIMIT: an append that does not fit leaves the value unchanged
	// (Atomic.h:123-126); one that fills it exactly applies.
	big := &Transaction{}
	stem := bytes.Repeat([]byte{'s'}, valueSizeLimit-60)
	big.Set(set, stem[:1])
	big.Atomic(MutAppendIfFits, set, stem[1:])
	big.Atomic(MutAppendIfFits, chain, stem)
	tail := bytes.Repeat([]byte{'t'}, 50)
	for _, k := range [][]byte{set, chain} {
		big.Atomic(MutAppendIfFits, k, tail)      // valueSizeLimit-10
		big.Atomic(MutAppendIfFits, k, tail[:11]) // over: unchanged
		big.Atomic(MutAppendIfFits, k, tail[:10]) // exactly the limit
		big.Atomic(MutAppendIfFits, k, tail[:1])  // over: unchanged
		big.Atomic(MutAppendIfFits, k, []byte{})  // empty operand: unchanged
	}
	wantBig := append(append(bytes.Clone(stem), tail...), tail[:10]...)
	if v, err := big.Snapshot().Get(ctx, set); err != nil || !bytes.Equal(v, wantBig) {
		t.Fatalf("size limit over Set: %d bytes, %v", len(v), err)
	}
	big.conflictMu.Lock()
	muts = slices.Clone(big.mutations)
	big.conflictMu.Unlock()
	ship, _, _ = coalesceCommitVectors(muts, nil, false)
	assertCoalesceMutationsEqual(t, ship, []Mutation{
		{Type: MutSetValue, Key: set, Value: wantBig},
		{Type: MutAppendIfFits, Key: chain, Value: wantBig},
	})
}

// appendModel is the write map with the pre-ownership fold: every coalesce
// copies existing+operand (doAppendIfFits), as C++ does into its arena.
type appendModel struct {
	kind  map[string]byte // 's' resolved value, 'a' atomic stack, absent: untouched
	value map[string][]byte
	stack map[string][]rywMutation
}

func (m *appendModel) clone() *appendModel {
	out := &appendModel{kind: map[string]byte{}, value: map[string][]byte{}, stack: map[string][]rywMutation{}}
	for k, v := range m.kind {
		out.kind[k] = v
	}
	for k, v := range m.value {
		out.value[k] = bytes.Clone(v)
	}
	for k, s := range m.stack {
		for _, op := range s {
			out.stack[k] = append(out.stack[k], rywMutation{typ: op.typ, param: bytes.Clone(op.param)})
		}
	}
	return out
}

func (m *appendModel) atomic(op MutationType, k string, p []byte, cleared bool) {
	switch m.kind[k] {
	case 's':
		m.value[k], _ = applyAtomic(op, m.value[k], p)
		if m.value[k] == nil {
			m.value[k] = []byte{}
		}
	case 'a':
		s := m.stack[k]
		if top := s[len(s)-1]; top.typ == op {
			r, _ := applyAtomic(op, existing(top.param), p)
			m.stack[k] = append(slices.Clone(s[:len(s)-1]), rywMutation{typ: op, param: r})
		} else {
			m.stack[k] = append(slices.Clone(s), rywMutation{typ: op, param: bytes.Clone(p)})
		}
	default:
		if cleared {
			m.kind[k] = 's'
			m.value[k], _ = applyAtomic(op, nil, p)
			if m.value[k] == nil {
				m.value[k] = []byte{}
			}
			return
		}
		m.kind[k] = 'a'
		m.stack[k] = []rywMutation{{typ: op, param: bytes.Clone(p)}}
	}
}

// check compares version w with the model, key by key.
func (m *appendModel) check(t *testing.T, where string, w *rywWrites, keys []string) {
	t.Helper()
	for _, k := range keys {
		e, ok := w.entryOf([]byte(k))
		switch m.kind[k] {
		case 's':
			if !ok || e.hasAtomics || e.absent || !bytes.Equal(e.value, m.value[k]) {
				t.Fatalf("%s: key %s = %q (ok=%t atomics=%t), want %q", where, k, e.value, ok, e.hasAtomics, m.value[k])
			}
			if e.valueOwned > 0 && cap(e.value) != len(e.value) {
				t.Fatalf("%s: key %s owned value not capped", where, k)
			}
		case 'a':
			if !ok || !e.hasAtomics || len(e.atomics) != len(m.stack[k]) {
				t.Fatalf("%s: key %s stack %+v, want %+v", where, k, e.atomics, m.stack[k])
			}
			for i, op := range e.atomics {
				if op.typ != m.stack[k][i].typ || !bytes.Equal(op.param, m.stack[k][i].param) {
					t.Fatalf("%s: key %s op %d = %d %q, want %d %q", where, k, i, op.typ, op.param, m.stack[k][i].typ, m.stack[k][i].param)
				}
			}
		default:
			if ok {
				t.Fatalf("%s: key %s unexpectedly written", where, k)
			}
		}
	}
}

// TestRYWAppendIfFitsOwnershipRandomized drives appends, Sets, Clears and a
// non-append fold (ByteMax) over a few keys with versions pinned and read
// results scribbled by a caller between operations, and checks the live and
// every pinned version against a copying model, then the commit vector against
// a replay. Operands occasionally cross VALUE_SIZE_LIMIT.
func TestRYWAppendIfFitsOwnershipRandomized(t *testing.T) {
	t.Parallel()
	keys := []string{"k/a", "k/b", "k/c", "k/d"}
	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var c rywCache
		m := (&appendModel{}).clone()
		cleared := map[string]bool{}
		var oplog []Mutation
		type pin struct {
			w     *rywWrites
			model *appendModel
		}
		var pins []pin
		var held []byte // a read result the caller appends to after the next write
		for step := 0; step < 300; step++ {
			k := keys[rng.Intn(len(keys))]
			n := rng.Intn(60)
			if rng.Intn(50) == 0 {
				n = 20000 + rng.Intn(40000)
			}
			p := bytes.Repeat([]byte{byte('a' + step%26)}, n)
			switch r := rng.Intn(20); {
			case r == 0:
				c.set([]byte(k), p)
				m.kind[k], m.value[k] = 's', bytes.Clone(p)
				delete(m.stack, k)
				oplog = append(oplog, Mutation{Type: MutSetValue, Key: []byte(k), Value: bytes.Clone(p)})
			case r == 1:
				c.clear([]byte(k))
				delete(m.kind, k)
				delete(m.value, k)
				delete(m.stack, k)
				cleared[k] = true
				oplog = append(oplog, Mutation{Type: MutClearRange, Key: []byte(k), Value: append([]byte(k), 0)})
			case r == 2:
				c.atomic(MutByteMax, []byte(k), p)
				m.atomic(MutByteMax, k, p, cleared[k])
				oplog = append(oplog, Mutation{Type: MutByteMax, Key: []byte(k), Value: bytes.Clone(p)})
			default:
				c.atomic(MutAppendIfFits, []byte(k), p)
				m.atomic(MutAppendIfFits, k, p, cleared[k])
				oplog = append(oplog, Mutation{Type: MutAppendIfFits, Key: []byte(k), Value: bytes.Clone(p)})
			}
			scribbleCallerBuf(p)
			scribbled := append(held, "caller scribble"...)
			scribbleCallerBuf(scribbled[len(held):])
			c.mu.Lock()
			live := c.liveLocked()
			m.check(t, fmt.Sprintf("seed %d step %d live", seed, step), live, keys)
			held = nil
			if v, known, err := c.localPointLocked(live, []byte(k)); err == nil && known {
				held = v
			}
			if rng.Intn(4) == 0 {
				pins = append(pins, pin{c.pinWritesLocked(), m.clone()})
			}
			if len(pins) > 0 && rng.Intn(6) == 0 {
				i := rng.Intn(len(pins))
				pins[i].model.check(t, fmt.Sprintf("seed %d step %d pinned", seed, step), pins[i].w, keys)
				c.unpinWritesLocked(pins[i].w)
				pins = slices.Delete(pins, i, i+1)
			}
			c.mu.Unlock()
		}
		c.mu.Lock()
		for i, p := range pins {
			p.model.check(t, fmt.Sprintf("seed %d final pinned %d", seed, i), p.w, keys)
			c.unpinWritesLocked(p.w)
		}
		c.mu.Unlock()
		assertCoalesceMutationsEqual(t, c.materializeCommit(), coalesceCommitMutations(oplog))
	}
}

// TestAtomicsOutcomeMatchesResolve pins atomicsOutcome to resolveAtomics's
// flags over random chains, bases and CompareAndClear operands that match.
func TestAtomicsOutcomeMatchesResolve(t *testing.T) {
	t.Parallel()
	ops := []MutationType{
		MutAppendIfFits, MutCompareAndClear, MutAddValue, MutByteMax, MutAndV2,
		MutMinV2, MutSetVersionstampedValue, MutSetVersionstampedKey,
	}
	operands := [][]byte{nil, {}, []byte("a"), []byte("ab"), {1, 0, 0, 0, 0, 0, 0, 0}}
	rng := rand.New(rand.NewSource(1))
	matched := 0
	for i := 0; i < 20000; i++ {
		base := operands[rng.Intn(len(operands))]
		chain := make([]rywMutation, rng.Intn(4))
		for j := range chain {
			chain[j] = rywMutation{typ: ops[rng.Intn(len(ops))], param: operands[rng.Intn(len(operands))]}
		}
		_, wantCleared, wantUnresolved := resolveAtomics(base, chain)
		cleared, unresolved := atomicsOutcome(base, chain)
		if cleared != wantCleared || unresolved != wantUnresolved {
			t.Fatalf("base %q chain %+v: got (%t,%t), want (%t,%t)", base, chain, cleared, unresolved, wantCleared, wantUnresolved)
		}
		if wantCleared {
			matched++
		}
	}
	if matched == 0 {
		t.Fatal("no chain cleared: the CompareAndClear arm was not exercised")
	}
}
