package client

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

// RFC-262 S4: pin lifecycle and version immutability. Run with -race.

// fingerprint is a deep, deterministic dump of a version: entries with their
// atomic stacks and bytes, the slices, the flags and the nodes. The lazy derived
// indexes (sortedKeys, the node build) are built first, so whether a reader has
// built them does not change it.
func (w *rywWrites) fingerprint() string {
	w.ensureSorted()
	var b strings.Builder
	keys := make([]string, 0, len(w.writes))
	for k := range w.writes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := w.writes[k]
		fmt.Fprintf(&b, "%q=%q abs=%t dep=%t atom=%t unr=%t [", k, e.value, e.absent, e.dependent, e.hasAtomics, e.unreadable)
		for _, m := range e.atomics {
			fmt.Fprintf(&b, "%d:%x ", m.typ, m.param)
		}
		b.WriteString("]\n")
	}
	fmt.Fprintf(&b, "sorted=%q\ncleared=", w.sortedKeys)
	for _, r := range w.cleared {
		fmt.Fprintf(&b, "[%q,%q)", r.begin, r.end)
	}
	b.WriteString("\nunreadable=")
	for _, r := range w.unreadableRanges {
		fmt.Fprintf(&b, "[%q,%q)", r.begin, r.end)
	}
	fmt.Fprintf(&b, "\nunreadableKeys=%q bypass=%t\n", w.unreadableKeys, w.bypassUnreadable)
	w.nodes.build()
	b.WriteString(w.nodes.dump())
	return b.String()
}

func pinNum(n uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, n)
	return b
}

// pinTracker records every pin, unpin and clone of one cache.
type pinTracker struct {
	c       *rywCache
	mu      sync.Mutex
	pins    map[*rywWrites]int
	unpins  map[*rywWrites]int
	atPin   map[*rywWrites]string
	bad     []string
	clones  int
	current map[*rywWrites]int
}

func trackPins(c *rywCache) *pinTracker {
	p := &pinTracker{c: c, pins: map[*rywWrites]int{}, unpins: map[*rywWrites]int{}, atPin: map[*rywWrites]string{}, current: map[*rywWrites]int{}}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onPin = func(w *rywWrites, pin bool) { // under c.mu
		p.mu.Lock()
		defer p.mu.Unlock()
		fp := w.fingerprint()
		if pin {
			p.pins[w]++
			// A version is immutable while some read pins it; once unpinned and live it
			// may change, so the reference fingerprint starts at each 0 → 1 pin.
			if p.current[w] > 0 && p.atPin[w] != fp {
				p.bad = append(p.bad, "a version changed while pinned (seen at a later pin)")
			}
			if p.current[w] == 0 {
				p.atPin[w] = fp
			}
			p.current[w]++
			return
		}
		p.unpins[w]++
		p.current[w]--
		if p.atPin[w] != fp {
			p.bad = append(p.bad, "a version changed while pinned (seen at release)")
		}
	}
	c.onClone = func(_, _ *rywWrites) { p.mu.Lock(); p.clones++; p.mu.Unlock() }
	return p
}

// quiesced checks every pinned version was released exactly as often as pinned.
func (p *pinTracker) cloneCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clones
}

func (p *pinTracker) quiesced(t *testing.T) {
	t.Helper()
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.bad {
		t.Error(b)
	}
	for w, n := range p.pins {
		if p.unpins[w] != n || w.pins != 0 {
			t.Errorf("version pinned %d times, released %d, pins now %d", n, p.unpins[w], w.pins)
		}
	}
}

// gatedStorage answers from memStorage after its gate opens; it counts calls.
type gatedStorage struct {
	m     *memStorage
	gate  chan struct{}
	calls atomic.Int32
	mu    sync.Mutex
}

func newGatedStorage(keys ...string) *gatedStorage {
	return &gatedStorage{m: newMemStorage(keys...), gate: make(chan struct{})}
}

