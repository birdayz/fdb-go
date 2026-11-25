package client

import (
	"bytes"
	"context"
	"sync"

	"fdb.dev/internal/fdbwire"
)

// rywWalk is C++ getRangeValue / getRangeValueBack (ReadYourWrites.actor.cpp:592-813,
// 896-1123) and read(GetKeyReq) (:140-160) as a state machine (RFC-262 D5). Its
// suspension points are exactly C++'s waits: a storage read (U8), the prologue's
// nested getKey (P3) and the loop's nested getKey (L3). Between them it reads an
// immutable version of the write map and an immutable snapshot of the snapshot
// cache, so it runs without the lock: the step is atomic as C++'s actor step is.
// It takes c.mu at issue, to insert a storage reply and to re-snapshot the cache
// after that reply and after the prologue's getKey. Every resumption that
// continues the loop re-resolves the selectors first, as C++ does.
type rywWalk struct {
	rywWalkState
	// its are the walk's iterators, reused by every step: it, itEnd and two copies.
	its [4]rywIter
}

// rywWalkState is the walk apart from its iterators: assigning a whole rywWalk
// builds a kilobytes-large temporary, which grows a fresh goroutine's stack.
type rywWalkState struct {
	c       *rywCache
	view    *rywWrites // the owner's version, or nil = the snapshot cache alone (A2.2); borrowed
	cache   pseq[cacheEntry]
	gen     uint64 // c.gen at issue
	maxKey  []byte
	reverse bool
	getKey  bool // read(GetKeyReq) (A9)

	begin, end KeySelector
	limits     rangeLimits
	result     []KeyValue // the walk's own slice; rows alias the cache, the write map and valBuf
	keyBuf     []byte     // the walk's materialized selectors, valid until release hands it on
	// valBuf holds the rows' resolved AppendIfFits values. Rows outlive the walk
	// until Get copies them, so unlike keyBuf it is dropped on release.
	valBuf                             []byte
	more                               bool
	additionalRows                     int64
	itemsPast                          int32 // itemsPastEnd / itemsPastBegin
	requestCount                       int
	readToBegin, readThroughEnd        bool
	actualBeginOffset, actualEndOffset int32

	pc  walkPC
	req walkRequest // pcAwaitStorage: read_begin, read_end, requestLimit
	sub *rywWalk    // pcAwait*Key: the nested getKey, with its own requestCount
	err error

	hooks *rywWalkTestHooks
}

type rywWalkTestHooks struct {
	// onRequest, when set, sees each storage request as it is built (S1 traces).
	onRequest func(walkRequest)
}

func (s *rywWalkState) testHooks() *rywWalkTestHooks {
	if s.hooks == nil {
		s.hooks = &rywWalkTestHooks{}
	}
	return s.hooks
}

type walkPC uint8

const (
	pcAwaitStorage walkPC = iota + 1
	pcAwaitPreKey
	pcAwaitLoopKey
	pcDone
)

// walkRequest is one storage request: tr.getRange(read_begin, read_end, requestLimit).
type walkRequest struct {
	begin, end KeySelector
	limits     rangeLimits
}

// rywRangeResult is a walk's RangeResult: rows in scan order and the flags.
type rywRangeResult struct {
	kvs                               []KeyValue
	more, readToBegin, readThroughEnd bool
}

// newRangeWalkLocked builds a walk over begin and end after removeOrEqual (RYW:1752-1756);
// view stays immutable while it runs: live only under c.mu, pinned until unpinned.
func (c *rywCache) newRangeWalkLocked(view *rywWrites, begin, end KeySelector, limits rangeLimits, reverse bool, maxKey []byte) *rywWalk {
	c.checkViewLocked(view)
	w := c.rangeWalk(view, c.serverCache.snapshot(), c.gen, KeySelector{}, KeySelector{}, limits, reverse, maxKey)
	w.begin, w.end = removeOrEqualPair(&w.keyBuf, begin, end)
	w.actualBeginOffset, w.actualEndOffset = w.begin.Offset, w.end.Offset // RYW:607-608, 911-912
	return w
}

