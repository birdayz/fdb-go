package client

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestRYWAtomicSkippedMutateKeepsNodes checks the write map's nodes against a replay that
// applies every WriteMap::mutate (WriteMap.cpp:87-158): a skipped mutate changes no node.
func TestRYWAtomicSkippedMutateKeepsNodes(t *testing.T) {
	t.Parallel()
	keys := []string{"a", "b", "b\x00", "c", "d"}
	skipped := 0
	for seed := int64(1); seed <= 3000; seed++ {
		rng := rand.New(rand.NewSource(seed))
		c := &rywCache{}
		var ops []rywNodeOp
		var log []string
		var pins []*rywWrites
		key := func() string { return keys[rng.Intn(len(keys))] }
		rangeOf := func() (string, string) {
			a, b := key(), key()
			if a > b {
				a, b = b, a
			}
			if a == b {
				b = a + "\x00"
			}
			return a, b
		}
		for i, n := 0, 2+rng.Intn(20); i < n; i++ {
			conflict := rng.Intn(4) != 0
			switch r := rng.Intn(12); {
			case r < 5:
				k := key()
				op := []MutationType{MutAppendIfFits, MutAddValue, MutAppendIfFits, MutSetVersionstampedValue, MutCompareAndClear}[rng.Intn(5)]
				before := len(c.liveNodesLog())
				c.atomicWith(op, []byte(k), []byte{1}, conflict)
				if !c.liveNodesBuilt() && len(c.liveNodesLog()) == before {
					skipped++
				}
				ops = append(ops, nodeMut(k, conflict, isUnresolvedVersionstamp(op)))
				log = append(log, fmt.Sprintf("atomic%d(%q,%t)", op, k, conflict))
			case r < 7:
				k := key()
				c.setWith([]byte(k), []byte("v"), conflict)
				ops = append(ops, nodeMut(k, conflict, false))
				log = append(log, fmt.Sprintf("set(%q,%t)", k, conflict))
			case r == 7:
				k := key()
				c.clearWith([]byte(k), conflict)
				ops = append(ops, nodeClear(k, k+"\x00", conflict))
				log = append(log, fmt.Sprintf("clear(%q,%t)", k, conflict))
			case r == 8:
				b, e := rangeOf()
				c.clearRangeWith([]byte(b), []byte(e), conflict)
				ops = append(ops, nodeClear(b, e, conflict))
				log = append(log, fmt.Sprintf("clearRange(%q,%q,%t)", b, e, conflict))
			case r == 9:
				b, e := rangeOf()
				c.addConflictRange([]byte(b), []byte(e))
				ops = append(ops, nodeConflict(b, e))
				log = append(log, fmt.Sprintf("conflict(%q,%q)", b, e))
			case r == 10:
				// SetVersionstampedKey: the candidate range, then the op at its min key
				// without a conflict (RYW:2264-2272).
				b, e := rangeOf()
				c.addUnreadableRange([]byte(b), []byte(e))
				c.atomicWith(MutSetVersionstampedKey, []byte(b), make([]byte, 4), false)
				ops = append(ops, nodeUnreadable(b, e), nodeMut(b, false, true))
				log = append(log, fmt.Sprintf("svk(%q,%q)", b, e))
			default:
				c.mu.Lock()
				pins = append(pins, c.pinWritesLocked())
				c.mu.Unlock()
				log = append(log, "pin")
			}
		}
		c.mu.Lock()
		w := c.liveLocked()
		w.nodes.build()
		got := w.nodes.dump()
		for _, p := range pins {
			c.unpinWritesLocked(p)
		}
		c.mu.Unlock()
		if want := replayNodes(ops, false).dump(); got != want {
			t.Fatalf("seed %d history %s:\nnodes:\n%s\nreplay:\n%s", seed, strings.Join(log, " "), got, want)
		}
	}
	if skipped == 0 {
		t.Fatal("no mutate was skipped: the histories never exercised the skip")
	}
}

func (c *rywCache) liveNodesLog() []rywNodeOp {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveLocked().nodes.log
}

func (c *rywCache) liveNodesBuilt() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveLocked().nodes.built
}

// TestRYWAtomicKeyNotRetained checks that atomicWith keeps no view of the caller's key
// or operand, on the unsorted and the sorted write map, across same-type folds.
func TestRYWAtomicKeyNotRetained(t *testing.T) {
	t.Parallel()
	for _, sorted := range []bool{false, true} {
		for _, op := range []MutationType{MutAppendIfFits, MutAddValue, MutByteMax} {
			t.Run(fmt.Sprintf("sorted=%t/op=%d", sorted, op), func(t *testing.T) {
				t.Parallel()
				c := &rywCache{}
				c.set([]byte("key-set"), []byte("v0")) // folds at site B; key-chain folds its stack
				if sorted {
					c.mu.Lock()
					c.pinWritesLocked()
					c.mu.Unlock()
				}
				kbuf, pbuf := make([]byte, 0, 16), make([]byte, 8)
				wantSet, wantChain := []byte("v0"), []byte(nil)
				for i := 0; i < 4; i++ {
					for j := range pbuf {
						pbuf[j] = byte((4-i)*37 + j) // decreasing, so ByteMax keeps the first operand
					}
					p := bytes.Clone(pbuf)
					wantSet, _ = applyAtomic(op, wantSet, p)
					if wantChain == nil {
						wantChain = p
					} else {
						wantChain, _ = applyAtomic(op, existing(wantChain), p)
					}
					kbuf = append(kbuf[:0], "key-set"...)
					c.atomic(op, kbuf, pbuf)
					kbuf = append(kbuf[:0], "key-chain"...)
					c.atomic(op, kbuf, pbuf)
					clear(pbuf)
				}
				clear(kbuf)
				c.mu.Lock()
				defer c.mu.Unlock()
				w := c.liveLocked()
				if w.isSorted != sorted {
					t.Fatalf("write map sorted=%t, want %t", w.isSorted, sorted)
				}
				e, ok := w.entryOf([]byte("key-set"))
				if !ok || e.hasAtomics || !bytes.Equal(e.value, wantSet) {
					t.Fatalf("key-set entry %+v (found %t), want value %x", e, ok, wantSet)
				}
				e, ok = w.entryOf([]byte("key-chain"))
				if !ok || len(e.atomics) != 1 || !bytes.Equal(e.atomics[0].param, wantChain) {
					t.Fatalf("key-chain entry %+v (found %t), want one operand %x", e, ok, wantChain)
				}
				if n := w.numWrites(); n != 2 {
					t.Fatalf("%d writes, want 2: the write map aliases the caller's key", n)
				}
				w.nodes.build()
				if d := w.nodes.dump(); !strings.Contains(d, `"key-chain"`) || !strings.Contains(d, `"key-set"`) {
					t.Fatalf("nodes alias the caller's key:\n%s", d)
				}
			})
		}
	}
}
