package client

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// native_range.go ports NativeAPI getRange for plain key-values (RFC-262 D4):
// Transaction::getRangeInternal (NativeAPI.actor.cpp:5801-5848), getRange
// (:4596-4893), getRangeFallback (:4457-4500) with resolveKey (:4447-4455), and
// getRangeFinished's conflict range (:4558-4586). Line numbers below are
// NativeAPI.actor.cpp of FoundationDB 7.3.77 unless another file is named.

// nativeRangeResult is the RangeResult getRange returns.
type nativeRangeResult struct {
	kvs                               []KeyValue // scan order
	more, readToBegin, readThroughEnd bool
	readThrough                       []byte     // nil when absent
	conflict                          *[2][]byte // set only when snapshot == false
}

type nativeRangeFunc func(ctx context.Context, begin, end KeySelector, limits rangeLimits, reverse bool) (nativeRangeResult, error)

// nativeGetRange is Transaction::getRange over selectors (getRangeInternal,
// :5801-5848, then ::getRange). The checks come before the read version, so a
// read they answer acquires none. Every request of the call, the fallback's
// getKeys and exact range included, carries the one version taken here.
// conflict is getRangeFinished's range when snapshot is false and the read got
// past the checks; nil otherwise.
func (tx *Transaction) nativeGetRange(parentCtx context.Context, begin, end KeySelector, limits rangeLimits, snapshot, reverse bool) (nativeRangeResult, error) {
	tx.db.metrics.getRangeRequests.Add(1) // increaseCounterForRequest (:5808)
	if limits.isReached() {               // :5810-5811
		return nativeRangeResult{}, nil
	}
	if !limits.isValid() { // :5813-5814
		return nativeRangeResult{}, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}
	begin, end = begin.removeOrEqual(), end.removeOrEqual() // :5818-5828
	if begin.Offset >= end.Offset && bytes.Compare(begin.Key, end.Key) >= 0 {
		return nativeRangeResult{}, nil // inverted (:5830-5833)
	}

	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	// startTransaction and validateVersion (:4617-4618), once per call.
	readVersion, err := tx.operationReadVersion(ctx)
	if err != nil {
		return nativeRangeResult{}, err
	}
	if sp := tx.startOpSpan("fdbgo.getRange"); sp != nil {
		defer sp.End()
	}
	r, err := tx.nativeGetRangeAt(ctx, begin, end, limits, readVersion, reverse)
	if err != nil {
		return nativeRangeResult{}, tx.mapReadError(ctx, err)
	}
	if !snapshot { // :5841-5844, filled by getRangeFinished (:4558-4586)
		c := getRangeFinishedConflict(begin, end, &r, reverse)
		r.conflict = &c
	}
	return r, nil
}

