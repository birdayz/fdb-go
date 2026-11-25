package client

import (
	"bytes"
	"sort"

	"fdb.dev/internal/fdbwire"
)

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
	if w.hooks != nil && w.hooks.onRequest != nil {
		w.hooks.onRequest(req)
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
			clear(w.result[keep:]) // the pooled slice keeps no dropped row's bytes alive
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
			clear(w.result[keep:])
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