// rangeWalk is a walk over view and the cache snapshot cache, issued at gen.
func (c *rywCache) rangeWalk(view *rywWrites, cache pseq[cacheEntry], gen uint64, begin, end KeySelector, limits rangeLimits, reverse bool, maxKey []byte) *rywWalk {
	w := walkPool.Get().(*rywWalk) // released walks hold only emptied result and keyBuf scratch, and scrubbed iterators
	res, kbuf := w.result, w.keyBuf
	w.rywWalkState = rywWalkState{
		c: c, view: view, cache: cache, gen: gen, maxKey: maxKey, reverse: reverse,
		begin: begin, end: end, limits: limits,
		actualBeginOffset: begin.Offset, actualEndOffset: end.Offset, // RYW:607-608, 911-912
		result: res, keyBuf: kbuf,
	}
	return w
}

// walkPool keeps walks with their iterators' storage, which C++ keeps on the
// actor's frame: a read allocates neither once warm.
var walkPool = sync.Pool{New: func() any { return new(rywWalk) }}

// release returns the walk, and a nested walk it still holds, to walkPool. The
// caller has taken its result; nothing may use the walk afterwards.
func (w *rywWalk) release() {
	if w.sub != nil {
		w.sub.release()
	}
	for i := range w.its {
		w.its[i].scrub()
	}
	res := w.result
	clear(res) // the epilogue cleared any rows it trimmed off
	kbuf := w.keyBuf
	w.rywWalkState = rywWalkState{result: res[:0], keyBuf: kbuf[:0]}
	walkPool.Put(w)
}

// newGetKeyWalkLocked is read(GetKeyReq) (RYW:140-160): a limit-1 range walk,
// forward from sel to FGE(maxKey) for offset > 0, else backward from FGE("") to
// sel with its offset incremented (:151).
func (c *rywCache) newGetKeyWalkLocked(view *rywWrites, sel KeySelector, maxKey []byte) *rywWalk {
	c.checkViewLocked(view)
	return c.getKeyWalk(view, c.serverCache.snapshot(), c.gen, sel, maxKey)
}

// getKeyWalk is newGetKeyWalkLocked over view and the cache snapshot cache.
func (c *rywCache) getKeyWalk(view *rywWrites, cache pseq[cacheEntry], gen uint64, sel KeySelector, maxKey []byte) *rywWalk {
	one := rangeLimits{rows: 1, minRows: 1, bytes: byteLimitUnlimited} // GetRangeLimits(1)
	var w *rywWalk
	if sel.Offset > 0 {
		w = c.rangeWalk(view, cache, gen, sel, firstGreaterOrEqual(maxKey), one, false, maxKey)
	} else {
		sel.Offset++
		w = c.rangeWalk(view, cache, gen, firstGreaterOrEqual(allKeysBegin), sel, one, true, maxKey)
	}
	w.getKey = true
	return w
}

func (w *rywWalk) done() bool { return w.pc == pcDone }

func (w *rywWalk) fail(err error) {
	w.err, w.pc = err, pcDone
}

// returnEmpty is `return RangeResultRef(readToBegin, readThroughEnd)`: no rows and
// the given flags, without the epilogue.
func (w *rywWalk) returnEmpty(readToBegin, readThroughEnd bool) {
	w.result, w.more = nil, false
	w.readToBegin, w.readThroughEnd = readToBegin, readThroughEnd
	w.pc = pcDone
}

// rangeResult is the walk's result (after pcDone without error).
func (w *rywWalk) rangeResult() rywRangeResult {
	return rywRangeResult{kvs: w.result, more: w.more, readToBegin: w.readToBegin, readThroughEnd: w.readThroughEnd}
}

// keyResult maps a getKey walk's result to the resolved key (RYW:144-158).
func (w *rywWalk) keyResult() []byte {
	if !w.reverse {
		switch {
		case w.readToBegin:
			return allKeysBegin
		case w.readThroughEnd || len(w.result) == 0:
			return w.maxKey
		}
		return w.result[0].Key
	}
	switch {
	case w.readThroughEnd:
		return w.maxKey
	case w.readToBegin || len(w.result) == 0:
		return allKeysBegin
	}
	return w.result[0].Key
}