// nativeGetRangeAt is ::getRange (:4596-4893) after startTransaction, at
// readVersion.
func (tx *Transaction) nativeGetRangeAt(ctx context.Context, begin, end KeySelector, limits rangeLimits, readVersion int64, reverse bool) (nativeRangeResult, error) {
	tx.hadRead.Store(true)
	originalBegin, originalEnd, originalLimits := begin, end, limits // :4606-4608
	l := nativeRangeLoop{begin: begin, end: end, limits: limits, reverse: reverse}
	l.start()
	relocateRetries, timeoutRetries := 0, 0
	var materializedBytes int64
	var tags readTagSample
	redraw := true
	for !l.atEnd() { // :4631-4637
		locationKey, backward := l.location() // :4638-4639
		loc, err := tx.db.locCache.locate(tx.db, ctx, locationKey, tx.tenantId, tx.currentSpan(), backward)
		if err != nil {
			return nativeRangeResult{}, fmt.Errorf("locate range: %w", err)
		}
		if loc.ShardEnd == nil {
			loc.ShardEnd = allKeysEnd
		}
		reqBegin, reqEnd, modified := nativeRequestSelectors(l.begin, l.end, loc.ShardBegin, loc.ShardEnd, reverse) // :4651-4679
		if redraw {
			tags = tx.sampleReadTags() // one native request (:4684), before loadBalance waits
		}
		kvs, more, err := sendRangeRPC(tx, ctx, reqBegin, reqEnd, l.limits, readVersion, reverse, loc.Servers, tags, plainRangeRPCOps)
		redraw = err == nil || readTagRetry(err)
		if err != nil {
			switch {
			case isReplyTimeout(err):
				// The location is fine: re-send, bounded, as rangeScanImpl does.
				timeoutRetries++
				if timeoutRetries > maxReadTimeoutRetries {
					return nativeRangeResult{}, &wire.FDBError{Code: ErrTransactionTooOld}
				}
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return nativeRangeResult{}, err
				}
			case isWrongShardServer(err): // :4858-4870, invalidate then fall back
				tx.db.locCache.invalidate(locationKey, tx.tenantId, backward)
				return tx.nativeGetRangeFallback(ctx, originalBegin, originalEnd, originalLimits, readVersion, reverse)
			case isAllAlternativesFailed(err) || isMaybeDelivered(err): // :4858-4863, 4873
				relocateRetries++
				if relocateRetries > maxRelocateRetries {
					return nativeRangeResult{}, &wire.FDBError{Code: ErrTransactionTooOld}
				}
				tx.db.locCache.invalidate(locationKey, tx.tenantId, backward)
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return nativeRangeResult{}, err
				}
			default:
				return nativeRangeResult{}, err
			}
			continue
		}
		if ceiling := tx.db.rangeByteCeiling; ceiling > 0 { // RFC-115 §2, as rangeScanImpl
			for i := range kvs {
				materializedBytes += plainRangeScanOps.sizeBytes(&kvs[i])
			}
			if materializedBytes > ceiling {
				return nativeRangeResult{}, &RangeMaterializationLimitError{LimitBytes: ceiling, ReachedBytes: materializedBytes}
			}
		}
		step, err := l.onReply(kvs, more, modified, loc.ShardBegin, loc.ShardEnd)
		switch {
		case err != nil:
			return nativeRangeResult{}, err
		case step == nativeStepDone:
			return l.out, nil
		case step == nativeStepFallback: // :4831-4836
			return tx.nativeGetRangeFallback(ctx, originalBegin, originalEnd, originalLimits, readVersion, reverse)
		}
	}
	return l.out, nil // :4633-4636
}

// nativeRangeLoop is the state getRange's loop carries between replies: the
// selectors still to read, the limits left, and the output so far.
type nativeRangeLoop struct {
	begin, end KeySelector
	limits     rangeLimits
	reverse    bool
	out        nativeRangeResult
}

type nativeStep uint8

const (
	nativeStepNext     nativeStep = iota // send the next request
	nativeStepDone                       // out is the result
	nativeStepFallback                   // getRangeFallback with the original arguments
)

// start is the readToBegin prologue (:4623-4626).
func (l *nativeRangeLoop) start() {
	if len(l.begin.Key) == 0 && l.begin.Offset < 1 {
		l.out.readToBegin = true
		l.begin = firstGreaterOrEqual(l.begin.Key)
	}
}

// atEnd is the loop's exit test (:4632): end resolves to "".
func (l *nativeRangeLoop) atEnd() bool {
	return len(l.end.Key) == 0 && (l.end.Offset < 1 || l.end.isFirstGreaterOrEqual())
}

// location is the key whose shard the next request reads and whether it is
// looked up backward (:4638-4639): end - 1 reverse, begin forward.
func (l *nativeRangeLoop) location() ([]byte, bool) {
	if l.reverse {
		return l.end.Key, keySelectorIsBackward(l.end.OrEqual, l.end.Offset-1)
	}
	return l.begin.Key, l.begin.isBackward()
}

