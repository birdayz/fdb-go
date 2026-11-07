package client

import (
	"bytes"
	"context"
	"sort"
	"sync"

	"fdb.dev/pkg/fdbgo/wire"
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
	keyBuf     []byte     // scratch for materialized selector boundaries during resolve
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

	// onRequest, when set, sees each storage request as it is built (S1 traces).
	onRequest func(walkRequest)
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

// newRangeWalkLocked starts nothing; start runs it to its first wait. Its selectors
// are begin and end after removeOrEqual (RYW:1752-1756), built in the walk's scratch.
// view must stay immutable while the walk runs: the live version only while c.mu
// is held, a pinned one until it is unpinned.
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
	clear(res)
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
	w.sub.onRequest = w.onRequest
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

// loop runs the loop (RYW:641-797 forward, 945-1109 backward) until a wait
// or the end.
func (w *rywWalk) loop(it, itEnd *rywIter) {
	if w.reverse {
		w.loopBack(it, itEnd)
		return
	}
	for {
		if it.broken || itEnd.broken {
			w.fail(&wire.FDBError{Code: ErrInternalError})
			return
		}
		// L1 (:653-655)
		if len(w.result) == 0 && w.actualBeginOffset >= w.actualEndOffset && bytes.Compare(w.begin.Key, w.end.Key) >= 0 {
			w.returnEmpty(false, false)
			return
		}
		// L2 (:657-659)
		if w.end.Offset <= 1 && len(w.end.Key) == 0 {
			w.returnEmpty(w.readToBegin, w.readThroughEnd)
			return
		}
		// L3 (:661-677)
		if (w.begin.Offset >= w.end.Offset && bytes.Compare(w.begin.Key, w.end.Key) >= 0) ||
			(w.begin.Offset >= 1 && bytes.Compare(w.begin.Key, w.maxKey) >= 0) {
			if w.end.isFirstGreaterOrEqual() || len(w.result) == 0 {
				w.epilogue()
				return
			}
			w.startSub(w.end, pcAwaitLoopKey)
			return
		}
		// L4 (:679-683)
		if !it.unreadable() && !it.unknown() && it.b.compare(itEnd.b) > 0 {
			if w.end.isFirstGreaterOrEqual() {
				w.epilogue()
				return
			}
			w.returnEmpty(w.readToBegin, w.readThroughEnd)
			return
		}
		// L5 (:685-686)
		if w.limits.isReached() && w.itemsPast >= 1-w.end.Offset {
			w.epilogue()
			return
		}
		// L6 (:688-690)
		if it.same(itEnd) && ((!it.unreadable() && !it.unknown()) ||
			(w.begin.Offset > 0 && w.end.isFirstGreaterOrEqual() && it.b.equal(rywBoundary{key: w.end.Key}))) {
			w.epilogue()
			return
		}
		// L7 (:692): type() throws on an unreadable segment.
		if it.unreadable() {
			w.fail(&wire.FDBError{Code: ErrAccessedUnreadable})
			return
		}
		switch {
		case it.unknown():
			if w.readUnknown(it, itEnd) {
				return
			}
		case it.isKV():
			w.kvForward(it)
		default:
			it.next() // E (:795-796)
		}
	}
}

// readUnknown is U (RYW:692-771). It reports true when the walk suspended or
// ended.
func (w *rywWalk) readUnknown(it, itEnd *rywIter) bool {
	// U1 (:693-697)
	if w.limits.hasByteLimit() && w.limits.hasSatisfiedMinRows() && len(w.result) > 0 && w.itemsPast >= 1-w.end.Offset {
		w.more = true
		w.epilogue()
		return true
	}
	// U2 (:699-703)
	ucEnd := w.copyIter(2, it)
	singleClears := 0
	limit := clearLimit(w.requestCount)
	if it.b.compare(itEnd.b) < 0 {
		singleClears = min(skipUncached(ucEnd, itEnd, limit+100), limit)
	}
	// U3 (:705-721)
	var readEnd KeySelector
	switch {
	case !ucEnd.same(itEnd):
		readEnd = firstGreaterOrEqual(ucEnd.endKey())
		if w.end.Offset < 1 {
			w.additionalRows += int64(1 - w.end.Offset)
		}
	case w.end.Offset < 1:
		readEnd = firstGreaterOrEqual(w.end.Key)
		w.additionalRows += int64(1 - w.end.Offset)
	default:
		readEnd = w.end
		if w.end.Offset > 1 {
			singleClears += countUncached(ucEnd, w.maxKey, limit-singleClears)
			readEnd.Offset += int32(singleClears)
		}
	}
	w.additionalRows += int64(singleClears) // U4 (:723)
	// U5 (:725-741)
	var readBegin KeySelector
	switch {
	case w.begin.isFirstGreaterOrEqual():
		k := w.begin.Key
		if it.b.compare(rywBoundary{key: k}) > 0 {
			k = it.beginKey()
		}
		w.begin = firstGreaterOrEqual(k)
		readBegin = w.begin
	case w.begin.Offset > 1:
		readBegin = firstGreaterOrEqual(w.begin.Key)
		w.additionalRows += int64(w.begin.Offset - 1)
	default:
		readBegin = w.begin
		n := countUncachedBack(w.copyIter(3, it), limit)
		readBegin.Offset -= int32(n)
		w.additionalRows += int64(n)
	}
	// U6 (:743-746)
	if bytes.Compare(readEnd.Key, readBegin.Key) < 0 {
		readEnd.Key = readBegin.Key
	}
	// U7 (:748-757): sized and counted before the wait.
	requestLimit := w.limits
	setRequestLimits(&requestLimit, w.additionalRows, 2-readBegin.Offset, w.requestCount)
	w.requestCount++
	w.additionalRows = 0
	w.await(walkRequest{begin: readBegin, end: readEnd, limits: requestLimit}) // U8
	return true
}

func (w *rywWalk) await(req walkRequest) {
	req.begin.Key = bytes.Clone(req.begin.Key)
	req.end.Key = bytes.Clone(req.end.Key)
	w.req, w.pc = req, pcAwaitStorage
	if w.onRequest != nil {
		w.onRequest(req)
	}
}

// kvForward is K (RYW:772-794).
func (w *rywWalk) kvForward(it *rywIter) {
	if _, ok := it.kv(); !ok { // a matched CompareAndClear
		it.next()
		return
	}
	bound := w.maxKey // "not technically correct ... protected above" (:778-780)
	if w.end.isFirstGreaterOrEqual() {
		bound = w.end.Key
	}
	run := it.skipContiguous(bound)
	count := 0
	for ; count < len(run) && !w.limits.isReached(); count++ {
		w.decrement(run[count])
	}
	w.itemsPast += int32(len(run) - count)
	w.reserve(count)
	w.result = append(w.result, run[:count]...)
	it.next()
}

// reserve gives a first run of one row room for a second, as a selector
// range's rows often come in separate runs.
func (w *rywWalk) reserve(count int) {
	if cap(w.result) == 0 && count == 1 && !w.limits.isReached() {
		w.result = make([]KeyValue, 0, 2)
	}
}

func (w *rywWalk) decrement(kv KeyValue) {
	size := 0
	if w.limits.hasByteLimit() {
		size = 8 + len(kv.Key) + len(kv.Value) // NAPI:2823-2829
	}
	w.limits.decrement(1, size)
}

// loopBack is getRangeValueBack's loop (RYW:945-1109).
func (w *rywWalk) loopBack(it, itEnd *rywIter) {
	for {
		if it.broken || itEnd.broken {
			w.fail(&wire.FDBError{Code: ErrInternalError})
			return
		}
		// L1 (:957-959)
		if len(w.result) == 0 && w.actualBeginOffset >= w.actualEndOffset && bytes.Compare(w.begin.Key, w.end.Key) >= 0 {
			w.returnEmpty(false, false)
			return
		}
		// L2' (:961-963)
		if !w.begin.isBackward() && bytes.Compare(w.begin.Key, w.maxKey) >= 0 {
			w.returnEmpty(w.readToBegin, w.readThroughEnd)
			return
		}
		// L3' (:965-981)
		if (w.begin.Offset >= w.end.Offset && bytes.Compare(w.begin.Key, w.end.Key) >= 0) ||
			(w.end.Offset <= 1 && len(w.end.Key) == 0) {
			if w.begin.isFirstGreaterOrEqual() || len(w.result) == 0 {
				w.epilogue()
				return
			}
			w.startSub(w.begin, pcAwaitLoopKey)
			return
		}
		// L4' (:983-988)
		if w.itemsPast >= w.begin.Offset-1 && !it.unreadable() && !it.unknown() && it.b.compare(itEnd.b) < 0 {
			if w.begin.isFirstGreaterOrEqual() {
				w.epilogue()
				return
			}
			w.returnEmpty(w.readToBegin, w.readThroughEnd)
			return
		}
		// L5' (:990-991)
		if w.limits.isReached() && w.itemsPast >= w.begin.Offset-1 {
			w.epilogue()
			return
		}
		// L6' (:993-997)
		if w.end.isFirstGreaterOrEqual() && it.b.equal(rywBoundary{key: w.end.Key}) {
			if w.itemsPast >= w.begin.Offset-1 && it.same(itEnd) {
				w.epilogue()
				return
			}
			it.prev()
			if it.broken {
				w.fail(&wire.FDBError{Code: ErrInternalError})
				return
			}
		}
		if it.unreadable() { // is_unknown_range() throws (:999)
			w.fail(&wire.FDBError{Code: ErrAccessedUnreadable})
			return
		}
		if it.unknown() {
			if w.readUnknownBack(it, itEnd) {
				return
			}
			continue
		}
		// K'/E' (:1082-1108)
		if it.isKV() {
			if _, ok := it.kv(); ok {
				bound := allKeysBegin
				if w.begin.isFirstGreaterOrEqual() {
					bound = w.begin.Key
				}
				run := it.skipContiguousBack(bound) // ascending: start .. end
				count := 0
				for ; count < len(run) && !w.limits.isReached(); count++ {
					w.decrement(run[len(run)-count-1])
				}
				w.itemsPast += int32(len(run) - count)
				w.reserve(count)
				for i := 0; i < count; i++ {
					w.result = append(w.result, run[len(run)-i-1])
				}
			}
		}
		if it.same(itEnd) {
			w.epilogue()
			return
		}
		it.prev()
	}
}

// readUnknownBack is U' (RYW:999-1081).
func (w *rywWalk) readUnknownBack(it, itEnd *rywIter) bool {
	// U1' (:1000-1003): no hasSatisfiedMinRows term, kept verbatim (A7).
	if w.limits.hasByteLimit() && len(w.result) > 0 && w.itemsPast >= w.begin.Offset-1 {
		w.more = true
		w.epilogue()
		return true
	}
	// U2' (:1005-1009)
	ucEnd := w.copyIter(2, it)
	singleClears := 0
	limit := clearLimit(w.requestCount)
	if it.b.compare(itEnd.b) > 0 {
		singleClears = min(skipUncachedBack(ucEnd, itEnd, limit+100), limit)
	}
	// U3' (:1011-1026)
	var readBegin KeySelector
	switch {
	case !ucEnd.same(itEnd):
		readBegin = firstGreaterOrEqual(ucEnd.beginKey())
		if w.begin.Offset > 1 {
			w.additionalRows += int64(w.begin.Offset - 1)
		}
	case w.begin.Offset > 1:
		readBegin = firstGreaterOrEqual(w.begin.Key)
		w.additionalRows += int64(w.begin.Offset - 1)
	default:
		readBegin = w.begin
		if w.begin.Offset < 1 {
			singleClears += countUncachedBack(ucEnd, limit-singleClears)
			readBegin.Offset -= int32(singleClears)
		}
	}
	w.additionalRows += int64(singleClears) // U4' (:1028)
	// U5' (:1030-1045)
	var readEnd KeySelector
	switch {
	case w.end.isFirstGreaterOrEqual():
		k := w.end.Key
		if it.e.compare(rywBoundary{key: k}) < 0 {
			k = it.endKey()
		}
		w.end = firstGreaterOrEqual(k)
		readEnd = w.end
	case w.end.Offset < 1:
		readEnd = firstGreaterOrEqual(w.end.Key)
		w.additionalRows += int64(1 - w.end.Offset)
	default:
		readEnd = w.end
		n := countUncached(w.copyIter(3, it), w.maxKey, limit)
		readEnd.Offset += int32(n)
		w.additionalRows += int64(n)
	}
	// U6' (:1047-1050)
	if bytes.Compare(readBegin.Key, readEnd.Key) > 0 {
		readBegin.Key = readEnd.Key
	}
	// U7' (:1052-1061): the offset argument is read_end.offset.
	requestLimit := w.limits
	setRequestLimits(&requestLimit, w.additionalRows, readEnd.Offset, w.requestCount)
	w.requestCount++
	w.additionalRows = 0
	w.await(walkRequest{begin: readBegin, end: readEnd, limits: requestLimit}) // U8'
	return true
}

// epilogue is RYW:799-811 forward, :1111-1120 backward.
func (w *rywWalk) epilogue() {
	w.more = w.more || w.limits.isReached()
	if !w.reverse {
		if w.end.isFirstGreaterOrEqual() {
			keep := sort.Search(len(w.result), func(i int) bool { return bytes.Compare(w.result[i].Key, w.end.Key) >= 0 })
			if keep < len(w.result) {
				w.more = false
			}
			w.result = w.result[:keep]
		}
		w.readThroughEnd = !w.more && w.readThroughEnd
	} else {
		if w.begin.isFirstGreaterOrEqual() {
			// Rows are descending; keep the prefix >= begin.key.
			keep := sort.Search(len(w.result), func(i int) bool { return bytes.Compare(w.result[i].Key, w.begin.Key) < 0 })
			if keep < len(w.result) {
				w.more = false
			}
			w.result = w.result[:keep]
		}
		w.readToBegin = !w.more && w.readToBegin
	}
	w.pc = pcDone
}

// skipUncached is RYW:510-538: from an unknown segment, cross unknown and KV
// segments and single-key empties toward end, at most iterationLimit segments; stop
// at a multi-key empty or an unreadable segment; back up to an unknown segment. It
// returns the single-key empties crossed.
func skipUncached(it, end *rywIter, iterationLimit int) int {
	b, e := it.b, it.e
	singleEmpty := 0
	for !it.same(end) {
		iterationLimit--
		if iterationLimit < 0 {
			break
		}
		if it.unreadable() || it.empty() {
			if it.unreadable() || !e.isKeyAfter(b) {
				for (it.unreadable() || !it.unknown()) && !it.broken {
					it.prev()
				}
				return singleEmpty
			}
			singleEmpty++
		} else {
			b = e
		}
		it.next()
		e = it.e
	}
	for (it.unreadable() || !it.unknown()) && !it.broken {
		it.prev()
	}
	return singleEmpty
}

// countUncached is RYW:540-567.
func countUncached(it *rywIter, maxKey []byte, maxClears int) int {
	if maxClears <= 0 {
		return 0
	}
	b, e := it.b, it.e
	last := rywBoundary{key: maxKey}
	singleEmpty := 0
	for e.compare(last) < 0 {
		if it.unreadable() || it.empty() {
			if it.unreadable() || !e.isKeyAfter(b) {
				return singleEmpty
			}
			singleEmpty++
			if singleEmpty >= maxClears {
				return maxClears
			}
		} else {
			b = e
		}
		it.next()
		e = it.e
	}
	return singleEmpty
}

// skipUncachedBack is RYW:841-871.
func skipUncachedBack(it, end *rywIter, iterationLimit int) int {
	b, e := it.b, it.e
	singleEmpty := 0
	for !it.same(end) {
		iterationLimit--
		if iterationLimit < 0 {
			break
		}
		if it.unreadable() || it.empty() {
			if it.unreadable() || !e.isKeyAfter(b) {
				for (it.unreadable() || !it.unknown()) && !it.broken {
					it.next()
				}
				return singleEmpty
			}
			singleEmpty++
		} else {
			e = b
		}
		it.prev()
		b = it.b
	}
	for (it.unreadable() || !it.unknown()) && !it.broken {
		it.next()
	}
	return singleEmpty
}

// countUncachedBack is RYW:873-894.
func countUncachedBack(it *rywIter, maxClears int) int {
	if maxClears <= 0 {
		return 0
	}
	b, e := it.b, it.e
	singleEmpty := 0
	for b.size() > 0 {
		if it.unreadable() || it.empty() {
			if it.unreadable() || !e.isKeyAfter(b) {
				return singleEmpty
			}
			singleEmpty++
			if singleEmpty >= maxClears {
				return maxClears
			}
		} else {
			e = b
		}
		it.prev()
		b = it.b
	}
	return singleEmpty
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