// start runs the prologue (P1-P3) and the loop to the first wait.
func (w *rywWalk) start() {
	it, itEnd := w.resolveBoth()
	// P2 (RYW:614-619, 918-923).
	if w.actualBeginOffset >= w.actualEndOffset && bytes.Compare(w.begin.Key, w.end.Key) >= 0 {
		w.returnEmpty(false, false)
		return
	}
	if (w.begin.isFirstGreaterOrEqual() && bytes.Equal(w.begin.Key, w.maxKey)) ||
		(w.end.isFirstGreaterOrEqual() && len(w.end.Key) == 0) {
		w.returnEmpty(w.readToBegin, w.readThroughEnd)
		return
	}
	// P3 (RYW:621-638): a nested getKey of the crossed bound.
	if !w.reverse && !w.end.isFirstGreaterOrEqual() && bytes.Compare(w.begin.Key, w.end.Key) > 0 {
		w.startSub(w.end, pcAwaitPreKey)
		return
	}
	if w.reverse && !w.begin.isFirstGreaterOrEqual() && bytes.Compare(w.begin.Key, w.end.Key) > 0 { // :925
		w.startSub(w.begin, pcAwaitPreKey)
		return
	}
	w.loop(it, itEnd)
}

// resolveBoth re-resolves both selectors: begin into it and end into itEnd
// forward (RYW:611-612), end into it and begin into itEnd backward (:914-916).
// it keeps its last position and itEnd starts as its copy, so each skip searches
// from nearby: a range's selectors are usually close.
func (w *rywWalk) resolveBoth() (it, itEnd *rywIter) {
	it, itEnd = &w.its[0], &w.its[1]
	initIter(it, w.view, &w.cache)
	it.valBuf = &w.valBuf
	if !w.reverse {
		w.resolve(&w.begin, it, &w.actualBeginOffset)
		itEnd.copyFrom(it)
		w.resolve(&w.end, itEnd, &w.actualEndOffset)
	} else {
		w.resolve(&w.end, it, &w.actualEndOffset)
		itEnd.copyFrom(it)
		w.resolve(&w.begin, itEnd, &w.actualBeginOffset)
	}
	return it, itEnd
}

// copyIter is it's copy in the walk's spare iterator i (2 or 3).
func (w *rywWalk) copyIter(i int, it *rywIter) *rywIter {
	c := &w.its[i]
	c.copyFrom(it)
	return c
}

// startSub starts the nested read(GetKeyReq(sel)) on the same version and cache
// snapshot and runs it to its first wait, as the nested actor runs synchronously
// until then. If it finishes without waiting, the parent resumes at once.
func (w *rywWalk) startSub(sel KeySelector, pc walkPC) {
	sel.Key = bytes.Clone(sel.Key)
	w.sub = w.c.getKeyWalk(w.view, w.cache, w.gen, sel, w.maxKey)
	if w.hooks != nil && w.hooks.onRequest != nil {
		w.sub.testHooks().onRequest = w.hooks.onRequest
	}
	w.sub.start()
	w.pc = pc
	if !w.sub.done() {
		return
	}
	if pc == pcAwaitPreKey {
		w.resumePreKey(w.sub.keyResult(), w.sub.err)
	} else {
		w.resumeLoopKey(w.sub.keyResult(), w.sub.err)
	}
}

// resumePreKey continues after P3's nested getKey (RYW:623-638, 927-940).
func (w *rywWalk) resumePreKey(resolved []byte, err error) {
	if err != nil {
		w.fail(err)
		return
	}
	w.sub.release() // resolved is the cache's or the write map's key, not the walk's
	w.sub = nil
	if len(resolved) == 0 {
		w.readToBegin = true
	}
	if bytes.Equal(resolved, w.maxKey) {
		w.readThroughEnd = true
	}
	if !w.reverse {
		if bytes.Compare(w.begin.Key, resolved) >= 0 && !w.begin.isBackward() {
			w.returnEmpty(false, false)
			return
		}
		if len(resolved) == 0 {
			w.returnEmpty(w.readToBegin, w.readThroughEnd)
			return
		}
	} else {
		if bytes.Compare(resolved, w.end.Key) >= 0 && w.end.Offset <= 1 {
			w.returnEmpty(false, false)
			return
		}
		if bytes.Equal(resolved, w.maxKey) {
			w.returnEmpty(w.readToBegin, w.readThroughEnd)
			return
		}
	}
	it, itEnd := w.resolveBoth()
	w.loop(it, itEnd)
}