// nativeRequestSelectors is the request's selectors for the shard
// [shardBegin, shardEnd) (:4651-4679). Reverse, a begin that resolves before
// the shard becomes FGE(shardBegin), unless it already is that selector;
// forward, an end that resolves after the shard becomes FGE(shardEnd).
// modified reports either rewrite.
func nativeRequestSelectors(begin, end KeySelector, shardBegin, shardEnd []byte, reverse bool) (reqBegin, reqEnd KeySelector, modified bool) {
	reqBegin, reqEnd = begin, end
	beginLess := KeySelector{Key: begin.Key, OrEqual: begin.OrEqual, Offset: begin.Offset - 1} // begin - 1
	if reverse && beginLess.isDefinitelyLess(shardBegin) &&
		(!begin.isFirstGreaterOrEqual() || !bytes.Equal(begin.Key, shardBegin)) {
		reqBegin = firstGreaterOrEqual(shardBegin)
		modified = true
	}
	if !reverse && end.isDefinitelyGreater(shardEnd) {
		reqEnd = firstGreaterOrEqual(shardEnd)
		modified = true
	}
	return reqBegin, reqEnd, modified
}

// onReply consumes one reply of the request for [shardBegin, shardEnd)
// (:4751-4851) and says what the loop does next. An error is a reply C++
// asserts impossible (:4751-4752).
func (l *nativeRangeLoop) onReply(kvs []KeyValue, more, modified bool, shardBegin, shardEnd []byte) (nativeStep, error) {
	if (more && len(kvs) == 0) || (l.limits.hasRowLimit() && len(kvs) > int(l.limits.rows)) {
		return nativeStepNext, &wire.FDBError{Code: ErrInternalError}
	}
	size := 0
	if l.limits.hasByteLimit() {
		for i := range kvs {
			size += plainRangeScanOps.limitBytes(&kvs[i])
		}
	}
	l.limits.decrement(len(kvs), size) // :4754

	// :4756-4759
	if l.reverse && l.begin.isLastLessOrEqual() && len(kvs) > 0 && bytes.Equal(kvs[len(kvs)-1].Key, l.begin.Key) {
		modified = false
	}

	finished := l.limits.isReached() || (!modified && !more) || l.limits.hasSatisfiedMinRows() // :4761
	readThrough := modified && !more                                                           // :4762
	shardEdge := shardEnd
	if l.reverse {
		shardEdge = shardBegin
	}

	if finished && len(l.out.kvs) == 0 { // the first request got everything (:4765-4808)
		l.out.kvs = kvs[:len(kvs):len(kvs)] // fence spare capacity, as rangeScanImpl
		l.out.more = modified || l.limits.isReached() || more
		if readThrough {
			l.out.more = true
			l.out.readThrough = shardEdge
		}
		return nativeStepDone, nil
	}

	if len(l.out.kvs) == 0 { // :4810-4811
		l.out.kvs = kvs[:len(kvs):len(kvs)]
	} else {
		l.out.kvs = append(l.out.kvs, kvs...)
	}
	if finished { // :4813-4826
		l.out.more = modified || l.limits.isReached() || more
		if readThrough {
			l.out.readThrough = shardEdge
		}
		return nativeStepDone, nil
	}

	if !more { // :4828-4845
		if len(kvs) == 0 {
			return nativeStepFallback, nil
		}
		if l.reverse {
			l.end = firstGreaterOrEqual(shardBegin)
		} else {
			l.begin = firstGreaterOrEqual(shardEnd)
		}
		return nativeStepNext, nil
	}
	last := l.out.kvs[len(l.out.kvs)-1].Key // :4846-4851
	if l.reverse {
		l.end = firstGreaterOrEqual(last)
	} else {
		l.begin = firstGreaterThan(last)
	}
	return nativeStepNext, nil
}

