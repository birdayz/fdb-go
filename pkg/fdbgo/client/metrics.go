package client

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
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
	return tx.getStorageMetricsRange(ctx, begin, end, minVersion, nil)
}

// Each size worker retries only its own range, as doGetStorageMetrics does.
// Singleton retries stay iterative; recursion is needed only when a shard splits.
func (tx *Transaction) getStorageMetricsRange(ctx context.Context, begin, end []byte, minVersion int64, locations []LocationResult) (int64, error) {
	for {
		if locations == nil {
			var err error
			// C++ uses std::numeric_limits<int>::max() to get all locations.
			locations, err = tx.db.locCache.locateRange(tx.db, ctx, begin, end, math.MaxInt32, false, tx.tenantId, tx.currentSpan())
			if err != nil {
				return 0, fmt.Errorf("locate range for metrics: %w", err)
			}
		}
		var total int64
		var err error
		if len(locations) == 1 {
			loc := locations[0]
			shardBegin, shardEnd := metricsShardBounds(begin, end, loc)
			if bytes.Compare(shardBegin, shardEnd) >= 0 {
				return 0, nil
			}
			total, err = tx.sendWaitMetrics(ctx, shardBegin, shardEnd, minVersion, loc.Servers)
		} else {
			var sizes []int64
			sizes, err = collectMetricsShards(ctx, locations, func(ctx context.Context, _ int, loc LocationResult) (int64, error) {
				shardBegin, shardEnd := metricsShardBounds(begin, end, loc)
				return tx.getStorageMetricsRange(ctx, shardBegin, shardEnd, minVersion, []LocationResult{loc})
			}, tx.db.afterMetricsShardResult.Load())
			for _, size := range sizes {
				total += size
			}
		}
		if err == nil {
			return total, nil
		}
		var delay time.Duration
		switch {
		case isWrongShardServer(err), isAllAlternativesFailed(err):
			tx.db.locCache.invalidateRange(begin, end, tx.tenantId)
			delay = wrongShardRetryDelay
		case isFutureVersion(err):
			delay = futureVersionDelay
		default:
			return 0, err
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return 0, err
		}
		locations = nil
	}
}

func metricsShardBounds(begin, end []byte, loc LocationResult) ([]byte, []byte) {
	shardBegin, shardEnd := loc.ShardBegin, loc.ShardEnd
	if bytes.Compare(shardBegin, begin) < 0 {
		shardBegin = begin
	}
	if shardEnd == nil || bytes.Compare(shardEnd, end) > 0 {
		shardEnd = end
	}
	return shardBegin, shardEnd
}

