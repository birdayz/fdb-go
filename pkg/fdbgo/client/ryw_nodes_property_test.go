package client

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// RFC-262 D1b property test: after random histories of every write-map mutator,
// with and without conflicts, the node-derived write segments agree with the
// stores, which stay authoritative for values and types: an operation segment
// has a writes entry, a range segment holds no entry, followingCleared matches
// isCleared, and the unreadable flags match the unreadable checks. RFC-098's
// deliberate divergence is excluded by its own argument: inside an SVK range Go
// keeps pending writes and clears that C++ removes, and those segments are
// unreadable in both, a barrier or an error at their edge (A4, A6 L7).
func TestRYWNodesAgreeWithStores(t *testing.T) {
	t.Parallel()
	keys := []string{"", "a", "a\x00", "b", "b\x00", "b5", "c", "c\x00", "c\x00\x00", "d", "e"}
	rng := rand.New(rand.NewSource(262))
	// Every mutator endpoint and its keyAfter is a probe in this independent
	// bitmap. No node flags or stores are used to derive the expected conflicts.
	var probes []string
	for _, k := range keys {
		probes = append(probes, k, k+"\x00")
	}
	slices.Sort(probes)
	probes = slices.Compact(probes)
	var conflicted, unconflicted int
	for h := 0; h < 2000; h++ {
		c := &rywCache{}
		bitmap := make([]bool, len(probes))
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
		for i, n := 0, 1+rng.Intn(12); i < n; i++ {
			conflict := rng.Intn(3) != 0
			var cb, ce []byte
			var resetConflict bool
			switch rng.Intn(8) {
			case 0:
				k := key()
				cb, ce = k, keyAfterBytes(k)
				c.setWith(k, []byte("v"), conflict)
				log = append(log, fmt.Sprintf("set(%q,%t)", k, conflict))
			case 1:
				k := key()
				cb, ce = k, keyAfterBytes(k)
				c.clearWith(k, conflict)
				log = append(log, fmt.Sprintf("clear(%q,%t)", k, conflict))
			case 2:
				b, e := rangeOf()
				cb, ce = b, e
				c.clearRangeWith(b, e, conflict)
				log = append(log, fmt.Sprintf("clearRange(%q,%q,%t)", b, e, conflict))
			case 3:
				k := key()
				cb, ce = k, keyAfterBytes(k)
				c.atomicWith(MutAddValue, k, []byte{1}, conflict)
				log = append(log, fmt.Sprintf("add(%q,%t)", k, conflict))
			case 4:
				k := key()
				cb, ce = k, keyAfterBytes(k)
				c.atomicWith(MutSetVersionstampedValue, k, make([]byte, 14), conflict)
				log = append(log, fmt.Sprintf("svv(%q,%t)", k, conflict))
			case 5:
				// SetVersionstampedKey: the candidate range, then the op at its min key
				// without a conflict (RYW:2264-2272).
				b, e := rangeOf()
				cb, ce = b, e
				resetConflict = true // WriteMap::addUnmodifiedAndUnreadableRange clears conflicts
				c.addUnreadableRange(b, e)
				c.atomicWith(MutSetVersionstampedKey, b, make([]byte, 4), false)
				log = append(log, fmt.Sprintf("svk(%q,%q)", b, e))
			case 6:
				b, e := rangeOf()
				cb, ce = b, e
				conflict = true
				c.addConflictRange(b, e)
				log = append(log, fmt.Sprintf("conflict(%q,%q)", b, e))
			case 7:
				k := key()
				cb, ce = k, keyAfterBytes(k)
				c.atomicWith(MutCompareAndClear, k, []byte("v"), conflict)
				log = append(log, fmt.Sprintf("cac(%q,%t)", k, conflict))
			}
			for j, p := range probes {
				if bytes.Compare([]byte(p), cb) >= 0 && bytes.Compare([]byte(p), ce) < 0 {
					if resetConflict {
						bitmap[j] = false
					} else if conflict {
						bitmap[j] = true
					}
				}
			}
		}
		c.mu.Lock()
		w := c.liveLocked()
		if err := nodesAgree(w); err != nil {
			c.mu.Unlock()
			t.Fatalf("history %s:\n%v\nnodes:\n%s", strings.Join(log, " "), err, w.nodes.dump())
		}
		for i, p := range probes {
			if got := w.nodes.segmentAt([]byte(p)).conflict; got != bitmap[i] {
				c.mu.Unlock()
				t.Fatalf("history %s: conflict at %q = %t, bitmap says %t", strings.Join(log, " "), p, got, bitmap[i])
			}
			if bitmap[i] {
				conflicted++
			} else {
				unconflicted++
			}
		}
		c.mu.Unlock()
	}
	if conflicted == 0 || unconflicted == 0 {
		t.Fatalf("vacuous conflict bitmap: %d conflicted, %d unconflicted probes", conflicted, unconflicted)
	}
}

func nodesAgree(w *rywWrites) error {
	p := []byte{}
	for bytes.Compare(p, allKeysEnd) < 0 {
		seg := w.nodes.segmentAt(p)
		b, e := seg.begin.materialize(), seg.end.materialize()
		entry, hasEntry := w.writes[string(b)]
		if seg.op {
			if !hasEntry {
				return fmt.Errorf("operation segment [%q,%q) has no writes entry", b, e)
			}
			if want := entry.unreadable || w.isUnreadable(b); seg.unreadable != want {
				return fmt.Errorf("operation segment %q unreadable %t, stores say %t", b, seg.unreadable, want)
			}
		} else {
			for k := range w.writes {
				if bytes.Compare([]byte(k), b) >= 0 && bytes.Compare([]byte(k), e) < 0 && !w.isUnreadable([]byte(k)) {
					return fmt.Errorf("range segment [%q,%q) holds the entry %q", b, e, k)
				}
			}
			// Inside an SVK range C++ makes the span unmodified (WriteMap.cpp:205-242) while
			// Go keeps its cleared list there (RFC-098); unreadability decides the type
			// first in both (segTypeAtLocked), so cleared is compared only outside it.
			if !w.isUnreadable(b) && seg.cleared != w.isCleared(b) {
				return fmt.Errorf("range segment [%q,%q) cleared %t, stores say %t", b, e, seg.cleared, w.isCleared(b))
			}
			if seg.unreadable != w.isUnreadable(b) {
				return fmt.Errorf("range segment [%q,%q) unreadable %t, stores say %t", b, e, seg.unreadable, w.isUnreadable(b))
			}
		}
		if bytes.Compare(e, p) <= 0 {
			return fmt.Errorf("segment at %q does not advance", p)
		}
		p = e
	}
	return nil
}