// nativeGetRangeFallback is getRangeFallback (:4457-4500): resolve both
// selectors, with both resolutions outstanding together, then read the exact
// range between them at readVersion.
func (tx *Transaction) nativeGetRangeFallback(ctx context.Context, begin, end KeySelector, limits rangeLimits, readVersion int64, reverse bool) (nativeRangeResult, error) {
	rctx, cancel := context.WithCancel(ctx)
	var startup chan struct{}
	if !begin.isFirstGreaterOrEqual() && !begin.isFirstGreaterThan() &&
		!end.isFirstGreaterOrEqual() && !end.isFirstGreaterThan() {
		startup = make(chan struct{})
	}
	released := startup == nil
	releaseStartup := func() {
		if !released {
			close(startup)
			released = true
		}
	}
	var fb, fe *keyFuture
	defer func() { // cancel before releasing continuations, then join both actors
		cancel()
		releaseStartup()
		if fb != nil {
			<-fb.done
		}
		if fe != nil {
			<-fe.done
		}
	}()
	// Neither worker can resume its first wait while the parent's synchronous
	// resolveKey calls are still starting (:4465-4469).
	var parked <-chan struct{}
	fb, parked = tx.startResolveKey(rctx, begin, readVersion, startup)
	select {
	case <-parked:
	case <-rctx.Done():
		return nativeRangeResult{}, rctx.Err()
	}
	var endParked <-chan struct{}
	fe, endParked = tx.startResolveKey(rctx, end, readVersion, startup)
	awaitResolveKeyStart(rctx, endParked)
	releaseStartup()
	b, err := fb.wait() // :4468
	if err != nil {
		return nativeRangeResult{}, err
	}
	e, err := fe.wait() // :4469
	if err != nil {
		return nativeRangeResult{}, err
	}
	if bytes.Compare(b, e) >= 0 { // :4470-4472
		return nativeRangeResult{}, nil
	}
	kvs, more, _, err := tx.getRangeImpl(ctx, b, e, limits, readVersion, reverse) // getExactRange (:4478-4480)
	if err != nil {
		return nativeRangeResult{}, err
	}
	r := nativeRangeResult{kvs: kvs, more: more}
	r.readToBegin, r.readThroughEnd = fallbackFlags(b, e, more, reverse)
	return r, nil
}

// fallbackFlags is getRangeFallback's readToBegin (:4482-4483) and
// readThroughEnd (:4490-4491) for the exact range [b, e).
func fallbackFlags(b, e []byte, more, reverse bool) (readToBegin, readThroughEnd bool) {
	readToBegin = len(b) == 0 && ((reverse && !more) || !reverse)
	readThroughEnd = bytes.Equal(e, allKeysEnd) && ((!reverse && !more) || reverse)
	return readToBegin, readThroughEnd
}

type (
	resolveKeyStartObserver  struct{}
	resolveKeyWorkerObserver struct{}
)

func awaitResolveKeyStart(ctx context.Context, parked <-chan struct{}) {
	select {
	case <-parked:
		return
	default:
	}
	// Observe only an unready startup, without changing transaction layout or
	// the ready path. Tests use this context-scoped barrier before releasing end.
	if observe, ok := ctx.Value(resolveKeyStartObserver{}).(func()); ok {
		observe()
	}
	select {
	case <-parked:
	case <-ctx.Done():
	}
}

