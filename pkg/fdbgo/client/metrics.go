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
func (tx *Transaction) GetEstimatedRangeSizeBytes(ctx context.Context, begin, end []byte) (int64, error) {
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
		locations, err := tx.db.locCache.locateRange(tx.db, ctx, begin, end, shardLimit, false, tx.tenantId)
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
func (tx *Transaction) GetRangeSplitPoints(ctx context.Context, begin, end []byte, chunkSize int64) ([][]byte, error) {
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
	for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
		loc, err := tx.db.locCache.locate(tx.db, ctx, begin, tx.tenantId)
		if err != nil {
			return nil, fmt.Errorf("locate for split points: %w", err)
		}
		if len(loc.Servers) == 0 {
			return nil, fmt.Errorf("no storage servers for key")
		}

		points, err := tx.sendSplitRange(ctx, begin, end, chunkSize, loc.Servers)
		if err == nil {
			return points, nil
		}
		if isWrongShardServer(err) || isAllAlternativesFailed(err) {
			tx.db.locCache.invalidate(begin, tx.tenantId)
			if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
				return nil, err
			}
			continue
		}
		// operation_failed (4) = endpoint not supported (e.g., old FDB version).
		// Return empty split points — the data fits in one shard.
		if isOperationFailed(err) {
			return nil, nil
		}
		return nil, err
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
