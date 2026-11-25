package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"fdb.dev/internal/fdbgo/transport"
	"fdb.dev/internal/fdbwire"
	"fdb.dev/internal/fdbwire/types"
)

// getKeyValuesBufPool pools marshal buffers for GetKeyValues requests.
var getKeyValuesBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// getRange reads a key range [begin, end), fetching all overlapping shard locations
// at once and iterating them in scan order. Matches C++ getExactRange in
// NativeAPI.actor.cpp: re-queries same shard on more=true (no re-locate),
// invalidates entire remaining range on wrong_shard_server, and passes reverse
// to getKeyRangeLocations so the proxy returns shards in the right order.
func (tx *Transaction) getRange(parentCtx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	kvs, more, _, err := tx.getRangeReadThrough(parentCtx, begin, end, limit, byteTarget, reverse)
	return kvs, more, err
}

// getRangeReadThrough is getRange that also reports readThrough: where a read that
// stopped after its first shard's rows finished that shard (NativeAPI.actor.cpp:4795-4799).
func (tx *Transaction) getRangeReadThrough(parentCtx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error) {
	// Once per call, before GRV: C++ Transaction::getRangeInternal (NativeAPI.actor.cpp:5808).
	tx.db.metrics.getRangeRequests.Add(1)
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	readVersion, err := tx.operationReadVersion(ctx)
	if err != nil {
		return nil, false, nil, err
	}
	if sp := tx.startOpSpan("fdbgo.getRange"); sp != nil { // RFC-115 §4 Layer 2 (C++ NAPI:getRange)
		defer sp.End()
	}
	kvs, more, readThrough, err := tx.getRangeImpl(ctx, begin, end, newRangeLimits(limit, byteTarget), readVersion, reverse)
	return kvs, more, readThrough, tx.mapReadError(ctx, err)
}

// RangeMaterializationLimitError is returned by a GetRange that would materialize
// more than the opt-in WithRangeByteCeiling cap (RFC-115 §2) — an OOM safety valve,
// off by default. libfdb_c has no such ceiling (its GetSliceWithError equivalent also
// materializes unbounded and never returns a "too big" error), so this NEVER fires
// unless the operator opts in via WithRangeByteCeiling; the default facade behavior
// stays oracle-matching. Match it with errors.As. For large scans, prefer the bounded,
// StreamingMode-honoring Iterator() instead of raising the ceiling.
type RangeMaterializationLimitError struct {
	LimitBytes   int64 // the configured WithRangeByteCeiling
	ReachedBytes int64 // total key+value bytes materialized when the cap was exceeded
}

func (e *RangeMaterializationLimitError) Error() string {
	return fmt.Sprintf("fdbgo: GetRange materialized %d bytes, exceeding the configured WithRangeByteCeiling of %d bytes; use Iterator() for large/unbounded scans",
		e.ReachedBytes, e.LimitBytes)
}

// rangeScanOps supplies the only four things the shard-walking scan loop needs
// to know about its element type. C++ reaches the same factoring through
// templates: getExactRange is one templated ACTOR over GetKeyValuesRequest /
// GetMappedKeyValuesRequest (NativeAPI.actor.cpp), and there is exactly one
// shared getRangeFallback rather than a mapped-specific copy. Keeping ONE loop
// here means the shard-boundary walk, the wrong_shard_server relocate, the
// reply-timeout re-send, the reverse "fix more" heuristic and the limit
// semantics cannot drift between the plain and mapped paths — which is the
// whole reason this is generic rather than duplicated.
type rangeScanOps[T any] struct {
	// send issues one shard-local request at the scan's limits and read version
	// and parses its reply.
	send func(ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool, servers []ServerInfo, tags readTagSample, lb lbRetry) ([]T, bool, error, lbRetry)
	// key returns the PRIMARY key of an element. For a mapped row this is the
	// primary (index) row's key, never a secondary key: it drives shard
	// re-query bounds, which are bounds in the primary range only.
	key func(*T) []byte
	// sizeBytes is the element's contribution to the WithRangeByteCeiling
	// accounting. Called only when that opt-in ceiling is armed.
	sizeBytes func(*T) int64
	// limitBytes is C++ GetRangeLimits::decrement's charge against a byte target
	// (NativeAPI.actor.cpp:2811-2853): the primary row's key and value plus 8.
	limitBytes func(*T) int
}

