package client

import (
	"bytes"
	"context"
	"math"

	"fdb.dev/internal/fdbwire"
)

// localPointLocked evaluates the merged point view without changing the write
// map's dependence or unreadability (C++ RYWIterator::kv).
func (c *rywCache) localPointLocked(w *rywWrites, key []byte) ([]byte, bool, error) {
	entry, written := w.entryOf(key)
	if !w.bypassUnreadable && ((written && entry.unreadable) || w.isUnreadable(key)) {
		return nil, true, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	if written {
		if !entry.hasAtomics {
			if entry.absent {
				return nil, true, nil
			}
			return entry.value, true, nil
		}
		base, known := c.serverCache.getKey(key)
		if !known && !(len(entry.atomics) > 0 && isUnresolvedVersionstamp(entry.atomics[0].typ)) {
			return nil, false, nil
		}
		value, err := resolvePointAtomics(base, entry.atomics, w.bypassUnreadable)
		return value, true, err
	}
	if w.isCleared(key) {
		return nil, true, nil
	}
	value, known := c.serverCache.getKey(key)
	return value, known, nil
}

// resolvePointAtomics is RYWIterator::kv for a pending atomic stack over a known
// storage base (nil = absent).
func resolvePointAtomics(base []byte, atomics []rywMutation, bypassUnreadable bool) ([]byte, error) {
	if bypassUnreadable {
		value, cleared := resolveAtomicsBypass(base, atomics)
		if cleared {
			return nil, nil
		}
		return value, nil
	}
	value, cleared, unreadable := resolveAtomics(base, atomics)
	if unreadable {
		return nil, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	if cleared {
		return nil, nil
	}
	return value, nil
}

// get intercepts a single-key read and merges with pending writes.
//
// A dependent read resolves the atomic stack it was issued against: C++ re-skips
// the read's own RYWIterator after the storage wait (ReadYourWrites.actor.cpp:127),
// and that iterator's write-map version is unaffected by later writes
// (WriteMap.h:158-160). Operands are copied because the stack outlives the lock.
func (c *rywCache) get(ctx context.Context, key []byte, serverGet func(ctx context.Context, key []byte) ([]byte, error)) ([]byte, error) {
	r := c.issueGet(key)
	return r.finish(ctx, serverGet)
}

// pointRead is a point read past its issue: decided by the writes and cache then, or a server
// read merged with the writes captured then (RYW get, ReadYourWrites.actor.cpp:1631-1693).
type pointRead struct {
	c            *rywCache
	key          []byte
	known        bool // answered at issue
	value        []byte
	err          error
	atomics      []rywMutation // the key's pending atomics at issue, applied over the server's value
	bypass       bool
	snapshotOnly bool // the cache, then the server: snapshot reads with RYW disabled for them
}

// issueGet is get's part in the call. The key is the caller's: finish needs it
// only while the caller holds it, or a copy.
func (c *rywCache) issueGet(key []byte) pointRead {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.liveLocked()
	r := pointRead{c: c, key: key, bypass: w.bypassUnreadable}
	r.value, r.known, r.err = c.localPointLocked(w, key)
	if entry, written := w.entryOf(key); !r.known && written && entry.hasAtomics {
		r.atomics = make([]rywMutation, len(entry.atomics))
		for i, m := range entry.atomics {
			r.atomics[i] = rywMutation{typ: m.typ, param: bytes.Clone(m.param)}
		}
	}
	return r
}

// issueGetSnapshot is getSnapshot's part in the call: the snapshot cache's answer.
func (c *rywCache) issueGetSnapshot(key []byte) pointRead {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := pointRead{c: c, key: key, snapshotOnly: true}
	r.value, r.known = c.serverCache.getKey(key)
	return r
}

// finish answers the read: as issued, or from the cache or the server, merged
// with the atomics captured at issue.
func (r *pointRead) finish(ctx context.Context, serverGet func(context.Context, []byte) ([]byte, error)) ([]byte, error) {
	if r.known {
		return r.value, r.err
	}
	if r.snapshotOnly {
		value, err := serverGet(ctx, r.key)
		if err == nil {
			r.c.cachePointResult(r.key, value)
		}
		return value, err
	}
	base, err := r.c.getSnapshot(ctx, r.key, serverGet)
	if err != nil || r.atomics == nil {
		return base, err
	}
	return resolvePointAtomics(base, r.atomics, r.bypass)
}

func (c *rywCache) getSnapshot(ctx context.Context, key []byte, serverGet func(context.Context, []byte) ([]byte, error)) ([]byte, error) {
	r := c.issueGetSnapshot(key)
	return r.finish(ctx, serverGet)
}

func (c *rywCache) cachePointResult(key, value []byte) {
	c.mu.Lock()
	c.serverCache.insertPoint(key, value)
	c.mu.Unlock()
}

// rywRangeRead is a range read past its RYW issue; its merge and conflict filter use the
// write state captured then (ReadYourWrites.actor.cpp:382, WriteMap.h:158).
type rywRangeRead struct {
	c                 *rywCache
	begin, end        []byte
	limit, byteTarget int
	reverse           bool
	known             bool // answered from local state at issue
	rows              []KeyValue
	more              bool
	err               error
	unreadableCap     []byte
	writesAtIssue     bool           // local writes or clears in [begin, end) at issue
	view              *rywWrites     // their state, a private copy for a read that must fetch
	localConflicts    []conflictSpan // a local answer's filtered read conflict, computed at issue
}

// issueRangeInto answers the read from local state when it can, deciding its read conflict in the
// same critical section; only a read that must fetch copies the write span it will merge against.
func (c *rywCache) issueRangeInto(r *rywRangeRead, begin, end []byte, limit, byteTarget int, reverse, wantConflicts bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*r = rywRangeRead{c: c, begin: begin, end: end, limit: limit, byteTarget: byteTarget, reverse: reverse}
	w := c.liveLocked()
	r.writesAtIssue = w.hasWritesInRange(begin, end) || w.hasClearsInRange(begin, end)
	if rows, more, known, err := c.localRangeLocked(w, begin, end, limit, byteTarget, reverse); known {
		r.known, r.rows, r.more, r.err = true, rows, more, err
		if wantConflicts && err == nil && r.writesAtIssue {
			if cb, ce := rangeConflictExtent(begin, end, rows, more, reverse); bytes.Compare(cb, ce) < 0 {
				r.localConflicts = w.appendConflictSpans(nil, cb, ce)
			}
		}
		return
	}
	if r.writesAtIssue {
		r.view = c.captureWriteSpanLocked(begin, end)
	}
	// Unreadable reach cap (RFC-098): truncate the scan window at the first
	// (forward) / last (reverse) unreadable position; iteration that would
	// CROSS the cap throws accessed_unreadable, results before it emit
	// normally (C++ reach semantics, ReadYourWrites.actor.cpp:685 vs :692).
	// Computed BEFORE the fast-path branch: an SVK candidate range can cover
	// spans with no local write entries at all.
	r.unreadableCap = w.unreadableScanCap(begin, end, reverse)
	if r.view == nil {
		r.known, r.rows, r.more, r.err = c.cachedRangeLocked(begin, end, limit, byteTarget, reverse, r.unreadableCap)
	}
}

// cachedRangeLocked answers a range with no local writes from the snapshot cache
// when it holds all of the range (capped at an unreadable position).
func (c *rywCache) cachedRangeLocked(begin, end []byte, limit, byteTarget int, reverse bool, unreadableCap []byte) (bool, []KeyValue, bool, error) {
	if unreadableCap != nil {
		if reverse {
			begin = unreadableCap
		} else {
			end = unreadableCap
		}
	}
	cachedKVs, fullyKnown := c.serverCache.getRangeKVsLimited(begin, end, cacheWalkBudget(limit), reverse)
	if !fullyKnown {
		return false, nil, false, nil
	}
	kvs := applyLimitAndDirection(cachedKVs, limit, reverse)
	kvs, remainingBytes := applyRangeByteLimit(kvs, byteTarget)
	bytesReached := byteTarget > 0 && remainingBytes <= 0
	// Reaching the unreadable cap without filling a limit throws (RFC-098).
	if !bytesReached && unreadableCap != nil && (limit <= 0 || len(kvs) < limit) {
		return true, nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	return true, kvs, bytesReached || computeMore(cachedKVs, limit) || limitReached(limit, len(kvs)) || unreadableCap != nil, nil
}

// conflictRanges filters [begin, end) through the issue-time write state (view must be set):
// C++ addConflictRange walks the read's own iterator (ReadYourWrites.actor.cpp:388).
// The view is this read's private copy, sharing only immutable nodes, so no lock is held.
func (r *rywRangeRead) conflictRanges(begin, end []byte) []conflictSpan {
	if bytes.Compare(begin, end) >= 0 {
		return nil
	}
	return r.view.appendConflictSpans(nil, begin, end)
}

// rangeServerFunc is the storage range read under the RYW layer. readThrough, when set, is
// where the read finished its last shard (NativeAPI.actor.cpp:4795-4799).
type rangeServerFunc func(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error)

// run fetches what local state did not answer and merges it, re-fetching past locally cleared
// rows rather than truncating, as the C++ RYWIterator does (ReadYourWrites.actor.cpp:685-686).
func (r *rywRangeRead) run(ctx context.Context, serverGetRange rangeServerFunc) ([]KeyValue, bool, error) {
	if r.known {
		return r.rows, r.more, r.err
	}
	begin, end := r.begin, r.end
	if r.unreadableCap != nil {
		if r.reverse {
			begin = r.unreadableCap
		} else {
			end = r.unreadableCap
		}
	}
	if r.view == nil {
		return r.runNoWrites(ctx, begin, end, serverGetRange)
	}
	return r.runSlow(ctx, begin, end, serverGetRange)
}

// unreadableReached reports whether iteration of the capped window would continue
// INTO the unreadable position: the limit was not filled inside the window (an
// unlimited scan always reaches).
func unreadableReached(unreadableCap []byte, limit, got int) bool {
	return unreadableCap != nil && (limit <= 0 || got < limit)
}

func (r *rywRangeRead) runNoWrites(ctx context.Context, begin, end []byte, serverGetRange rangeServerFunc) ([]KeyValue, bool, error) {
	c, limit, byteTarget, reverse, unreadableCap := r.c, r.limit, r.byteTarget, r.reverse, r.unreadableCap
	// No local mutations, and the snapshot cache did not answer at issue.
	kvs, more, err := c.fetchServer(ctx, begin, end, limit, rywFirstRequestBytes(byteTarget), reverse, serverGetRange)
	if err != nil {
		return nil, false, err
	}
	cut, remainingBytes := applyRangeByteLimit(kvs, byteTarget)
	bytesReached := byteTarget > 0 && remainingBytes <= 0
	if byteTarget > 0 && more && !bytesReached && c.replyEdgeKnown(begin, end, kvs, reverse) {
		// Re-walk storage only, within the issue-time cap: the read was issued
		// with no writes in range, and later writes must not join it.
		if rows, rowsMore, known, err := c.localRange(nil, begin, end, limit, byteTarget, reverse); known {
			if err != nil {
				return nil, false, err
			}
			if !rowsMore && unreadableReached(unreadableCap, limit, len(rows)) {
				return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
			}
			return rows, rowsMore || unreadableCap != nil, nil
		}
	}
	if !more && !bytesReached && unreadableReached(unreadableCap, limit, len(cut)) {
		return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	// A byte-limited page ends like the cache re-walk, which reports a filled row limit.
	limitStop := byteTarget > 0 && limitReached(limit, len(cut))
	return cut, more || bytesReached || limitStop || unreadableCap != nil, nil
}

func (r *rywRangeRead) runSlow(ctx context.Context, begin, end []byte, serverGetRange rangeServerFunc) ([]KeyValue, bool, error) {
	c, limit, byteTarget, reverse := r.c, r.limit, r.byteTarget, r.reverse
	unreadableCap, view := r.unreadableCap, r.view
	requestedBegin, requestedEnd := r.begin, r.end

	// Slow path: iterative fetch + merge. Loop until we either fill
	// the limit or the server is exhausted for the remaining range.
	var result []KeyValue
	remainingBytes := byteTarget
	remaining := limit
	if remaining <= 0 {
		remaining = math.MaxInt // C++ ROW_LIMIT_UNLIMITED: 0 or negative = no limit
	}
	curBegin := begin
	curEnd := end
	requestCount := 0
	countedGetRange := func(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error) {
		requestCount++
		return serverGetRange(ctx, begin, end, limit, byteTarget, reverse)
	}

	for remaining > 0 && bytes.Compare(curBegin, curEnd) < 0 {
		// Fetch from server with headroom to compensate for clears.
		// Cap at 10000 before doubling to avoid overflow when remaining=math.MaxInt.
		fetchLimit := 10000
		if remaining <= 5000 {
			fetchLimit = remaining * 2
			if fetchLimit < 256 {
				fetchLimit = 256
			}
		}

		serverKVs, serverMore, err := c.fetchOrCached(ctx, curBegin, curEnd, fetchLimit, rywRequestBytes(remainingBytes, requestCount), reverse, countedGetRange)
		if err != nil {
			return nil, false, err
		}
		if byteTarget > 0 {
			if rows, more, known, err := c.localRange(view, requestedBegin, requestedEnd, limit, byteTarget, reverse); known {
				return rows, more, err
			}
		}

		// Knowledge boundary: when serverMore=true, we only know the DB
		// state up to the last returned key. Writes beyond this boundary
		// MUST NOT be included — un-fetched server keys may interleave.
		// When serverMore=false, boundary is nil → all writes in range
		// are safe to include.
		var boundary []byte
		if serverMore && len(serverKVs) > 0 {
			boundary = serverKVs[len(serverKVs)-1].Key
		}

		batch := c.mergeBatch(view, serverKVs, curBegin, curEnd, boundary, reverse)

		take := len(batch)
		if take > remaining {
			take = remaining
		}
		batch, remainingBytes = applyRangeByteLimit(batch[:take], remainingBytes)
		result = append(result, batch...)
		remaining -= len(batch)

		if remaining <= 0 || (byteTarget > 0 && remainingBytes <= 0) {
			// C++ reports more whenever a limit stopped iteration, including an
			// exactly-filled final page; do not reach the unreadable cap below.
			return result, true, nil
		}

		if !serverMore {
			// Server exhausted the (possibly capped) window without filling
			// the limit: iteration would continue INTO the unreadable
			// position — that is the REACH (RFC-098).
			if unreadableReached(unreadableCap, limit, len(result)) {
				return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
			}
			// All writes included. Done.
			return result, false, nil
		}

		if byteTarget > 0 && len(result) > 0 {
			return result, true, nil // C++ soft byte stop before the next unknown span.
		}
		// Server had more data, but we still need results.
		// Advance the scan range past the last fetched server key.
		if len(serverKVs) == 0 {
			break // Shouldn't happen: serverMore=true with 0 results.
		}

		if reverse {
			curEnd = serverKVs[len(serverKVs)-1].Key // [curBegin, lastKey)
		} else {
			// keyAfter(lastKey): append \x00 to step past the last fetched key.
			lastKey := serverKVs[len(serverKVs)-1].Key
			curBegin = append(append([]byte{}, lastKey...), 0)
		}
	}

	if unreadableReached(unreadableCap, limit, len(result)) {
		// The capped window was exhausted without filling the limit (RFC-098).
		return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	return result, false, nil
}

func (c *rywCache) getSnapshotRange(
	ctx context.Context,
	begin, end []byte,
	limit, byteTarget int,
	reverse bool,
	serverGetRange rangeServerFunc,
) ([]KeyValue, bool, error) {
	rows, more, known, err := c.localRange(nil, begin, end, limit, byteTarget, reverse)
	if known {
		return rows, more, err
	}
	rows, more, err = c.fetchServer(ctx, begin, end, limit, rywFirstRequestBytes(byteTarget), reverse, serverGetRange)
	if err == nil {
		cut, remainingBytes := applyRangeByteLimit(rows, byteTarget)
		bytesReached := byteTarget > 0 && remainingBytes <= 0
		if byteTarget > 0 && more && !bytesReached && c.replyEdgeKnown(begin, end, rows, reverse) {
			if rows, more, known, err := c.localRange(nil, begin, end, limit, byteTarget, reverse); known {
				return rows, more, err
			}
		}
		return cut, more || bytesReached || (byteTarget > 0 && limitReached(limit, len(cut))), nil
	}
	return rows, more, err
}

// replyEdgeKnown reports whether a more=true storage reply is followed by cached
// state, so only the snapshot-cache walk can tell where the page ends.
func (c *rywCache) replyEdgeKnown(begin, end []byte, kvs []KeyValue, reverse bool) bool {
	if len(kvs) == 0 {
		return true
	}
	last := kvs[len(kvs)-1].Key
	if (!reverse && isKeyAfter(end, last)) || (reverse && bytes.Equal(last, begin)) {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverCache.knownPast(last, reverse)
}

// captureWriteSpanLocked copies the write-map state intersecting [begin, end) into a
// private version the old range loop merges against: C++ iterates a WriteMap version
// later writes do not change (WriteMap.h:158-160). The copy is never c.w and never
// pinned, so mergeBatchLocked may fold resolved atomics into it. It has no nodes; its
// cursor keeps the legacy boundary sources until phase 3 deletes the loop (RFC-262 D2).
// The span shares the live version's nodes and atomic stacks, so it costs O(log n).
// Re-stamping the live version here keeps later folds from writing those stacks in place.
func (c *rywCache) captureWriteSpanLocked(begin, end []byte) *rywWrites {
	w := c.liveLocked()
	w.ensureSorted()
	c.nextVer++
	w.ver = c.nextVer
	ui := w.unreadableKeys.search(func(k *string) bool { return *k >= string(begin) })
	uj := w.unreadableKeys.search(func(k *string) bool { return *k >= string(end) })
	return &rywWrites{
		sorted:           w.sorted.slice(w.writeIndexOf(begin), w.writeIndexOf(end)),
		isSorted:         true,
		cleared:          overlappingRanges(&w.cleared, begin, end),
		unreadableRanges: overlappingRanges(&w.unreadableRanges, begin, end),
		unreadableKeys:   w.unreadableKeys.slice(ui, uj),
		bypassUnreadable: w.bypassUnreadable,
	}
}

// overlappingRanges returns the sorted, non-overlapping ranges intersecting [begin, end), unclipped.
func overlappingRanges(ranges *pseq[rywRange], begin, end []byte) pseq[rywRange] {
	lo := ranges.search(func(r *rywRange) bool { return bytes.Compare(r.end, begin) > 0 })
	hi := ranges.search(func(r *rywRange) bool { return bytes.Compare(r.begin, end) >= 0 })
	return ranges.slice(lo, hi)
}

// rywRequestBytes is C++ setRequestLimits' byte limit for a storage read of a
// RYW range call: min(byteTarget << min(requestCount, 20), REPLY_BYTE_LIMIT)
// (ReadYourWrites.actor.cpp:569-590, 748-753).
func rywRequestBytes(byteTarget, requestCount int) int {
	if byteTarget <= 0 {
		return byteTarget
	}
	return int(min(int64(min(byteTarget, replyByteLimit))<<min(requestCount, 20), int64(replyByteLimit)))
}

// rywFirstRequestBytes is C++ setRequestLimits' byte limit for the first storage read of a
// RYW range call (requestCount 0): never above REPLY_BYTE_LIMIT, while the rows returned are
// still cut against the caller's own target (ReadYourWrites.actor.cpp:569-590, 748-753).
func rywFirstRequestBytes(byteTarget int) int {
	return rywRequestBytes(byteTarget, 0)
}

// applyRangeByteLimit includes the row that exhausts the budget, guaranteeing
// progress even when one row is larger than the requested page.
func applyRangeByteLimit(rows []KeyValue, remaining int) ([]KeyValue, int) {
	if remaining > 0 {
		for i, row := range rows {
			remaining -= 8 + len(row.Key) + len(row.Value)
			if remaining <= 0 {
				return rows[:i+1], remaining
			}
		}
	}
	return rows, remaining
}