// resumeLoopKey continues after L3's nested getKey: set the flags and the bound,
// then the epilogue. No re-resolution and no loop guard runs again (RYW:667-676,
// 971-980). It touches no shared state.
func (w *rywWalk) resumeLoopKey(resolved []byte, err error) {
	if err != nil {
		w.fail(err)
		return
	}
	w.sub.release()
	w.sub = nil
	if len(resolved) == 0 {
		w.readToBegin = true
	}
	if bytes.Equal(resolved, w.maxKey) {
		w.readThroughEnd = true
	}
	if !w.reverse {
		w.end = firstGreaterOrEqual(resolved)
	} else {
		w.begin = firstGreaterOrEqual(resolved)
	}
	w.epilogue()
}

// acceptStorageLocked takes a storage reply: it inserts it into the snapshot cache
// (U9) and takes the cache snapshot the walk resumes on (resumeStorage).
func (w *rywWalk) acceptStorageLocked(known storageKnown, err error) {
	if w.gen != w.c.gen {
		// The transaction was reset while the read waited: C++ kills the read through
		// resetPromise, which wins over whatever the storage read returns
		// (readWithConflictRangeRYW's choose, RYW:378-395), so its reply never reaches
		// the new cache. The read fails with its incarnation's cause (mapReadError).
		w.fail(context.Canceled)
		return
	}
	if err != nil {
		w.fail(err)
		return
	}
	if known.ok {
		w.c.serverCache.insertOwned(known.begin, known.end, known.kvs)
	}
	w.cache = w.c.serverCache.snapshot()
}

// storageKnown is what a storage reply makes known (getKnownKeyRange), its rows
// ascending: prepared before the reply's insert takes c.mu.
type storageKnown struct {
	begin, end []byte
	kvs        []KeyValue
	ok         bool
}

// known computes the reply's known range and rows (U9). The rows are the
// reply's own, which nothing writes; a reverse reply is reversed into a new slice.
func (w *rywWalk) known(r nativeRangeResult, err error) storageKnown {
	if err != nil {
		return storageKnown{}
	}
	var k storageKnown
	if !w.reverse {
		k.begin, k.end, k.ok = knownKeyRange(r, w.req.begin, w.req.end)
		k.kvs = r.kvs
		return k
	}
	k.begin, k.end, k.ok = knownKeyRangeBack(r, w.req.begin, w.req.end)
	if k.ok {
		asc := make([]KeyValue, len(r.kvs))
		for i, kv := range r.kvs {
			asc[len(r.kvs)-1-i] = kv
		}
		k.kvs = asc // RYW:1068-1072
	}
	return k
}

// resumeStorage re-resolves both selectors (U10) and continues the loop.
func (w *rywWalk) resumeStorage() {
	it, itEnd := w.resolveBoth()
	w.loop(it, itEnd)
}

// run drives the walk's waits without holding c.mu.
func (w *rywWalk) run(ctx context.Context, native nativeRangeFunc) error {
	c := w.c
	for w.pc != pcDone {
		switch w.pc {
		case pcAwaitStorage:
			res, err := native(ctx, w.req.begin, w.req.end, w.req.limits, w.reverse) // U8 / U8'
			known := w.known(res, err)
			c.withLock(func() { w.acceptStorageLocked(known, err) })
			if w.pc != pcDone {
				w.resumeStorage()
			}
		case pcAwaitPreKey:
			err := w.sub.run(ctx, native)
			// The nested read's replies are in the cache the parent re-resolves on.
			c.withLock(func() { w.cache = c.serverCache.snapshot() })
			w.resumePreKey(w.sub.keyResult(), err)
		case pcAwaitLoopKey:
			err := w.sub.run(ctx, native)
			w.resumeLoopKey(w.sub.keyResult(), err)
		default:
			w.fail(&wire.FDBError{Code: ErrInternalError})
		}
	}
	return w.err
}

