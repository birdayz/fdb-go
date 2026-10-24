package client

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// GetEstimatedRangeSizeBytes returns an estimate of the byte size of the given
// key range. Matches C++ getStorageMetricsLargeKeyRange in NativeAPI.actor.cpp:
// gets all shard locations, sends WaitMetricsRequest to each with min.bytes=0,
// max.bytes=-1 (reversed range = immediate response), and sums the bytes.
func (tx *Transaction) GetEstimatedRangeSizeBytes(parentCtx context.Context, begin, end []byte) (int64, error) {
	// Bound the locate + WaitMetrics RPCs by SetTimeout (RFC-112): C++ wraps this in
	// waitOrError(getStorageMetrics(...), resetPromise) (ReadYourWrites.actor.cpp:1863),
	// so a hung shard-metrics RPC must surface transaction_timed_out once the timeout is delivered.
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	n, err := tx.getEstimatedRangeSizeBytesImpl(ctx, begin, end)
	return n, tx.mapReadError(ctx, err)
}

func (tx *Transaction) getEstimatedRangeSizeBytesImpl(ctx context.Context, begin, end []byte) (int64, error) {
	// inverted_range (2005) first — same KeyRangeRef-construction semantics as getRangeSplitPoints:
	// libfdb_c's C-API range construction throws inverted_range on begin > end before the metric op
	// runs, so Go (raw begin/end) checks it here. (Unlike getRangeSplitPoints there is NO maxKey check
	// — C++ getEstimatedRangeSizeBytes/getStorageMetrics, ReadYourWrites.actor.cpp:1853, has none.)
	if bytes.Compare(begin, end) > 0 {
		return 0, &wire.FDBError{Code: ErrInvertedRange} // 2005
	}
	// Without a tenant this path bypasses GRV, but uses the same captured
	// deferred-before-resetPromise entry verdict, which also returns a delivered
	// timeout (1031). Range construction above precedes dispatch.
	if err := tx.readEntryError(ctx); err != nil {
		return 0, err
	}
	minVersion, err := tx.metricsMinVersion(ctx)
	if err != nil {
		return 0, err
	}
	// C++ uses std::numeric_limits<int>::max() — get ALL locations at once.
	const shardLimit = math.MaxInt32

	// No attempt cap: C++ retries until the transaction times out (NativeAPI.actor.cpp:7799-7811).
	for {
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, shardLimit, false, tx.tenantId, tx.currentSpan())
		if err != nil {
			return 0, fmt.Errorf("locate range for metrics: %w", err)
		}

		var total int64
		retry := false
		for _, loc := range locations {
			// Clamp shard boundaries to requested range (same pattern as readpath.go).
			shardBegin := loc.ShardBegin
			shardEnd := loc.ShardEnd
			if bytes.Compare(shardBegin, begin) < 0 {
				shardBegin = begin
			}
			if shardEnd == nil || bytes.Compare(shardEnd, end) > 0 {
				shardEnd = end
			}
			if bytes.Compare(shardBegin, shardEnd) >= 0 {
				continue // empty range for this shard
			}

			b, err := tx.sendWaitMetrics(ctx, shardBegin, shardEnd, minVersion, loc.Servers)
			if err != nil {
				if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
					tx.db.locCache.invalidateRange(begin, end, tx.tenantId)
					if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
						return 0, err
					}
					retry = true
					break
				}
				// C++ catches future_version (1009), delays, and retries.
				if isFutureVersion(err) {
					if err := sleepCtx(ctx, futureVersionDelay); err != nil {
						return 0, err
					}
					retry = true
					break
				}
				return 0, err
			}
			total += b
		}
		if !retry {
			return total, nil
		}
	}
}

// isFutureVersion returns true if the error is FDB error 1009.
func isFutureVersion(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && code == ErrFutureVersion
}

// metricsMinVersion is the version a WaitMetricsRequest asks for. C++ passes
// the transaction state, and so starts the transaction and reads at its
// version, only for a tenant (ReadYourWrites.actor.cpp:1860-1863;
// NativeAPI.actor.cpp:7820-7824); otherwise it asks for the latest version and
// sends no GRV. C++ only starts the transaction here, without the reads'
// validateVersion (NativeAPI.actor.cpp:511-523): an explicit read version below
// the client's floor is sent as is, and the storage server clamps it.
func (tx *Transaction) metricsMinVersion(ctx context.Context) (int64, error) {
	if tx.tenantId < 0 {
		return LatestVersion, nil
	}
	rv, _, err := tx.cachedOrFetchReadVersion(ctx, 0)
	return rv, err
}

