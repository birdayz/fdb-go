package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// GetEstimatedRangeSizeBytes returns an estimate of the byte size of the given
// key range. Matches C++ getStorageMetricsLargeKeyRange in NativeAPI.actor.cpp:
// gets all shard locations, sends WaitMetricsRequest to each with min.bytes=0,
// max.bytes=-1 (reversed range = immediate response), and sums the bytes.
func (tx *Transaction) GetEstimatedRangeSizeBytes(parentCtx context.Context, begin, end []byte) (int64, error) {
	// Bound the locate + WaitMetrics RPCs by SetTimeout (RFC-112): C++ wraps this in
	// waitOrError(getStorageMetrics(...), resetPromise) (ReadYourWrites.actor.cpp:1863),
	// so a hung shard-metrics RPC must surface transaction_timed_out at the deadline.
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	n, err := tx.getEstimatedRangeSizeBytesImpl(ctx, begin, end)
	return n, tx.mapTimeout(parentCtx, err)
}

func (tx *Transaction) getEstimatedRangeSizeBytesImpl(ctx context.Context, begin, end []byte) (int64, error) {
	// inverted_range (2005) first — same KeyRangeRef-construction semantics as getRangeSplitPoints:
	// libfdb_c's C-API range construction throws inverted_range on begin > end before the metric op
	// runs, so Go (raw begin/end) checks it here. (Unlike getRangeSplitPoints there is NO maxKey check
	// — C++ getEstimatedRangeSizeBytes/getStorageMetrics, ReadYourWrites.actor.cpp:1853, has none.)
	if bytes.Compare(begin, end) > 0 {
		return 0, &wire.FDBError{Code: ErrInvertedRange} // 2005
	}
	// A cancelled txn returns transaction_cancelled (1025) — C++ races resetPromise at op entry,
	// before any other check (RFC-068). This path bypasses ensureReadVersion, so gate explicitly.
	if err := tx.checkCancelled(); err != nil {
		return 0, err
	}
	// A transaction poisoned by SetReadYourWritesDisable-after-an-op returns
	// client_invalid_operation here too (verified differentially: libfdb_c poisons the metrics
	// path). This entry point does not fetch a read version, so it is gated explicitly rather
	// than via ensureReadVersion (RFC-059).
	if tx.rywPoisonErr != nil {
		return 0, tx.rywPoisonErr
	}
	// C++ uses std::numeric_limits<int>::max() — get ALL locations at once.
	const shardLimit = math.MaxInt32

	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, shardLimit, false, tx.tenantId, tx.spanContext)
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

			b, err := tx.sendWaitMetrics(ctx, shardBegin, shardEnd, loc.Servers)
			if err != nil {
				if isWrongShardServer(err) || isAllAlternativesFailed(err) {
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
	return 0, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

// isFutureVersion returns true if the error is FDB error 1009.
func isFutureVersion(err error) bool {
	var fdbErr *wire.FDBError
	return errors.As(err, &fdbErr) && fdbErr.Code == ErrFutureVersion
}

// sendWaitMetrics sends a WaitMetricsRequest to a storage server and returns
// the bytes field from the StorageMetrics reply. Uses min.bytes=0, max.bytes=-1
// (reversed range) to get an immediate response instead of waiting for a
// threshold change.
func (tx *Transaction) sendWaitMetrics(ctx context.Context, begin, end []byte, servers []ServerInfo) (int64, error) {
	for _, server := range servers {
		conn, err := tx.db.getOrDial(ctx, server.Address)
		if err != nil {
			tx.db.handleDialError(ctx, server.Address)
			continue
		}
		replyToken, replyCh, replyHandle := conn.PrepareReply()
		req := types.WaitMetricsRequest{
			Keys:       types.KeyRangeRef{Begin: begin, End: end},
			Min:        types.StorageMetrics{Bytes: 0},
			Max:        types.StorageMetrics{Bytes: -1},
			Reply:      types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
			TenantInfo: types.TenantInfo{TenantId: tx.tenantId},
			MinVersion: tx.readVersion,
		}
		wmToken := getAdjustedEndpoint(server.Token, EndpointWaitMetrics)
		if err := conn.SendFrame(wmToken, req.MarshalFDB()); err != nil {
			replyHandle.Cancel()
			replyHandle.Release()
			tx.db.handleConnError(server.Address)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			replyHandle.Release()
			if resp.Err != nil {
				tx.db.handleConnError(server.Address)
				continue
			}
			return parseWaitMetricsReply(resp.Body)
		case <-rctx.Done():
			cancel()
			replyHandle.Cancel()
			replyHandle.Release()
			continue
		}
	}
	return 0, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

// GetRangeSplitPoints returns suggested split points for the given key range.
// Matches C++ Transaction::getRangeSplitPoints in NativeAPI.actor.cpp.
func (tx *Transaction) GetRangeSplitPoints(parentCtx context.Context, begin, end []byte, chunkSize int64) ([][]byte, error) {
	// Bound the locate + SplitRange RPCs by SetTimeout (RFC-112): C++ wraps this in
	// waitOrError(tr.getRangeSplitPoints(...), resetPromise) (ReadYourWrites.actor.cpp:1879).
	ctx, cancel := tx.opContext(parentCtx)
	defer cancel()
	pts, err := tx.getRangeSplitPointsImpl(ctx, begin, end, chunkSize)
	return pts, tx.mapTimeout(parentCtx, err)
}

func (tx *Transaction) getRangeSplitPointsImpl(ctx context.Context, begin, end []byte, chunkSize int64) ([][]byte, error) {
	// inverted_range (2005) is reported FIRST — libfdb_c constructs a KeyRangeRef from the C args
	// before entering RYW::getRangeSplitPoints, and the KeyRangeRef ctor throws inverted_range on
	// begin > end, ahead of the used_during_commit / maxKey checks. So an inverted range — even one
	// also past maxReadKey — is 2005, not 2004 (codex catch). Go's API takes raw begin/end with no
	// constructing range, so the check lives here, before everything else.
	if bytes.Compare(begin, end) > 0 {
		return nil, &wire.FDBError{Code: ErrInvertedRange} // 2005
	}
	// A cancelled txn returns transaction_cancelled (1025) — resetPromise at op entry (RFC-068).
	if err := tx.checkCancelled(); err != nil {
		return nil, err
	}
	// Sibling of GetEstimatedRangeSizeBytes: bypasses ensureReadVersion but is poisoned by a
	// SetReadYourWritesDisable-after-an-op (libfdb_c gates it via the same deferredError /
	// checkValid path) — RFC-059.
	if tx.rywPoisonErr != nil {
		return nil, tx.rywPoisonErr
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

	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, shardLimit, false, tx.tenantId, tx.spanContext)
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
				if isWrongShardServer(perr) || isAllAlternativesFailed(perr) {
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
	return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

func (tx *Transaction) sendSplitRange(ctx context.Context, begin, end []byte, chunkSize int64, servers []ServerInfo) ([][]byte, error) {
	for _, server := range servers {
		conn, err := tx.db.getOrDial(ctx, server.Address)
		if err != nil {
			tx.db.handleDialError(ctx, server.Address)
			continue
		}
		replyToken, replyCh, replyHandle := conn.PrepareReply()
		req := types.SplitRangeRequest{
			Keys:       types.KeyRangeRef{Begin: begin, End: end},
			ChunkSize:  chunkSize,
			Reply:      types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
			TenantInfo: types.TenantInfo{TenantId: tx.tenantId},
		}
		srToken := getAdjustedEndpoint(server.Token, EndpointGetRangeSplitPoints)
		if err := conn.SendFrame(srToken, req.MarshalFDB()); err != nil {
			replyHandle.Cancel()
			replyHandle.Release()
			tx.db.handleConnError(server.Address)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
		select {
		case resp := <-replyCh:
			cancel()
			replyHandle.Release()
			if resp.Err != nil {
				tx.db.handleConnError(server.Address)
				continue
			}
			return parseSplitRangeReply(resp.Body)
		case <-rctx.Done():
			cancel()
			replyHandle.Cancel()
			replyHandle.Release()
			continue
		}
	}
	return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
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
	var fdbErr *wire.FDBError
	return errors.As(err, &fdbErr) && fdbErr.Code == ErrOperationFailed
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
	metrics.UnmarshalFromReader(&r)
	return metrics.Bytes, nil
}