func (g *gatedStorage) native(ctx context.Context, begin, end KeySelector, limits rangeLimits, reverse bool) (nativeRangeResult, error) {
	g.calls.Add(1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nativeRangeResult{}, ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.m.native(ctx, begin, end, limits, reverse)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// joined runs fn and fails the test if it does not return before the deadline.
func joined(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
}

func fgtSel(k string) KeySelector { return KeySelector{Key: []byte(k), OrEqual: true, Offset: 1} }

func pinsOf(c *rywCache, w *rywWrites) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return w.pins
}

func liveOf(c *rywCache) *rywWrites {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveLocked()
}

// S4.1: two reads issued back to back share the version; a Set clones once; both
// reads finish with their issue results; pins return to 0.
func TestRYWPinSharedThenCloned(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	g := newGatedStorage("a", "c")
	tx.nativeRange = g.native
	tr := trackPins(&tx.ryw)
	issued := liveOf(&tx.ryw)
	p1 := tx.StartGetKey(context.Background(), fgtSel("a"))
	p2 := tx.StartGetKey(context.Background(), fgtSel("a"))
	if n := pinsOf(&tx.ryw, issued); n != 2 {
		t.Fatalf("pins %d, want 2 (both reads share the version)", n)
	}
	tx.Set([]byte("b"), []byte("x"))
	if liveOf(&tx.ryw) == issued || tr.cloneCount() != 1 {
		t.Fatalf("the Set did not clone the pinned version once (clones %d)", tr.cloneCount())
	}
	tx.Set([]byte("b0"), []byte("x"))
	if tr.cloneCount() != 1 {
		t.Fatalf("an unpinned live version was cloned again (clones %d)", tr.cloneCount())
	}
	close(g.gate)
	joined(t, "the reads", func() {
		for _, p := range []*PendingKey{p1, p2} {
			k, err := p.Get()
			if err != nil || string(k) != "c" {
				t.Errorf("read = %q, %v; want c (the Set after issue is not visible)", k, err)
			}
		}
	})
	tr.quiesced(t)
}

// S4.2: a writer folding a same-type atomic over a pinned stack leaves the pinned
// version unchanged.
func TestRYWPinnedAtomicStackUnchanged(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	g := newGatedStorage("a", "c")
	tx.nativeRange = g.native
	tr := trackPins(&tx.ryw)
	tx.Atomic(MutAddValue, []byte("k"), pinNum(1))
	pinned := liveOf(&tx.ryw)
	p := tx.StartGetKey(context.Background(), fgtSel("a"))
	tx.ryw.mu.Lock()
	before := pinned.fingerprint()
	tx.ryw.mu.Unlock()
	tx.Atomic(MutAddValue, []byte("k"), pinNum(2)) // coalesceOverAtomics rewrites the top
	tx.ryw.mu.Lock()
	after := pinned.fingerprint()
	live := tx.ryw.liveLocked().writes["k"]
	tx.ryw.mu.Unlock()
	if before != after {
		t.Fatalf("pinned version changed:\n%s\n%s", before, after)
	}
	if len(live.atomics) != 1 || string(live.atomics[0].param) == string(pinNum(1)) {
		t.Fatalf("live stack %v, want the folded operand", live.atomics)
	}
	close(g.gate)
	joined(t, "the read", func() { _, _ = p.Get() })
	tr.quiesced(t)
}

// S4.3: the old range loop's mergeBatchLocked resolves atomics only into its private
// span while a getKey walk pins the live version; the shared versions are rejected.
func TestRYWMergeBatchOnlyOnPrivateSpan(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	g := newGatedStorage("a", "c")
	tx.nativeRange = g.native
	tr := trackPins(&tx.ryw)
	tx.Atomic(MutAddValue, []byte("k"), pinNum(1))
	p := tx.StartGetKey(context.Background(), fgtSel("a"))
	c := &tx.ryw
	c.mu.Lock()
	pinned := c.liveLocked()
	before := pinned.fingerprint()
	span := c.captureWriteSpanLocked([]byte(""), []byte("\xff"))
	c.mu.Unlock()
	rows := c.mergeBatch(span, []KeyValue{{Key: []byte("k"), Value: pinNum(5)}}, []byte(""), []byte("\xff"), nil, false)
	if keysOf(rows) != "k" {
		t.Fatalf("merge rows %q", keysOf(rows))
	}
	c.mu.Lock()
	after := pinned.fingerprint()
	c.mu.Unlock()
	if before != after {
		t.Fatalf("mergeBatch changed the pinned live version")
	}
	if !span.writes["k"].hasAtomics && span.writes["k"].value == nil {
		t.Fatalf("the span did not take the merge")
	}
	for name, w := range map[string]*rywWrites{"live": c.w, "pinned": pinned} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("mergeBatchLocked accepted the %s version", name)
				}
			}()
			c.mu.Lock()
			defer c.mu.Unlock()
			c.mergeBatchLocked(w, nil, []byte(""), []byte("\xff"), nil, false)
		}()
	}
	close(g.gate)
	joined(t, "the read", func() { _, _ = p.Get() })
	tr.quiesced(t)
}