// sendWaitMetrics sends a WaitMetricsRequest to a storage server and returns
// the bytes field from the StorageMetrics reply. Uses min.bytes=0, max.bytes=-1
// (reversed range) to get an immediate response instead of waiting for a
// threshold change.
func (tx *Transaction) sendWaitMetrics(ctx context.Context, begin, end []byte, minVersion int64, servers []ServerInfo) (int64, error) {
	body, err := tx.sendMetricsRequest(ctx, servers, EndpointWaitMetrics, func(reply transport.UID) []byte {
		req := types.WaitMetricsRequest{
			Keys:       types.KeyRangeRef{Begin: begin, End: end},
			Min:        types.StorageMetrics{Bytes: 0},
			Max:        types.StorageMetrics{Bytes: -1},
			Reply:      types.ReplyPromise{Token: wire.UIDFromParts(reply.First, reply.Second)},
			TenantInfo: types.TenantInfo{TenantId: tx.tenantId},
			MinVersion: minVersion,
		}
		return req.MarshalFDB()
	})
	if err != nil {
		return 0, err
	}
	return parseWaitMetricsReply(body)
}

// sendMetricsRequest sends a request as loadBalance does (LoadBalance.actor.h:589-686):
// no reply timeout, and 1006 only once every replica stays failed for the delay.
func (tx *Transaction) sendMetricsRequest(ctx context.Context, servers []ServerInfo, endpoint int, build func(reply transport.UID) []byte) ([]byte, error) {
	var closed <-chan struct{} // Close ends the loop, as destroying the database cancels the actor
	if tx.db.ctx != nil {
		closed = tx.db.ctx.Done()
	}
	var backoff time.Duration
	for {
		// Endpoints reported not found never recover, so each round drops them first.
		live, ok := tx.db.failMon.liveServers(servers, endpoint)
		if !ok {
			return nil, tx.db.allAlternativesFailed(ctx)
		}
		// Excluded replicas are skipped; with none left, wait before sending (:595-639).
		if recovered, wait := tx.db.altFailureWait(live); wait > 0 {
			if err := tx.db.awaitAlternative(ctx, closed, live, recovered, wait); err != nil {
				return nil, err
			}
			backoff = 0
		}
		if body, ok := tx.metricsRound(ctx, tx.db.notExcluded(live), endpoint, build); ok {
			return body, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if tx.db.ctx != nil && tx.db.ctx.Err() != nil {
			return nil, tx.db.ctx.Err()
		}
		backoff = time.Duration(math.Min(math.Max(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceStartBackoff)), float64(loadBalanceMaxBackoff)))
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-closed:
			timer.Stop()
			return nil, tx.db.ctx.Err()
		}
	}
}

// metricsRound sends the request to each replica in turn until one replies.
func (tx *Transaction) metricsRound(ctx context.Context, servers []ServerInfo, endpoint int, build func(reply transport.UID) []byte) ([]byte, bool) {
	var replyCleanup replyGuard
	defer replyCleanup.close()
	var sendCleanup sendGuard
	defer sendCleanup.close()
	for _, server := range servers {
		conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
		if err != nil {
			tx.db.handleDialError(ctx, server.Address)
			continue
		}
		sendCleanup.permit = permit
		token := getAdjustedEndpoint(server.Token, endpoint)
		replyToken, replyCh, replyHandle := conn.PrepareReplyTo(token)
		replyCleanup.handle = replyHandle
		if err := sendReadFrame(ctx, conn, token, build(replyToken), replyHandle, permit, nil); err != nil {
			replyHandle.Cancel()
			replyCleanup.release()
			tx.db.handleReadConnError(server.Address, conn, err)
			continue
		}
		select {
		case resp := <-replyCh:
			replyCleanup.release()
			if resp.Err != nil {
				tx.db.handleReadConnError(server.Address, conn, resp.Err)
				continue
			}
			// An in-band broken_promise or request_maybe_delivered is not a response:
			// loadBalance moves on to the next replica (LoadBalance.actor.h:344-377).
			if err := wire.ReadErrorOrInto(resp.Body, new(wire.Reader)); isMaybeDelivered(err) {
				tx.db.noteBrokenPromise(err, server.Address, token)
				continue
			}
			return resp.Body, true
		case <-ctx.Done():
			replyHandle.Cancel()
			replyCleanup.release()
			return nil, false
		}
	}
	return nil, false
}