var plainRangeScanOps = rangeScanOps[KeyValue]{
	key:        func(kv *KeyValue) []byte { return kv.Key },
	sizeBytes:  func(kv *KeyValue) int64 { return int64(len(kv.Key) + len(kv.Value)) },
	limitBytes: func(kv *KeyValue) int { return 8 + len(kv.Key) + len(kv.Value) },
}

func (tx *Transaction) getRangeImpl(ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool) ([]KeyValue, bool, []byte, error) {
	ops := plainRangeScanOps
	ops.send = tx.sendGetRangePaced
	return rangeScanImpl(tx, ctx, begin, end, limits, readVersion, reverse, ops)
}

type shardScanOutcome uint8

const (
	shardScanDone shardScanOutcome = iota
	shardScanRelocated
	shardScanReturnMore
)

type rangeScanState[T any] struct {
	tx                *Transaction
	readVersion       int64
	reverse           bool
	byteLimited       bool
	limits            rangeLimits
	allKVs            []T
	materializedBytes int64
	curBegin          []byte
	curEnd            []byte
	relocateRetries   int
	timeoutRetries    int
	lb                lbRetry
	firstReply        bool
	readThrough       []byte
	tags              readTagSample
	redraw            bool
}

// SOFT BYTE LIMIT — C++ getExactRange:4415:
//
//	// Soft byte limit - return results early if the user specified a byte limit and we got results
//	if (limits.hasSatisfiedMinRows() && output.size() > 0) { output.more = true; return output; }
//
// hasSatisfiedMinRows() is hasByteLimit() && minRows == 0 (:2875). A caller's own limits
// start with minRows 1 (FDBTypes.h GetRangeLimits ctor), so for them ANY non-empty reply
// satisfies it; a RYW request can ask for more (setRequestLimits). C++ checks it before
// re-querying a shard or moving to a location pair's second shard, but relocates after the
// second without checking (:4392-4407). A call whose first rows come from a pair's second
// shard therefore also reads the next pair's first, unless that shard's reply was cut short;
// getRange reaches that through its fallback from an empty first reply (:4828-4836). Each
// request carries the bytes left.
//
// This is the half that makes the request's byte target OBSERVABLE. Without it the loop
// absorbs a truncated reply and re-queries until the ROW budget fills, which is why
// lowering the request's byte limit ALONE changes no division — a real measurement that
// must not be read as "the request side is not involved". Both halves are required and
// neither reproduces the other: the server's reply-size accounting is not the client's, so
// a client-side byte budget over an untruncated reply cuts at a different row (measured: 19
// where C cuts 18 at a 4096-byte target — libfdbc:TestLibFDBC_ByteTargetCutIsNotTheClientSideBudget).
//
// EXACT falls out of this rather than being special-cased: mode_bytes_array[EXACT] is
// BYTE_LIMIT_UNLIMITED (fdb_c.cpp:1002), so the byte limit is unlimited, this can never fire,
// and the loop keeps absorbing across replies exactly as it did before.
//
// It is checked where the loop is about to fetch MORE data, never where the scan is
// finishing. C's early return sits AFTER the shard-exhaustion block that returns
// output.more = false (:4391-4400), so a range drained before its bytes run out still
// reports more=false; a check placed before that would label every completed byte-limited
// drain as having more.
func (s *rangeScanState[T]) softByteStop() bool {
	return s.limits.hasSatisfiedMinRows() && len(s.allKVs) > 0
}