// keyFuture is a key being resolved.
type keyFuture struct {
	done chan struct{}
	key  []byte
	err  error
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func readyKeyFuture(key []byte) *keyFuture { return &keyFuture{done: closedChan, key: key} }

func (f *keyFuture) wait() ([]byte, error) {
	<-f.done
	return f.key, f.err
}

// startResolveKey is resolveKey (:4447-4455): FGE → its key, FGT → keyAfter(key),
// both ready with no request; otherwise a goroutine runs getKeyImpl at version rv.
// parked closes when that goroutine first blocks on the network (location
// lookup or GetKeyRequest), or finishes. The goroutine stops when ctx ends.
func (tx *Transaction) startResolveKey(ctx context.Context, sel KeySelector, rv int64, resume <-chan struct{}) (fut *keyFuture, parked <-chan struct{}) {
	if sel.isFirstGreaterOrEqual() {
		return readyKeyFuture(sel.Key), closedChan
	}
	if sel.isFirstGreaterThan() {
		return readyKeyFuture(keyAfterSelectorKey(sel.Key)), closedChan
	}
	fut = &keyFuture{done: make(chan struct{})}
	var beforeWait func()
	if hook := tx.beforeResolveKeyResume; hook != nil {
		beforeWait = func() { hook(sel) }
	}
	pctx, park := transport.WithSuspendHandoff(ctx, resume, beforeWait)
	go func() {
		// A client goroutine boundary (panicbackstop.go): every exit publishes the
		// result and closes done and parked, so the fallback's wait and cleanup
		// cannot be stranded; a panic fails the resolution with internal_error.
		defer park.Fire()
		defer close(fut.done)
		if tx.resolveKeyBackstop().run(func() {
			if observe, ok := pctx.Value(resolveKeyWorkerObserver{}).(func(KeySelector)); ok {
				observe(sel)
			}
			fut.key, _, fut.err = tx.getKeyImpl(pctx, rv, sel.Key, sel.OrEqual, sel.Offset)
		}) {
			fut.key, fut.err = nil, &wire.FDBError{Code: ErrInternalError}
		}
	}()
	return fut, park.Done()
}

// resolveKeyBackstop recovers a panic in a fallback's resolveKey goroutine.
func (tx *Transaction) resolveKeyBackstop() *panicBackstop {
	if tx.db == nil {
		return &panicBackstop{name: "resolveKey"}
	}
	return &tx.db.resolveKeyPanics
}

// getRangeFinishedConflict is the read conflict range getRangeFinished sends
// for a non-snapshot read (:4558-4586), from the call's selectors (after
// removeOrEqual) and its result.
func getRangeFinishedConflict(begin, end KeySelector, r *nativeRangeResult, reverse bool) [2][]byte {
	n := len(r.kvs)
	var rangeBegin, rangeEnd []byte
	switch {
	case r.readToBegin:
		rangeBegin = []byte{}
	case ((!reverse || !r.more || begin.Offset > 1) && begin.Offset > 0) || n == 0:
		rangeBegin = begin.Key
	case reverse:
		rangeBegin = r.kvs[n-1].Key
	default:
		rangeBegin = r.kvs[0].Key
	}
	if end.Offset > begin.Offset && bytes.Compare(end.Key, rangeBegin) < 0 {
		rangeBegin = end.Key
	}

	switch {
	case r.readThroughEnd:
		rangeEnd = allKeysEnd
	case ((reverse || !r.more || end.Offset <= 0) && end.Offset <= 1) || n == 0:
		rangeEnd = end.Key
	case reverse:
		rangeEnd = keyAfterSelectorKey(r.kvs[0].Key)
	default:
		rangeEnd = keyAfterSelectorKey(r.kvs[n-1].Key)
	}
	if begin.Offset < end.Offset && bytes.Compare(begin.Key, rangeEnd) > 0 {
		rangeEnd = begin.Key
	}
	return [2][]byte{rangeBegin, rangeEnd}
}

// The park seam. Flow runs resolveKey(begin) until its first wait that is not
// ready before it creates resolveKey(end) (:4465-4466); a goroutine has no such
// point, so the goroutine resolving begin carries a transport.SuspendSignal and
// every wait on its path fires it before blocking (transport/suspend.go): the
// dial wait (getOrDialConn), a full write queue, the flush acknowledgment, the
// writer turn, the reply wait (sendReadFrame), location backoff and delays
// (sleepCtx). On a ready path the frame is queued, in order, before the signal.
func withParkSignal(ctx context.Context) (context.Context, *transport.SuspendSignal) {
	return transport.WithSuspendSignal(ctx)
}

// notifyParked fires ctx's park signal, if it carries one.
func notifyParked(ctx context.Context) { transport.NotifySuspend(ctx) }