// GetRangeSplitPoints returns suggested split points for the given key range.
// Matches C++ Transaction::getRangeSplitPoints in NativeAPI.actor.cpp.
func (tx *Transaction) GetRangeSplitPoints(parentCtx context.Context, begin, end []byte, chunkSize int64) ([][]byte, error) {
	// Bound the locate + SplitRange RPCs by SetTimeout (RFC-112): C++ wraps this in
	// waitOrError(tr.getRangeSplitPoints(...), resetPromise) (ReadYourWrites.actor.cpp:1879).
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	pts, err := tx.getRangeSplitPointsImpl(ctx, begin, end, chunkSize)
	return pts, tx.mapReadError(ctx, err)
}

func (tx *Transaction) getRangeSplitPointsImpl(ctx context.Context, begin, end []byte, chunkSize int64) ([][]byte, error) {
	// inverted_range (2005) is reported FIRST — libfdb_c constructs a KeyRangeRef from the C args
	// before entering RYW::getRangeSplitPoints, and the KeyRangeRef ctor throws inverted_range on
	// begin > end, ahead of the used_during_commit / maxKey checks. So an inverted range — even one
	// also past maxReadKey — is 2005, not 2004. Go's API takes raw begin/end with no
	// constructing range, so the check lives here, before everything else.
	if bytes.Compare(begin, end) > 0 {
		return nil, &wire.FDBError{Code: ErrInvertedRange} // 2005
	}
	// Range construction precedes the captured deferred/resetPromise entry gate. C++ checks
	// resetPromise.isSet() BEFORE the maxKey check (ReadYourWrites.actor.cpp:1872 before :1875),
	// so a timed-out txn returns transaction_timed_out (1031), not key_outside_legal_range.
	if err := tx.readEntryError(ctx); err != nil {
		return nil, err
	}
	// C++ RYW::getRangeSplitPoints rejects an out-of-range key (ReadYourWrites.actor.cpp:1875-1877):
	// `begin > getMaxReadKey() || end > getMaxReadKey() → key_outside_legal_range`. Sibling of the
	// getRange/Get path; without this Go silently accepted a split-points request past maxReadKey
	// where libfdb_c rejects (RFC-126, FDB-C-dev review).
	maxKey := tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, &wire.FDBError{Code: 2004} // key_outside_legal_range
	}
	// C++ uses std::numeric_limits<int>::max() (CLIENT_KNOBS->TOO_MANY) to fetch ALL
	// shard locations overlapping [begin,end) at once (NativeAPI.actor.cpp:8153-8159).
	const shardLimit = math.MaxInt32

	// No attempt cap: C++ loops until the transaction times out (NativeAPI.actor.cpp:8162, 8204-8208).
	for {
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, shardLimit, false, tx.tenantId, tx.currentSpan())
		if err != nil {
			return nil, fmt.Errorf("locate range for split points: %w", err)
		}
		if len(locations) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		// Port of C++ getRangeSplitPoints assembly (NativeAPI.actor.cpp:8164-8191):
		// results = [begin, (for each shard i>0) shard[i].begin, <shard i split points>..., end].
		// The result is ALWAYS framed by the range bounds, and each internal shard
		// boundary is itself a split point — so a range spanning N shards yields the
		// N-1 internal boundaries even when no shard has an internal chunk split.
		result := make([][]byte, 0, len(locations)+2)
		result = append(result, append([]byte(nil), begin...)) // push_back(keys.begin)
		retry, opFailed := false, false
		for i, loc := range locations {
			// partBegin/partEnd per C++ :8165-8166: first shard starts at keys.begin,
			// last ends at keys.end, interior shards use their own bounds.
			partBegin := loc.ShardBegin
			if i == 0 {
				partBegin = begin
			}
			partEnd := loc.ShardEnd
			if i == len(locations)-1 || partEnd == nil || bytes.Compare(partEnd, end) > 0 {
				partEnd = end
			}
			// Insert the internal shard boundary BEFORE this shard's points (C++ :8179-8182).
			if i > 0 {
				result = append(result, append([]byte(nil), loc.ShardBegin...))
			}
			points, perr := tx.sendSplitRange(ctx, partBegin, partEnd, chunkSize, loc.Servers)
			if perr != nil {
				if isWrongShardServer(perr) || isAllAlternativesFailed(perr) || isMaybeDelivered(perr) {
					tx.db.locCache.invalidateRange(begin, end, tx.tenantId)
					if serr := sleepCtx(ctx, wrongShardRetryDelay); serr != nil {
						return nil, serr
					}
					retry = true
					break
				}
				// operation_failed (4) = endpoint unsupported (old FDB): degrade to no
				// split points, matching the prior graceful fallback.
				if isOperationFailed(perr) {
					opFailed = true
					break
				}
				return nil, perr
			}
			result = append(result, points...)
		}
		if retry {
			continue
		}
		if opFailed {
			return nil, nil
		}
		// C++ :8189-8191: append keys.end unless the last point already is it.
		if !bytes.Equal(result[len(result)-1], end) {
			result = append(result, append([]byte(nil), end...))
		}
		return result, nil
	}
}