// S4.4: a reset while the walk awaits storage. The cache's reset (the RYW half of
// Transaction.reset) lands between the request and its reply: the reply is not
// inserted into the new cache and the walk fails; the owner's pin is released once.
// Through Transaction.Reset, which cancels the read's incarnation and waits for it,
// the read fails and its pin is released once.
func TestRYWResetWhileAwaitingStorage(t *testing.T) {
	t.Parallel()
	t.Run("cache-reset-before-reply", func(t *testing.T) {
		t.Parallel()
		c := &rywCache{}
		tr := trackPins(c)
		c.set([]byte("b"), []byte("x"))
		m := newMemStorage("a", "c")
		c.mu.Lock()
		view := c.pinWritesLocked()
		w := c.newGetKeyWalkLocked(view, fgtSel("a"), []byte("\xff"))
		w.startLocked()
		c.mu.Unlock()
		native := func(ctx context.Context, b, e KeySelector, l rangeLimits, rev bool) (nativeRangeResult, error) {
			c.reset() // the transaction resets while the request is in flight
			return m.answer(walkRequest{begin: b, end: e, limits: l}, rev), nil
		}
		if err := w.run(context.Background(), native); !errors.Is(err, context.Canceled) {
			t.Fatalf("walk err %v, want the reset's cancellation", err)
		}
		c.mu.Lock()
		dump := c.cacheDump()
		c.unpinWritesLocked(view)
		c.mu.Unlock()
		if dump != "" {
			t.Fatalf("the late reply reached the new cache: %s", dump)
		}
		tr.quiesced(t)
	})
	t.Run("transaction-reset", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		var calls atomic.Int32
		tx.nativeRange = func(ctx context.Context, _, _ KeySelector, _ rangeLimits, _ bool) (nativeRangeResult, error) {
			calls.Add(1)
			<-ctx.Done()
			return nativeRangeResult{}, ctx.Err()
		}
		tr := trackPins(&tx.ryw)
		p := tx.StartGetKey(context.Background(), fgtSel("a"))
		waitFor(t, "the storage request", func() bool { return calls.Load() == 1 })
		joined(t, "Reset", tx.Reset)
		joined(t, "the read", func() {
			if k, err := p.Get(); err == nil {
				t.Errorf("read after reset = %q, nil; want an error", k)
			}
		})
		tr.quiesced(t)
		if len(tr.pins) != 1 {
			t.Fatalf("pinned %d versions, want 1", len(tr.pins))
		}
	})
}

// S4.5: cancellation while a nested getKey awaits storage (S1 case 7 shape): the
// walk fails, the owner's single pin is released once, the sub walk never pins.
func TestRYWCancelDuringNestedGetKey(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	tr := trackPins(c)
	c.set([]byte("m"), []byte("x"))
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	view := c.pinWritesLocked() // the owner
	// begin FGE(n) > end (c, false, 5): P2 does not apply (offsets 1 < 5), so P3
	// resolves end with a nested getKey (RYW:621).
	w := c.newRangeWalkLocked(view, firstGreaterOrEqual([]byte("n")), KeySelector{Key: []byte("c"), Offset: 5}, rangeLimits{rows: -1, minRows: 1, bytes: -1}, false, []byte("\xff"))
	w.startLocked()
	c.mu.Unlock()
	if w.pc != pcAwaitPreKey || w.sub == nil || w.sub.pc != pcAwaitStorage {
		t.Fatalf("walk pc %d, want a nested getKey awaiting storage", w.pc)
	}
	started := make(chan struct{})
	native := func(ctx context.Context, _, _ KeySelector, _ rangeLimits, _ bool) (nativeRangeResult, error) {
		close(started)
		<-ctx.Done()
		return nativeRangeResult{}, ctx.Err()
	}
	errc := make(chan error, 1)
	go func() { errc <- w.run(ctx, native) }()
	<-started
	if n := pinsOf(c, view); n != 1 {
		t.Fatalf("pins %d during the nested read, want the owner's 1", n)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("walk err %v, want cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("walk did not end")
	}
	c.mu.Lock()
	c.unpinWritesLocked(view) // the owner's complete()
	c.mu.Unlock()
	tr.quiesced(t)
}

// S4.6: concurrent getKeys (Get called or not, contexts cancelled) and writes; after
// quiescence every version was released exactly as often as pinned and never
// changed while pinned. Range reads use the old loop in phase 2 and pin nothing.
func TestRYWPinStress(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	var smu sync.Mutex
	m := newMemStorage("a", "b", "c", "d", "e", "f", "g")
	tx.nativeRange = func(ctx context.Context, b, e KeySelector, l rangeLimits, rev bool) (nativeRangeResult, error) {
		time.Sleep(time.Duration(len(b.Key)%3) * 100 * time.Microsecond)
		if err := ctx.Err(); err != nil {
			return nativeRangeResult{}, err
		}
		smu.Lock()
		defer smu.Unlock()
		return m.answer(walkRequest{begin: b, end: e, limits: l}, rev), nil
	}
	tr := trackPins(&tx.ryw)
	var wg sync.WaitGroup
	var pending sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := string(rune('a' + (g+i)%7))
				switch i % 5 {
				case 0:
					tx.Set([]byte(k+"1"), []byte("x"))
				case 1:
					tx.Clear([]byte(k))
				case 2:
					tx.Atomic(MutAddValue, []byte(k+"2"), pinNum(1))
				default:
					ctx, cancel := context.WithCancel(context.Background())
					p := tx.StartGetKey(ctx, KeySelector{Key: []byte(k), OrEqual: i%2 == 0, Offset: int32(i%4) - 1})
					if i%3 == 0 {
						cancel()
					}
					if i%2 == 0 {
						_, _ = p.Get()
						cancel()
					} else {
						pending.Add(1)
						go func() { defer pending.Done(); defer cancel(); _, _ = p.Get() }()
					}
				}
			}
		}(g)
	}
	joined(t, "the workers", func() { wg.Wait(); pending.Wait() })
	tr.quiesced(t)
}