// Run each shard through its first enqueue or suspension before starting the next,
// as Flow does. Match waitForAll's error propagation and ordered results.
func collectMetricsShards[T any](parent context.Context, locations []LocationResult, send func(context.Context, int, LocationResult) (T, error), afterResult *func(int)) ([]T, error) {
	ctx, cancel := context.WithCancel(parent)
	type result struct {
		index int
		value T
		err   error
	}
	completed := make(chan result, len(locations))
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for i, loc := range locations {
		workerCtx, started := withParkSignal(ctx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer started.Fire()
			value, err := send(workerCtx, i, loc)
			completed <- result{index: i, value: value, err: err}
		}()
		select {
		case <-started.Done():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if len(completed) < len(locations) {
		notifyParked(ctx)
	}
	values := make([]T, len(locations))
	for range locations {
		select {
		case res := <-completed:
			if res.err != nil {
				return nil, res.err
			}
			values[res.index] = res.value
			if afterResult != nil {
				(*afterResult)(res.index)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return values, nil
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

// sendMetricsRequest sends a request as loadBalance does (LoadBalance.actor.h:589-725):
// one replica per attempt, no reply timeout, and a backoff once all were tried.
func (tx *Transaction) sendMetricsRequest(ctx context.Context, servers []ServerInfo, endpoint int, build func(reply transport.UID) []byte) ([]byte, error) {
	var closed <-chan struct{} // Close ends the loop, as destroying the database cancels the actor
	if tx.db.ctx != nil {
		closed = tx.db.ctx.Done()
	}
	var backoff time.Duration
	attempts, next := 0, rand.IntN(max(len(servers), 1))
	for {
		// Endpoints reported not found never recover, so each attempt drops them first.
		live, ok := tx.db.failMon.liveServers(servers, endpoint)
		if !ok || len(live) == 0 {
			return nil, tx.db.allAlternativesFailed(ctx)
		}
		// Excluded replicas are skipped; with none left, wait before sending (:595-641).
		var recovered <-chan struct{}
		if tx.db.failMon != nil {
			recovered = tx.db.failMon.waitForRecovery() // before the check, so no recovery is missed
		}
		avail := tx.db.notExcluded(live)
		if len(avail) == 0 {
			if err := tx.db.awaitAlternative(ctx, closed, live, recovered, tx.db.altFailureDelay()); err != nil {
				return nil, err
			}
			attempts = 0 // the backoff is kept (:641)
			continue
		}
		if backoff > 0 {
			timer := time.NewTimer(backoff)
			notifyParked(ctx)
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
		if body, ok := tx.metricsSend(ctx, avail[next%len(avail)], endpoint, build); ok {
			return body, nil
		}
		next++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if tx.db.ctx != nil && tx.db.ctx.Err() != nil {
			return nil, tx.db.ctx.Err()
		}
		if attempts++; attempts >= len(live) {
			backoff = time.Duration(math.Min(math.Max(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceStartBackoff)), float64(loadBalanceMaxBackoff)))
		}
	}
}

// metricsSend sends the request to server and waits for its reply.
func (tx *Transaction) metricsSend(ctx context.Context, server ServerInfo, endpoint int, build func(reply transport.UID) []byte) ([]byte, bool) {
	conn, permit, err := tx.db.getOrDialForSend(ctx, server.Address)
	if err != nil {
		tx.db.handleDialError(ctx, server.Address)
		return nil, false
	}
	sendCleanup := sendGuard{permit: permit}
	defer sendCleanup.close()
	token := getAdjustedEndpoint(server.Token, endpoint)
	replyToken, replyCh, replyHandle := conn.PrepareReplyTo(token)
	replyCleanup := replyGuard{handle: replyHandle}
	defer replyCleanup.close()
	if err := sendReadFrame(ctx, conn, token, build(replyToken), replyHandle, permit, nil); err != nil {
		replyHandle.Cancel()
		replyCleanup.release()
		tx.db.handleReadConnError(server.Address, conn, err)
		return nil, false
	}
	var closed <-chan struct{}
	if tx.db.ctx != nil {
		closed = tx.db.ctx.Done()
	}
	if hook := tx.db.beforeMetricsReplyWait.Load(); hook != nil {
		(*hook)()
	}
	var resp transport.Response
	select {
	case resp = <-replyCh:
	case <-conn.Done():
		if hook := tx.db.beforeMetricsDisconnectReply.Load(); hook != nil {
			(*hook)()
		}
		// A published reply wins over disconnect; otherwise do not wait for
		// pending-reply failure delivery behind a blocked socket Close.
		select {
		case resp = <-replyCh:
		default:
			replyHandle.Cancel()
			replyCleanup.release()
			tx.db.handleReadConnError(server.Address, conn, &transport.ConnClosedError{})
			return nil, false
		}
	case <-closed:
		replyHandle.Cancel()
		replyCleanup.release()
		return nil, false
	case <-ctx.Done():
		replyHandle.Cancel()
		replyCleanup.release()
		return nil, false
	}
	replyCleanup.release()
	if resp.Err != nil {
		tx.db.handleReadConnError(server.Address, conn, resp.Err)
		return nil, false
	}
	// An in-band broken_promise or request_maybe_delivered is not a response:
	// loadBalance moves on to the next replica (LoadBalance.actor.h:344-377).
	if err := wire.ReadErrorOrInto(resp.Body, new(wire.Reader)); isMaybeDelivered(err) {
		tx.db.noteBrokenPromise(err, server.Address, token)
		return nil, false
	}
	return resp.Body, true
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
		shardPoints, err := collectMetricsShards(ctx, locations, func(ctx context.Context, i int, loc LocationResult) ([][]byte, error) {
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
			return tx.sendSplitRange(ctx, partBegin, partEnd, chunkSize, loc.Servers)
		}, tx.db.afterMetricsShardResult.Load())
		if err != nil {
			if isWrongShardServer(err) || isAllAlternativesFailed(err) {
				tx.db.locCache.invalidateRange(begin, end, tx.tenantId)
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return nil, err
				}
				continue
			}
			// operation_failed (4) = endpoint unsupported (old FDB): keep the
			// existing graceful fallback to no split points.
			if isOperationFailed(err) {
				return nil, nil
			}
			return nil, err
		}
		result := make([][]byte, 0, len(locations)+2)
		result = append(result, append([]byte(nil), begin...))
		for i, points := range shardPoints {
			if i > 0 {
				result = append(result, append([]byte(nil), locations[i].ShardBegin...))
			}
			result = append(result, points...)
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