// rangeScanImpl is NativeAPI getExactRange over [begin, end), consuming limits as
// replies arrive (NativeAPI.actor.cpp:4255-4444). Every request carries readVersion.
func rangeScanImpl[T any](tx *Transaction, ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool, ops rangeScanOps[T]) ([]T, bool, []byte, error) {
	tx.hadRead.Store(true) // a read was issued (RFC-059 poison signal)
	// C++ getExactRange locates GET_RANGE_SHARD_LIMIT (=2, ClientKnobs.cpp:101) shards at a time
	// (NativeAPI.actor.cpp:4272-4278) and the soft byte limit below depends on those pairs. Go
	// fetches more per locate RPC, always a whole number of pairs so pair origins coincide.
	const cppGetRangeShardLimit = 2
	const getRangeShardLimit = 50 * cppGetRangeShardLimit

	s := rangeScanState[T]{
		tx:          tx,
		readVersion: readVersion,
		reverse:     reverse,
		byteLimited: limits.hasByteLimit(),
		limits:      limits,
		curBegin:    begin,
		curEnd:      end,
		lb:          lbRetry{observe: tx.db.readBackoffObserver()},
		// C++ getRange's own loop, before any getRangeFallback, reports readThrough when the
		// first reply's rows end a shard the range continues past (NativeAPI.actor.cpp:4762,
		// 4795-4799). A byte-targeted call then stops; wrong_shard_server falls back (:4864-4870).
		firstReply: true,
		redraw:     true,
	}

	for !s.limits.isReached() && bytes.Compare(s.curBegin, s.curEnd) < 0 {
		// Get all shard locations for current range. C++ getKeyRangeLocations
		// receives the reverse flag so the proxy returns shards in scan order.
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, s.curBegin, s.curEnd, getRangeShardLimit, reverse, tx.tenantId, tx.currentSpan())
		if err != nil {
			return nil, false, nil, fmt.Errorf("locate range: %w", err)
		}
		if len(locations) == 0 {
			return s.allKVs, false, s.readThrough, nil
		}

		// C++ getExactRange iterates shard=0,1,2,... linearly. With reverse=true
		// on the request, locations[0] is already the shard nearest end.
		relocated := false
		for shard := 0; shard < len(locations) && !s.limits.isReached(); shard++ {
			loc := locations[shard]
			shardBegin, shardEnd, empty := clampShardRange(loc, s.curBegin, s.curEnd)
			if empty {
				continue // empty range for this shard
			}

			// About to fetch from a FURTHER shard on a byte-limited read that already has
			// rows. C++ reaches its soft-byte-limit return after ++shard for exactly this
			// case, so the call ends at its location pair — which is the point of the soft
			// limit: "This can prevent problems where the desired range spans many shards
			// and would be too slow to fetch entirely." A pair's first shard is fetched
			// without the check.
			if shard%cppGetRangeShardLimit != 0 && s.softByteStop() {
				return s.allKVs, true, s.readThrough, nil
			}

			outcome, err := s.scanShard(ctx, loc, shardBegin, shardEnd, ops)
			if err != nil {
				return nil, false, nil, err
			}
			if outcome == shardScanReturnMore {
				return s.allKVs, true, s.readThrough, nil
			}
			if outcome == shardScanRelocated {
				relocated = true
				break
			}
		}

		if relocated {
			continue // re-locate with adjusted curBegin/curEnd
		}

		var done bool
		s.curBegin, s.curEnd, done = advanceRangeLocations(locations, s.curBegin, s.curEnd, reverse)
		if done {
			break
		}
	}

	return s.allKVs, s.limits.rows == 0, s.readThrough, nil
}

// clampShardRange intersects a cached shard location with the remaining [curBegin, curEnd) range.
func clampShardRange(loc LocationResult, curBegin, curEnd []byte) (shardBegin, shardEnd []byte, empty bool) {
	shardBegin = loc.ShardBegin
	shardEnd = loc.ShardEnd
	if bytes.Compare(shardBegin, curBegin) < 0 {
		shardBegin = curBegin
	}
	if shardEnd == nil || bytes.Compare(shardEnd, curEnd) > 0 {
		shardEnd = curEnd
	}
	return shardBegin, shardEnd, bytes.Compare(shardBegin, shardEnd) >= 0
}