// S4.7: a panicking reply hook fails the read through the backstop; the pin is
// released once.
func TestRYWPanicReleasesPin(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	m := newMemStorage("a", "c")
	tx.nativeRange = m.native
	tx.afterRangeReply = func() { panic("injected") }
	tr := trackPins(&tx.ryw)
	_, err := tx.StartGetKey(context.Background(), fgtSel("a")).Get()
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != ErrInternalError {
		t.Fatalf("err %v, want internal_error", err)
	}
	tr.quiesced(t)
	if len(tr.pins) != 1 {
		t.Fatalf("pinned %d versions, want 1", len(tr.pins))
	}
}

// S4.8: every mutator clones a pinned version and leaves it unchanged (D1).
func TestRYWEveryMutatorClonesPinned(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(c *rywCache){
		"set":              func(c *rywCache) { c.setWith([]byte("k"), []byte("v"), true) },
		"clear":            func(c *rywCache) { c.clearWith([]byte("k"), true) },
		"clearRange":       func(c *rywCache) { c.clearRangeWith([]byte("a"), []byte("z"), true) },
		"atomic":           func(c *rywCache) { c.atomicWith(MutAddValue, []byte("k"), pinNum(1), true) },
		"versionstamp":     func(c *rywCache) { c.atomicWith(MutSetVersionstampedValue, []byte("v"), make([]byte, 14), true) },
		"addUnreadable":    func(c *rywCache) { c.addUnreadableRange([]byte("s"), []byte("t")) },
		"addClearedRange":  func(c *rywCache) { c.addClearedRange([]byte("q"), []byte("r")) },
		"bypassUnreadable": func(c *rywCache) { c.setBypassUnreadable(true) },
		"conflictRange":    func(c *rywCache) { c.addConflictRange([]byte("m"), []byte("n")) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &rywCache{}
			c.set([]byte("k"), []byte("old"))
			c.atomic(MutAddValue, []byte("j"), pinNum(1))
			tr := trackPins(c)
			c.mu.Lock()
			w := c.pinWritesLocked()
			before := w.fingerprint()
			c.mu.Unlock()
			mutate(c)
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.w == w || tr.cloneCount() != 1 {
				t.Fatalf("%s did not clone the pinned version (clones %d)", name, tr.cloneCount())
			}
			if after := w.fingerprint(); after != before {
				t.Fatalf("%s changed the pinned version:\n%s\n%s", name, before, after)
			}
			if c.w.fingerprint() == before {
				t.Fatalf("%s changed nothing in the live version", name)
			}
			c.unpinWritesLocked(w)
		})
	}
}

// S4.9: concurrent and repeated Get on one issued read run it once, return one
// result and release the pin once.
func TestRYWRepeatedGetRunsOnce(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	g := newGatedStorage("a", "c")
	tx.nativeRange = g.native
	tr := trackPins(&tx.ryw)
	p := tx.StartGetKey(context.Background(), fgtSel("a"))
	var wg sync.WaitGroup
	keys := make([]string, 8)
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := p.Get()
			if err != nil {
				t.Error(err)
			}
			keys[i] = string(k)
		}()
	}
	close(g.gate)
	joined(t, "the getters", wg.Wait)
	k, err := p.Get()
	keys = append(keys, string(k))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k != "c" {
			t.Fatalf("results %q, want c from every Get", keys)
		}
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("storage called %d times, want 1", n)
	}
	tr.quiesced(t)
}

