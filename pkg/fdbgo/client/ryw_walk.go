package client

import (
	"bytes"
	"context"
	"sort"

	"fdb.dev/pkg/fdbgo/wire"
)

// rywWalk is C++ getRangeValue / getRangeValueBack (ReadYourWrites.actor.cpp:592-813,
// 896-1123) and read(GetKeyReq) (:140-160) as a state machine (RFC-262 D5). It runs
// under c.mu between suspension points, which are exactly C++'s waits: a storage
// read (U8), the prologue's nested getKey (P3) and the loop's nested getKey (L3).
// Iterators live only inside one critical section; every resumption that continues
// the loop re-resolves the selectors first, as C++ does.
type rywWalk struct {
	c       *rywCache
	view    *rywWrites // the owner's version, or nil = the snapshot cache alone (A2.2); borrowed
	gen     uint64     // c.gen at issue
	maxKey  []byte
	reverse bool
	getKey  bool // read(GetKeyReq) (A9)

	begin, end                         KeySelector
	limits                             rangeLimits
	result                             []KeyValue
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

// newRangeWalkLocked starts nothing; startLocked runs it to its first wait.
func (c *rywCache) newRangeWalkLocked(view *rywWrites, begin, end KeySelector, limits rangeLimits, reverse bool, maxKey []byte) *rywWalk {
	return &rywWalk{
		c: c, view: view, gen: c.gen, maxKey: maxKey, reverse: reverse,
		begin: begin, end: end, limits: limits,
		actualBeginOffset: begin.Offset, actualEndOffset: end.Offset, // RYW:607-608, 911-912
	}
}

// newGetKeyWalkLocked is read(GetKeyReq) (RYW:140-160): a limit-1 range walk,
// forward from sel to FGE(maxKey) for offset > 0, else backward from FGE("") to
// sel with its offset incremented (:151).
func (c *rywCache) newGetKeyWalkLocked(view *rywWrites, sel KeySelector, maxKey []byte) *rywWalk {
	one := rangeLimits{rows: 1, minRows: 1, bytes: byteLimitUnlimited} // GetRangeLimits(1)
	var w *rywWalk
	if sel.Offset > 0 {
		w = c.newRangeWalkLocked(view, sel, firstGreaterOrEqual(maxKey), one, false, maxKey)
	} else {
		sel.Offset++
		w = c.newRangeWalkLocked(view, firstGreaterOrEqual(allKeysBegin), sel, one, true, maxKey)
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

// startLocked runs the prologue (P1-P3) and the loop to the first wait.
func (w *rywWalk) startLocked() {
	it, itEnd := w.resolveBothLocked()
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
		w.startSubLocked(w.end, pcAwaitPreKey)
		return
	}
	if w.reverse && !w.begin.isFirstGreaterOrEqual() && bytes.Compare(w.begin.Key, w.end.Key) > 0 { // :925
		w.startSubLocked(w.begin, pcAwaitPreKey)
		return
	}
	w.loopLocked(it, itEnd)
}

// resolveBothLocked re-resolves both selectors: begin into it and end into itEnd
// forward (RYW:611-612), end into it and begin into itEnd backward (:914-916).
func (w *rywWalk) resolveBothLocked() (it, itEnd *rywIter) {
	it, itEnd = w.c.newIterLocked(w.view), w.c.newIterLocked(w.view)
	if !w.reverse {
		w.resolveLocked(&w.begin, it, &w.actualBeginOffset)
		w.resolveLocked(&w.end, itEnd, &w.actualEndOffset)
	} else {
		w.resolveLocked(&w.end, it, &w.actualEndOffset)
		w.resolveLocked(&w.begin, itEnd, &w.actualBeginOffset)
	}
	return it, itEnd
}

// startSubLocked starts the nested read(GetKeyReq(sel)) on the same version and
// runs it to its first wait, as the nested actor runs synchronously until then. If
// it finishes without waiting, the parent resumes at once.
func (w *rywWalk) startSubLocked(sel KeySelector, pc walkPC) {
	w.sub = w.c.newGetKeyWalkLocked(w.view, sel, w.maxKey)
	w.sub.gen = w.gen
	w.sub.onRequest = w.onRequest
	w.sub.startLocked()
	w.pc = pc
	if !w.sub.done() {
		return
	}
	if pc == pcAwaitPreKey {
		w.resumePreKeyLocked(w.sub.keyResult(), w.sub.err)
	} else {
		w.resumeLoopKey(w.sub.keyResult(), w.sub.err)
	}
}

// resumePreKeyLocked continues after P3's nested getKey (RYW:623-638, 927-940).
func (w *rywWalk) resumePreKeyLocked(resolved []byte, err error) {
	if err != nil {
		w.fail(err)
		return
	}
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
	it, itEnd := w.resolveBothLocked()
	w.loopLocked(it, itEnd)
}

// resumeLoopKey continues after L3's nested getKey: set the flags and the bound,
// then the epilogue. No re-resolution and no loop guard runs again (RYW:667-676,
// 971-980). It touches no shared state.
func (w *rywWalk) resumeLoopKey(resolved []byte, err error) {
	if err != nil {
		w.fail(err)
		return
	}
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

// resumeStorageLocked continues after a storage read: insert the reply into the
// snapshot cache (U9) and re-resolve both selectors (U10), then the loop.
func (w *rywWalk) resumeStorageLocked(r nativeRangeResult, err error) {
	if err != nil {
		w.fail(err)
		return
	}
	if w.gen != w.c.gen {
		// The transaction was reset while the read waited: C++ kills the read through
		// resetPromise (RYW:358-364), so its reply never reaches the new cache. The
		// read fails with its incarnation's cause (mapReadError).
		w.fail(context.Canceled)
		return
	}
	var b, e []byte
	var ok bool
	if !w.reverse {
		b, e, ok = knownKeyRange(r, w.req.begin, w.req.end)
		if ok {
			w.c.serverCache.insert(b, e, r.kvs)
		}
	} else {
		b, e, ok = knownKeyRangeBack(r, w.req.begin, w.req.end)
		if ok {
			asc := make([]KeyValue, len(r.kvs))
			for i, kv := range r.kvs {
				asc[len(r.kvs)-1-i] = kv
			}
			w.c.serverCache.insert(b, e, asc) // RYW:1068-1072
		}
	}
	it, itEnd := w.resolveBothLocked()
	w.loopLocked(it, itEnd)
}

// run drives the walk's waits without holding c.mu.
func (w *rywWalk) run(ctx context.Context, native nativeRangeFunc) error {
	c := w.c
	for w.pc != pcDone {
		switch w.pc {
		case pcAwaitStorage:
			res, err := native(ctx, w.req.begin, w.req.end, w.req.limits, w.reverse) // U8 / U8'
			c.mu.Lock()
			w.resumeStorageLocked(res, err)
			c.mu.Unlock()
		case pcAwaitPreKey:
			err := w.sub.run(ctx, native)
			c.mu.Lock()
			w.resumePreKeyLocked(w.sub.keyResult(), err)
			c.mu.Unlock()
		case pcAwaitLoopKey:
			err := w.sub.run(ctx, native)
			w.resumeLoopKey(w.sub.keyResult(), err)
		default:
			w.fail(&wire.FDBError{Code: ErrInternalError})
		}
	}
	return w.err
}

// resolveLocked is resolveKeySelectorFromCache (RYW:409-485, A3).
func (w *rywWalk) resolveLocked(sel *KeySelector, it *rywIter, actualOffset *int32) {
	*sel = sel.removeOrEqual() // :420
	alreadyExhausted := sel.Offset == 1
	it.skip(sel.Key) // :424
	if sel.Offset <= 0 && bytes.Equal(it.begin, sel.Key) && len(sel.Key) != 0 {
		it.prev() // :426-427
	}
	keykey := sel.Key
	// :433-439
	for sel.Offset > 1 && !it.unreadable() && !it.unknown() && bytes.Compare(it.end, w.maxKey) < 0 {
		if it.isKV() {
			sel.Offset--
		}
		it.next()
		keykey = it.begin
	}
	// :440-452
	for sel.Offset < 1 && !it.unreadable() && !it.unknown() && len(it.begin) != 0 {
		if it.isKV() {
			sel.Offset++
			if sel.Offset == 1 {
				keykey = it.begin
				break
			}
		}
		it.prev()
		keykey = it.end
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
	for !it.unreadable() && it.empty() && bytes.Compare(it.end, w.maxKey) < 0 { // :472-476
		it.next()
		keykey = it.begin
	}
	sel.Key = keykey
}

// clearLimit is A4's counted-clear cap (RYW:701, 1007).
func clearLimit(requestCount int) int {
	if requestCount == 0 {
		return 0
	}
	return 1 << min(requestCount, 20)
}

// loopLocked runs the loop (RYW:641-797 forward, 945-1109 backward) until a wait
// or the end.
func (w *rywWalk) loopLocked(it, itEnd *rywIter) {
	if w.reverse {
		w.loopBackLocked(it, itEnd)
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
			w.startSubLocked(w.end, pcAwaitLoopKey)
			return
		}
		// L4 (:679-683)
		if !it.unreadable() && !it.unknown() && bytes.Compare(it.begin, itEnd.begin) > 0 {
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
			(w.begin.Offset > 0 && w.end.isFirstGreaterOrEqual() && bytes.Equal(w.end.Key, it.begin))) {
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
			if w.unknownLocked(it, itEnd) {
				return
			}
		case it.isKV():
			w.kvForwardLocked(it)
		default:
			it.next() // E (:795-796)
		}
	}
}

// unknownLocked is U (RYW:692-771). It reports true when the walk suspended or
// ended.
func (w *rywWalk) unknownLocked(it, itEnd *rywIter) bool {
	// U1 (:693-697)
	if w.limits.hasByteLimit() && w.limits.hasSatisfiedMinRows() && len(w.result) > 0 && w.itemsPast >= 1-w.end.Offset {
		w.more = true
		w.epilogue()
		return true
	}
	// U2 (:699-703)
	ucEnd := it.clone()
	singleClears := 0
	limit := clearLimit(w.requestCount)
	if bytes.Compare(it.begin, itEnd.begin) < 0 {
		singleClears = min(skipUncached(ucEnd, itEnd, limit+100), limit)
	}
	// U3 (:705-721)
	var readEnd KeySelector
	switch {
	case !ucEnd.same(itEnd):
		readEnd = firstGreaterOrEqual(ucEnd.end)
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
		if bytes.Compare(it.begin, k) > 0 {
			k = it.begin
		}
		w.begin = firstGreaterOrEqual(k)
		readBegin = w.begin
	case w.begin.Offset > 1:
		readBegin = firstGreaterOrEqual(w.begin.Key)
		w.additionalRows += int64(w.begin.Offset - 1)
	default:
		readBegin = w.begin
		n := countUncachedBack(it.clone(), limit)
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
	w.req, w.pc = req, pcAwaitStorage
	if w.onRequest != nil {
		w.onRequest(req)
	}
}

// kvForwardLocked is K (RYW:772-794).
func (w *rywWalk) kvForwardLocked(it *rywIter) {
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
	w.result = append(w.result, run[:count]...)
	it.next()
}

func (w *rywWalk) decrement(kv KeyValue) {
	size := 0
	if w.limits.hasByteLimit() {
		size = 8 + len(kv.Key) + len(kv.Value) // NAPI:2823-2829
	}
	w.limits.decrement(1, size)
}

// loopBackLocked is getRangeValueBack's loop (RYW:945-1109).
func (w *rywWalk) loopBackLocked(it, itEnd *rywIter) {
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
			w.startSubLocked(w.begin, pcAwaitLoopKey)
			return
		}
		// L4' (:983-988)
		if w.itemsPast >= w.begin.Offset-1 && !it.unreadable() && !it.unknown() && bytes.Compare(it.begin, itEnd.begin) < 0 {
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
		if w.end.isFirstGreaterOrEqual() && bytes.Equal(w.end.Key, it.begin) {
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
			if w.unknownBackLocked(it, itEnd) {
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

// unknownBackLocked is U' (RYW:999-1081).
func (w *rywWalk) unknownBackLocked(it, itEnd *rywIter) bool {
	// U1' (:1000-1003): no hasSatisfiedMinRows term, kept verbatim (A7).
	if w.limits.hasByteLimit() && len(w.result) > 0 && w.itemsPast >= w.begin.Offset-1 {
		w.more = true
		w.epilogue()
		return true
	}
	// U2' (:1005-1009)
	ucEnd := it.clone()
	singleClears := 0
	limit := clearLimit(w.requestCount)
	if bytes.Compare(it.begin, itEnd.begin) > 0 {
		singleClears = min(skipUncachedBack(ucEnd, itEnd, limit+100), limit)
	}
	// U3' (:1011-1026)
	var readBegin KeySelector
	switch {
	case !ucEnd.same(itEnd):
		readBegin = firstGreaterOrEqual(ucEnd.begin)
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
		if bytes.Compare(it.end, k) < 0 {
			k = it.end
		}
		w.end = firstGreaterOrEqual(k)
		readEnd = w.end
	case w.end.Offset < 1:
		readEnd = firstGreaterOrEqual(w.end.Key)
		w.additionalRows += int64(1 - w.end.Offset)
	default:
		readEnd = w.end
		n := countUncached(it.clone(), w.maxKey, limit)
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
	b, e := it.begin, it.end
	singleEmpty := 0
	for !it.same(end) {
		iterationLimit--
		if iterationLimit < 0 {
			break
		}
		if it.unreadable() || it.empty() {
			if it.unreadable() || !isKeyAfter(e, b) {
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
		e = it.end
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
	b, e := it.begin, it.end
	singleEmpty := 0
	for bytes.Compare(e, maxKey) < 0 {
		if it.unreadable() || it.empty() {
			if it.unreadable() || !isKeyAfter(e, b) {
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
		e = it.end
	}
	return singleEmpty
}

// skipUncachedBack is RYW:841-871.
func skipUncachedBack(it, end *rywIter, iterationLimit int) int {
	b, e := it.begin, it.end
	singleEmpty := 0
	for !it.same(end) {
		iterationLimit--
		if iterationLimit < 0 {
			break
		}
		if it.unreadable() || it.empty() {
			if it.unreadable() || !isKeyAfter(e, b) {
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
		b = it.begin
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
	b, e := it.begin, it.end
	singleEmpty := 0
	for len(b) > 0 {
		if it.unreadable() || it.empty() {
			if it.unreadable() || !isKeyAfter(e, b) {
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
		b = it.begin
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