// advanceRangeLocations updates curBegin/curEnd after exhausting a locateRange batch.
func advanceRangeLocations(locations []LocationResult, curBegin, curEnd []byte, reverse bool) (nextBegin, nextEnd []byte, done bool) {
	if reverse {
		firstShard := locations[len(locations)-1]
		if bytes.Compare(firstShard.ShardBegin, curBegin) <= 0 {
			return curBegin, curEnd, true // first shard covers our begin, done
		}
		curEnd = firstShard.ShardBegin
	} else {
		lastShard := locations[len(locations)-1]
		if lastShard.ShardEnd == nil || bytes.Compare(lastShard.ShardEnd, curEnd) >= 0 {
			return curBegin, curEnd, true
		}
		curBegin = lastShard.ShardEnd
	}
	return curBegin, curEnd, bytes.Compare(curBegin, curEnd) >= 0
}

func (s *rangeScanState[T]) scanShard(ctx context.Context, loc LocationResult, shardBegin, shardEnd []byte, ops rangeScanOps[T]) (shardScanOutcome, error) {
	// Inner loop: re-query same shard while more=true (C++ stays on same
	// shard index, mutates locations[shard].range in-place).
	for !s.limits.isReached() {
		if s.redraw {
			s.tags = s.tx.sampleReadTags() // a request per send (NativeAPI.actor.cpp:4303, 4684)
		}
		kvs, more, err, nextRetry := ops.send(ctx, shardBegin, shardEnd, s.limits, s.readVersion, s.reverse, loc.Servers, s.tags, s.lb)
		s.lb = nextRetry
		s.redraw = err == nil || readTagRetry(err)
		if err != nil {
			relocated, retry, handleErr := s.handleScanError(ctx, shardBegin, shardEnd, err)
			if handleErr != nil {
				return 0, handleErr
			}
			if retry {
				continue // re-send same shard
			}
			if relocated {
				return shardScanRelocated, nil
			}
		}
		s.lb = lbRetry{observe: s.tx.db.readBackoffObserver()} // the next request is a new loadBalance call

		returnMore, nextBegin, nextEnd, nextShard, err := s.consumeBatch(loc, shardBegin, shardEnd, kvs, more, ops)
		if err != nil {
			return 0, err
		}
		if returnMore {
			return shardScanReturnMore, nil
		}
		if nextShard {
			break // move to next shard
		}
		shardBegin, shardEnd = nextBegin, nextEnd
	}
	return shardScanDone, nil
}