// BenchmarkRYWPinnedClone is D1's bad case: with 10k pending writes, every Set
// lands while a read pins the version, so each Set clones it (O(writes)). The
// unpinned variant is the same Set without a pin.
func BenchmarkRYWPinnedClone(b *testing.B) {
	for _, pinned := range []bool{false, true} {
		b.Run(fmt.Sprintf("pinned=%t", pinned), func(b *testing.B) {
			c := &rywCache{}
			for i := 0; i < 10000; i++ {
				c.set([]byte(fmt.Sprintf("k%05d", i)), []byte("v"))
			}
			c.mu.Lock()
			c.liveLocked().nodes.build()
			c.liveLocked().ensureSorted()
			c.mu.Unlock()
			key := []byte("k05000")
			b.ReportAllocs()
			for b.Loop() {
				var w *rywWrites
				if pinned {
					c.mu.Lock()
					w = c.pinWritesLocked()
					c.mu.Unlock()
				}
				c.set(key, []byte("x"))
				if pinned {
					c.mu.Lock()
					c.unpinWritesLocked(w)
					c.mu.Unlock()
				}
			}
		})
	}
}

// S1 case 14 (hermetic part): a getKey answered from the write map takes no read
// version. The transaction has no database, so a GRV or a storage read would fail.
func TestRYWLocalGetKeyTakesNoReadVersion(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.Set([]byte("k"), []byte("v"))
	tx.ClearRange([]byte("k\x00"), []byte("m"))
	for _, sel := range []KeySelector{firstGreaterOrEqual([]byte("k")), {Key: []byte("k"), OrEqual: true, Offset: 0}} {
		k, err := tx.GetKey(context.Background(), sel.Key, sel.OrEqual, sel.Offset)
		if err != nil || string(k) != "k" {
			t.Fatalf("GetKey(%s) = %q, %v; want k from the write map", selString(sel), k, err)
		}
	}
	tx.readVersionMu.Lock()
	has := tx.hasReadVersion
	tx.readVersionMu.Unlock()
	if has {
		t.Fatal("a locally answered getKey acquired a read version")
	}
}

// S1 case 13 for StartGetKey: every later mutation kind between issue and
// completion leaves the requests and the result equal to the run without it; 60
// later conflict ranges do not split the request (T6 shape).
func TestRYWStartGetKeyIssueTimeIsolation(t *testing.T) {
	t.Parallel()
	later := map[string]func(tx *Transaction){
		"none":         func(*Transaction) {},
		"set":          func(tx *Transaction) { tx.Set([]byte("b5"), []byte("x")) },
		"clear":        func(tx *Transaction) { tx.Clear([]byte("c")) },
		"clearRange":   func(tx *Transaction) { _ = tx.ClearRange([]byte("b"), []byte("d")) },
		"atomic":       func(tx *Transaction) { tx.Atomic(MutAddValue, []byte("b5"), pinNum(1)) },
		"versionstamp": func(tx *Transaction) { tx.Atomic(MutSetVersionstampedValue, []byte("b5"), make([]byte, 14)) },
		"bypass":       func(tx *Transaction) { tx.ryw.setBypassUnreadable(true) },
		"conflicts": func(tx *Transaction) {
			for i := 0; i < 60; i++ {
				k := fmt.Sprintf("c%02d", i)
				_ = tx.stateAddWriteConflictRange([]byte(k), []byte(k+"\x00"))
			}
		},
	}
	run := func(mutate func(tx *Transaction)) (string, string) {
		tx := &Transaction{}
		g := newGatedStorage("a", "b", "c", "d", "e")
		tx.nativeRange = g.native
		tx.Set([]byte("b1"), []byte("w"))
		p := tx.StartGetKey(context.Background(), KeySelector{Key: []byte("a"), OrEqual: false, Offset: 3})
		mutate(tx)
		close(g.gate)
		k, err := p.Get()
		if err != nil {
			t.Fatal(err)
		}
		var reqs []string
		for _, r := range g.m.requests {
			reqs = append(reqs, reqString(r, false))
		}
		return string(k), strings.Join(reqs, "\n")
	}
	wantKey, wantReqs := run(later["none"])
	for name, mutate := range later {
		k, reqs := run(mutate)
		if k != wantKey || reqs != wantReqs {
			t.Errorf("%s after issue: key %q requests\n%s\nwant %q\n%s", name, k, reqs, wantKey, wantReqs)
		}
	}
}