func (tx *Transaction) sendSplitRange(ctx context.Context, begin, end []byte, chunkSize int64, servers []ServerInfo) ([][]byte, error) {
	body, err := tx.sendMetricsRequest(ctx, servers, EndpointGetRangeSplitPoints, func(reply transport.UID) []byte {
		req := types.SplitRangeRequest{
			Keys:       types.KeyRangeRef{Begin: begin, End: end},
			ChunkSize:  chunkSize,
			Reply:      types.ReplyPromise{Token: wire.UIDFromParts(reply.First, reply.Second)},
			TenantInfo: types.TenantInfo{TenantId: tx.tenantId},
		}
		return req.MarshalFDB()
	})
	if err != nil {
		return nil, err
	}
	return parseSplitRangeReply(body)
}

func parseSplitRangeReply(data []byte) ([][]byte, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return nil, fmt.Errorf("SplitRange: %w", err)
	}
	// splitPoints is a C++ VectorRef<KeyRef> with the DEFAULT FlatBuffers
	// strategy: [count][RelativeOffset]* with each offset → [len][data] —
	// NOT a VecSerStrategy::String inline blob. The old parse read the slot
	// as a dynamic blob (landing on the count word) and decoded ZERO split
	// points from every real reply; the only e2e test tolerated an empty
	// result. Pinned by the SplitRangeReply_two_points reply ground-truth
	// vector (real C++ ObjectWriter bytes).
	count, err := r.ReadVectorCount(types.SplitRangeReplySlotSplitPoints)
	if err != nil {
		return nil, fmt.Errorf("SplitRange: %w", err)
	}
	points := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		p, err := r.ReadVectorBytes(types.SplitRangeReplySlotSplitPoints, i)
		if err != nil {
			return nil, fmt.Errorf("SplitRange: %w", err)
		}
		points = append(points, p)
	}
	return points, nil
}

func isOperationFailed(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && code == ErrOperationFailed
}

// parseWaitMetricsReply parses the ErrorOr-wrapped StorageMetrics reply.
func parseWaitMetricsReply(data []byte) (int64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return 0, fmt.Errorf("WaitMetrics: %w", err)
	}
	// Hygiene, not a bug fix: the old UnmarshalFDB(data) on the full ErrorOr
	// payload happened to parse correctly — the ErrorOr union stores its
	// value RelativeOffset at object offset 4, exactly where FakeRoot keeps
	// its field 0, so NewReader's root walk lands on the inner table by
	// layout coincidence (proven by mutation: reverting this stays green).
	// Parse from the envelope-positioned reader anyway so every reply parser
	// uses the one canonical ReadErrorOrInto walk instead of leaning on that
	// coincidence. Covered by the StorageMetrics_basic vector.
	var metrics types.StorageMetrics
	if err := metrics.UnmarshalFromReader(&r); err != nil {
		return 0, fmt.Errorf("WaitMetrics: %w", err)
	}
	return metrics.Bytes, nil
}