func (s *rangeScanState[T]) handleScanError(ctx context.Context, shardBegin, shardEnd []byte, err error) (relocated, retry bool, outErr error) {
	// Reply timeout (a slow-but-alive server): the location is
	// fine — re-send the SAME shard (no relocate). libfdb_c has no
	// per-read client timeout and waits until the server replies or
	// the read version ages to
	// transaction_too_old). Bounded; on exhaustion surface a
	// RETRYABLE transaction_too_old so the Transact loop retries
	// with a fresh read version. errReplyTimeout never escapes.
	if isReplyTimeout(err) {
		s.timeoutRetries++
		if s.timeoutRetries > maxReadTimeoutRetries {
			return false, false, &wire.FDBError{Code: ErrTransactionTooOld}
		}
		if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	// maybeDelivered (1030/1100: a failed connect or a teardown on
	// every replica tried) re-locates the same way but is
	// loadBalance's own unbounded retry, paced by lbRetry rather
	// than charged to relocateRetries (see getValueImpl).
	if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
		delay := wrongShardRetryDelay
		if isMaybeDelivered(err) {
			delay = 0 // each request was already paced by the sender
		} else if s.relocateRetries++; s.relocateRetries > maxRelocateRetries {
			// Surface a RETRYABLE transaction_too_old (not the
			// terminal all_alternatives_failed): the read path
			// absorbs all_alternatives_failed and lets the
			// transaction retry with a fresh locate, matching
			// libfdb_c's internal retry of the read.
			return false, false, &wire.FDBError{Code: ErrTransactionTooOld}
		}
		s.firstReply = s.firstReply && !isWrongShardServer(err)
		// C++ invalidates just the stale shard's range:
		// cx->invalidateCache(locations[shard].range).
		// Narrower than our previous whole-remaining-range invalidation.
		s.tx.db.locCache.invalidateRange(shardBegin, shardEnd, s.tx.tenantId)
		if s.reverse {
			s.curEnd = shardEnd
		} else {
			s.curBegin = shardBegin
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	return false, false, err
}

func (s *rangeScanState[T]) consumeBatch(loc LocationResult, shardBegin, shardEnd []byte, kvs []T, more bool, ops rangeScanOps[T]) (returnMore bool, nextBegin, nextEnd []byte, nextShard bool, err error) {
	if s.firstReply {
		s.firstReply = false
		if s.byteLimited && len(kvs) > 0 && !more {
			if !s.reverse && loc.ShardEnd != nil && bytes.Compare(loc.ShardEnd, s.curEnd) < 0 {
				s.readThrough = shardEnd
			} else if s.reverse && bytes.Compare(loc.ShardBegin, s.curBegin) > 0 {
				s.readThrough = shardBegin
			}
		}
	}

	if len(s.allKVs) == 0 && len(kvs) > 0 {
		// Parsers transfer fresh descriptors; fence spare capacity before later appends.
		s.allKVs = kvs[:len(kvs):len(kvs)]
	} else {
		s.allKVs = append(s.allKVs, kvs...)
	}

	// RFC-115 §2: opt-in OOM ceiling. Checked AFTER each batch append so a
	// runaway unbounded scan errors instead of OOM-ing; the overshoot is at
	// most one reply (~80 KB). 0 = unlimited (default, oracle-matching).
	if ceiling := s.tx.db.rangeByteCeiling; ceiling > 0 {
		for i := range kvs {
			s.materializedBytes += ops.sizeBytes(&kvs[i])
		}
		if s.materializedBytes > ceiling {
			return false, nil, nil, false, &RangeMaterializationLimitError{LimitBytes: ceiling, ReachedBytes: s.materializedBytes}
		}
	}

	size := 0
	if s.limits.hasByteLimit() {
		for i := range kvs {
			size += ops.limitBytes(&kvs[i])
		}
	}
	s.limits.decrement(len(kvs), size)
	// A row or byte limit met reports more regardless of the shard's own flag
	// (:4359-4362).
	if s.limits.isReached() {
		return true, nil, nil, false, nil
	}

	// C++ "fix more" heuristic (NativeAPI.actor.cpp:2331-2333):
	// If reverse scan's last returned key equals shard begin, the
	// shard is exhausted regardless of what more says.
	if more && s.reverse && len(kvs) > 0 &&
		bytes.Equal(ops.key(&kvs[len(kvs)-1]), shardBegin) {
		more = false
	}

	if !more {
		return false, nil, nil, true, nil
	}

	// C++ ASSERT: more=true with zero rows is impossible.
	// Guard against infinite loop on misbehaving storage server.
	if len(kvs) == 0 {
		return false, nil, nil, true, nil
	}

	// About to re-query the SAME shard because the reply was truncated. On a
	// byte-limited read that truncation IS the batch boundary, so the call ends
	// here instead of absorbing it.
	if s.softByteStop() {
		return true, nil, nil, false, nil
	}

	// Narrow range and re-query same shard (C++ mutates
	// locations[shard].range in-place, lines 2349-2354).
	lastKey := ops.key(&kvs[len(kvs)-1])
	if s.reverse {
		shardEnd = lastKey // [shardBegin, smallestReturnedKey)
	} else {
		shardBegin = append(append([]byte{}, lastKey...), 0) // keyAfter(lastKey)
	}
	return false, shardBegin, shardEnd, false, nil
}

func (tx *Transaction) sendGetRange(ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool, servers []ServerInfo, tags readTagSample) ([]KeyValue, bool, error) {
	return sendRangeRPC(tx, ctx, firstGreaterOrEqual(begin), firstGreaterOrEqual(end), limits, readVersion, reverse, servers, tags, plainRangeRPCOps)
}

func (tx *Transaction) sendGetRangePaced(ctx context.Context, begin, end []byte, limits rangeLimits, readVersion int64, reverse bool, servers []ServerInfo, tags readTagSample, lb lbRetry) ([]KeyValue, bool, error, lbRetry) {
	rows, more, err := sendRangeRPCPaced(tx, ctx, firstGreaterOrEqual(begin), firstGreaterOrEqual(end), limits, readVersion, reverse, servers, tags, plainRangeRPCOps, &lb)
	return rows, more, err, lb
}

// plainRangeRPCOps sends a GetKeyValuesRequest.
var plainRangeRPCOps = rangeRPCOps[KeyValue]{
	endpoint: EndpointGetKeyValues,
	build:    buildGetKeyValuesRequest,
	putBuf:   func(bufp *[]byte) { getKeyValuesBufPool.Put(bufp) },
	parse:    parseGetKeyValuesReply,
}

// rangeRPCOps is the per-request-kind half of the shard-local range RPC: which
// storage endpoint to hit, how to build the request, which pool its scratch
// buffer came from, and how to parse the reply. Everything else — hedging,
// QueueModel accounting, the connection-error and reply-timeout arms — is
// identical between getKeyValues and getMappedKeyValues in C++ (both go through
// the same loadBalance), so it is shared here rather than copied. A copy is how
// the QueueModel delta-leak fix (RFC-010 #5) would end up applied to one path
// and not the other.
type rangeRPCOps[T any] struct {
	endpoint int
	build    func(begin, end KeySelector, version int64, limit int32, limitBytes int32, lockAware bool, tenantId int64, span types.SpanContext, tags readTagSample, replyToken transport.UID, serverToken transport.UID) ([]byte, *[]byte)
	putBuf   func(*[]byte)
	parse    func([]byte) ([]T, bool, float64, error)
}

// limits.bytes is the per-fetch BYTE target left for this read (unlimited when the streaming
// mode has none). It is not the same thing as replyByteLimit: that is CLIENT_KNOBS->REPLY_BYTE_LIMIT,
// a flat ceiling on any single reply, while the target is the mode's own, usually much smaller,
// one — and it is the value the STORAGE SERVER truncates against, which is what actually
// divides a scan into batches. The request is sent at readVersion, the version the read's
// operation captured (NativeAPI.actor.cpp:4617-4618).
func sendRangeRPC[T any](tx *Transaction, ctx context.Context, begin, end KeySelector, limits rangeLimits, readVersion int64, reverse bool, servers []ServerInfo, tags readTagSample, ops rangeRPCOps[T]) ([]T, bool, error) {
	return sendRangeRPCPaced(tx, ctx, begin, end, limits, readVersion, reverse, servers, tags, ops, nil)
}

func sendRangeRPCPaced[T any](tx *Transaction, ctx context.Context, begin, end KeySelector, limits rangeLimits, readVersion int64, reverse bool, servers []ServerInfo, tags readTagSample, ops rangeRPCOps[T], lb *lbRetry) ([]T, bool, error) {
	alternatives := len(servers)
	// C++ transformRangeLimits, called from BOTH range loops (getExactRange:4299, getRange:4681).
	wireLimit, wireLimitBytes := transformRangeLimits(limits, reverse)
	servers, ok := tx.db.failMon.liveServers(servers, ops.endpoint)
	if !ok {
		return nil, false, tx.db.allAlternativesFailed(ctx)
	}

	alts := newReadAlternatives(tx.db, servers, alternatives, lb)

	// Capture tx fields before closures (see sendGetValue comment).
	lockAware := tx.lockAware || tx.readLockAware
	tenantId := tx.tenantId
	span := childSpanContext(tx.currentSpan()) // RFC-115 §4: per-op CHILD span context on the wire (C++ GetValueRequest(span.context):3677 — child of the tx span, not the tx span itself)

	makeSender := func(server ServerInfo) sendFunc {
		return func() inFlightRPC {
			conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
			if err != nil {
				tx.db.handleDialError(ctx, server.Address)
				return inFlightRPC{err: err, addr: server.Address}
			}
			gkvToken := getAdjustedEndpoint(server.Token, ops.endpoint)
			defer permit.Release()
			replyToken, replyCh, replyHandle := conn.PrepareReplyTo(gkvToken)
			replyCleanup := replyGuard{handle: replyHandle}
			defer replyCleanup.close()
			body, poolBuf := ops.build(begin, end, readVersion, wireLimit, wireLimitBytes, lockAware, tenantId, span, tags, replyToken, server.Token)

			delta := tx.db.queueModel.startRequest(server.Address)
			start := time.Now()

			if err := sendReadFrame(ctx, conn, gkvToken, body, replyHandle, permit, nil); err != nil {
				ops.putBuf(poolBuf) // the write queue owns its copy
				tx.db.queueModel.endRequest(server.Address, delta, readFailureLatency(err, start), false)
				replyHandle.Cancel()
				replyCleanup.release()
				tx.db.handleReadConnError(server.Address, conn, err)
				return inFlightRPC{err: err, addr: server.Address}
			}
			ops.putBuf(poolBuf)
			replyCleanup.transfer()
			return inFlightRPC{
				replyCh:     replyCh,
				replyHandle: replyHandle,
				addr:        server.Address,
				conn:        conn,
				delta:       delta,
				start:       start,
			}
		}
	}

	result := alts.start(ctx, makeSender, tx.readRPCTimeout())
	for {
		tx.db.finishReadRPC(result)
		err := result.err
		var kvs []T
		var more bool
		if err == nil {
			var penalty float64
			kvs, more, penalty, err = ops.parse(result.body)
			tx.db.noteReadBrokenPromise(err, servers, result.addr, ops.endpoint)
			tx.db.queueModel.endRequestFull(result.addr, result.delta, time.Since(result.start), err == nil, isFutureVersionOrProcessBehind(err), penalty)
			alts.fail(ctx, err)
		}
		if ctx.Err() != nil && err != nil {
			return nil, false, ctx.Err()
		}
		if !isMaybeDelivered(err) {
			return kvs, more, err
		}
		next, ok := alts.advance(ctx, makeSender, tx.readRPCTimeout())
		if !ok {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, err
		}
		result = next
	}
}

func buildGetKeyValuesRequest(begin, end KeySelector, version int64, limit int32, limitBytes int32, lockAware bool, tenantId int64, span types.SpanContext, tags readTagSample, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	req := types.GetKeyValuesRequest{
		Begin:                  begin.ref(),
		End:                    end.ref(),
		Version:                version,
		Limit:                  limit,
		LimitBytes:             limitBytes,
		Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SpanContext:            span, // RFC-115 §4
		SsLatestCommitVersions: emptyVersionVector,
	}
	tags.apply(&req.HasTags, &req.Tags)
	if lockAware {
		req.HasOptions = true
		req.Options = lockAwareReadOptions()
	}
	bufp := getKeyValuesBufPool.Get().(*[]byte)
	result := req.MarshalFDBPooled(*bufp)
	*bufp = result
	return result, bufp
}

// parseGetKeyValuesReply parses the ErrorOr-wrapped GetKeyValuesReply.
// Returns (keyValues, more, penalty, error).
func parseGetKeyValuesReply(data []byte) ([]KeyValue, bool, float64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return nil, false, -1.0, fmt.Errorf("GetKeyValues: %w", err)
	}
	var reply types.GetKeyValuesReply
	if err := reply.UnmarshalFromReader(&r); err != nil {
		return nil, false, -1.0, fmt.Errorf("GetKeyValues: %w", err)
	}
	// Inline LoadBalancedReply.error: the SS delivers wrong_shard_server etc. for
	// reads through this field, not the ErrorOr root. RFC-010 #1.
	if reply.HasError {
		return nil, false, reply.Penalty, &wire.FDBError{Code: int(reply.Error.ErrorCode)}
	}

	kvs, err := types.ParseKeyValueRefStringVector(reply.Data)
	if err != nil {
		return nil, false, reply.Penalty, fmt.Errorf("GetKeyValues: %w", err)
	}
	return kvs, reply.More, reply.Penalty, nil
}