// resolve is resolveKeySelectorFromCache (RYW:409-485, A3).
func (w *rywWalk) resolve(sel *KeySelector, it *rywIter, actualOffset *int32) {
	*sel = sel.removeOrEqual() // :420
	alreadyExhausted := sel.Offset == 1
	it.skip(sel.Key) // :424
	if sel.Offset <= 0 && it.b.equal(rywBoundary{key: sel.Key}) && len(sel.Key) != 0 {
		it.prev() // :426-427
	}
	maxKey := rywBoundary{key: w.maxKey}
	keykey := rywBoundary{key: sel.Key}
	// :433-439
	for sel.Offset > 1 && !it.unreadable() && !it.unknown() && it.e.compare(maxKey) < 0 {
		if it.isKV() {
			sel.Offset--
		}
		it.next()
		keykey = it.b
	}
	// :440-452
	for sel.Offset < 1 && !it.unreadable() && !it.unknown() && !it.atStart() {
		if it.isKV() {
			sel.Offset++
			if sel.Offset == 1 {
				keykey = it.b
				break
			}
		}
		it.prev()
		keykey = it.e
	}
	if !alreadyExhausted {
		*actualOffset = sel.Offset // :454-456
	}
	if !it.unreadable() && !it.unknown() && sel.Offset < 1 { // :458-463
		w.readToBegin = true
		*sel = firstGreaterOrEqual(allKeysBegin)
		return
	}
	if !it.unreadable() && !it.unknown() && sel.Offset > 1 { // :465-470
		w.readThroughEnd = true
		*sel = firstGreaterOrEqual(w.maxKey)
		return
	}
	for !it.unreadable() && it.empty() && it.e.compare(maxKey) < 0 { // :472-476
		it.next()
		keykey = it.b
	}
	sel.Key = materializeInto(&w.keyBuf, keykey)
}

// materializeInto is b's key, a successor appended to *buf; earlier keys in it stay valid.
func materializeInto(buf *[]byte, b rywBoundary) []byte {
	if !b.after {
		return b.key
	}
	n := b.size()
	if cap(*buf)-len(*buf) < n {
		*buf = make([]byte, 0, max(256, 2*cap(*buf), n))
	}
	off := len(*buf)
	s := (*buf)[off : off+n : off+n]
	copy(s, b.key)
	clear(s[len(b.key):])
	*buf = (*buf)[:off+n]
	return s
}

// clearLimit is A4's counted-clear cap (RYW:701, 1007).
func clearLimit(requestCount int) int {
	if requestCount == 0 {
		return 0
	}
	return 1 << min(requestCount, 20)
}

// knownKeyRange is getKnownKeyRange (RYW:487-508, A8): the range a forward reply
// makes known, or ok=false when it is empty.
func knownKeyRange(r nativeRangeResult, begin, end KeySelector) (b, e []byte, ok bool) {
	b = allKeysEnd
	if begin.Offset <= 1 {
		b = begin.Key
	}
	e = allKeysBegin
	if !r.more && end.Offset >= 1 {
		e = end.Key
	}
	if r.readToBegin {
		b = allKeysBegin
	}
	if r.readThroughEnd {
		e = allKeysEnd
	}
	if n := len(r.kvs); n > 0 {
		if bytes.Compare(r.kvs[0].Key, b) < 0 {
			b = r.kvs[0].Key
		}
		last := r.kvs[n-1].Key
		switch {
		case r.readThrough != nil:
			if bytes.Compare(r.readThrough, e) > 0 {
				e = r.readThrough
			}
		case !r.more && bytes.Compare(last, e) < 0:
		default:
			e = keyAfterBytes(last)
		}
	}
	if bytes.Compare(b, e) >= 0 {
		return nil, nil, false
	}
	return b, e, true
}

// knownKeyRangeBack is getKnownKeyRangeBack (RYW:816-839); rows are descending.
func knownKeyRangeBack(r nativeRangeResult, begin, end KeySelector) (b, e []byte, ok bool) {
	b = allKeysEnd
	if !r.more && begin.Offset <= 1 {
		b = begin.Key
	}
	e = allKeysBegin
	if end.Offset >= 1 {
		e = end.Key
	}
	if r.readToBegin {
		b = allKeysBegin
	}
	if r.readThroughEnd {
		e = allKeysEnd
	}
	if n := len(r.kvs); n > 0 {
		last := r.kvs[n-1].Key // the smallest
		switch {
		case r.readThrough != nil:
			if bytes.Compare(r.readThrough, b) < 0 {
				b = r.readThrough
			}
		case !r.more && bytes.Compare(last, b) > 0:
		default:
			b = last
		}
		if first := r.kvs[0].Key; bytes.Compare(first, e) >= 0 {
			e = keyAfterBytes(first)
		}
	}
	if bytes.Compare(b, e) >= 0 {
		return nil, nil, false
	}
	return b, e, true
}
