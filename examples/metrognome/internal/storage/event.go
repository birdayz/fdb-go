package storage

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type EventStore struct {
	db *DB
}

// IngestResult contains per-event acceptance information.
type IngestResult struct {
	Accepted   int32
	Duplicates int32
	// AcceptedIndexes contains the indices of events that were accepted (not duplicates).
	AcceptedIndexes []int
}

// Ingest saves a batch of usage events in a single transaction.
// Deduplicates by idempotency_key using pipelined index lookups — all N dedup
// checks fire as FDB GetRange futures at once (1 round trip total instead of N
// sequential), then resolves and saves non-duplicates.
// Returns detailed result including which event indices were accepted.
func (s *EventStore) Ingest(ctx context.Context, events []*storev1.UsageEvent) (*IngestResult, error) {
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		result := &IngestResult{}
		tx := rs.Context().Transaction()

		// Phase 1: Fire all dedup lookups as futures (non-blocking).
		// Each lookup checks if the idempotency key exists in the unique index.
		// All N reads pipeline over the wire — ~1 FDB round trip total.
		idx := rs.GetRecordMetaData().GetIndex("event_by_idempotency_key")
		indexSS := rs.Subspace().Sub(rl.IndexKey, idx.SubspaceTupleKey())

		// Use pipelined prefix-range checks. A unique index has exactly one entry
		// per value, but the PK suffix is unknown so we can't do a point Get.
		// GetRange with limit=1 on the prefix is the fastest check.
		dedupFutures := make([]fdb.RangeResult, len(events))
		for i, evt := range events {
			prefix := fdb.Key(indexSS.Pack(tuple.Tuple{evt.GetIdempotencyKey()}))
			kr, _ := fdb.PrefixRange(prefix)
			dedupFutures[i] = tx.Snapshot().GetRange(kr, fdb.RangeOptions{Limit: 1})
		}

		// Phase 2: Resolve dedup futures — collect non-duplicate events.
		var toSave []proto.Message
		var toSaveIndexes []int
		for i := range events {
			existing, err := dedupFutures[i].GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(existing) > 0 {
				result.Duplicates++
				continue
			}
			toSave = append(toSave, events[i])
			toSaveIndexes = append(toSaveIndexes, i)
		}

		// Phase 3: Save non-duplicates using InsertBatch (no existence check —
		// dedup already proved these are new). Max throughput path: no reads,
		// disabled RYW, disabled write conflicts.
		if len(toSave) > 0 {
			if err := rs.InsertBatch(toSave); err != nil {
				return nil, err
			}
			result.Accepted = int32(len(toSave))
			result.AcceptedIndexes = toSaveIndexes
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	return r.(*IngestResult), nil
}

// BulkInsert writes events using InsertBatch — maximum throughput path.
// Skips read-before-write, disables RYW cache + write conflict ranges.
// Use for bulk loads where keys are guaranteed unique.
func (s *EventStore) BulkInsert(ctx context.Context, events []*storev1.UsageEvent) (int, error) {
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		records := make([]proto.Message, len(events))
		for i, evt := range events {
			records[i] = evt
		}
		if err := rs.InsertBatch(records); err != nil {
			return nil, err
		}
		return len(events), nil
	})
	if err != nil {
		return 0, err
	}
	return r.(int), nil
}

// GetUsage returns the total aggregated value for a customer/meter across a bucket range.
func (s *EventStore) GetUsage(ctx context.Context, customerID, meterSlug string, startBucket, endBucket int64) (int64, error) {
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		result, err := rs.EvaluateAggregateFunction(ctx,
			[]string{"UsageEvent"},
			rl.NewSumAggregateFunction(
				rl.GroupBy(rl.Field("value"), rl.Field("customer_id"), rl.Field("meter_slug"), rl.Field("timestamp_bucket"))),
			rl.TupleRangeBetweenInclusive(
				tuple.Tuple{customerID, meterSlug, startBucket},
				tuple.Tuple{customerID, meterSlug, endBucket}),
			rl.IsolationLevelSnapshot)
		if err != nil {
			return nil, err
		}
		if len(result) == 0 {
			return int64(0), nil
		}
		return result[0], nil
	})
	if err != nil {
		return 0, err
	}
	return r.(int64), nil
}

// GetUsageCount returns the event count for a customer/meter across a bucket range.
func (s *EventStore) GetUsageCount(ctx context.Context, customerID, meterSlug string, startBucket, endBucket int64) (int64, error) {
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		result, err := rs.EvaluateAggregateFunction(ctx,
			[]string{"UsageEvent"},
			rl.NewCountAggregateFunction(
				rl.GroupBy(rl.EmptyKey(), rl.Field("customer_id"), rl.Field("meter_slug"), rl.Field("timestamp_bucket"))),
			rl.TupleRangeBetweenInclusive(
				tuple.Tuple{customerID, meterSlug, startBucket},
				tuple.Tuple{customerID, meterSlug, endBucket}),
			rl.IsolationLevelSnapshot)
		if err != nil {
			return nil, err
		}
		if len(result) == 0 {
			return int64(0), nil
		}
		return result[0], nil
	})
	if err != nil {
		return 0, err
	}
	return r.(int64), nil
}

// GetUsageBuckets returns per-bucket usage values by scanning the SUM index.
func (s *EventStore) GetUsageBuckets(ctx context.Context, customerID, meterSlug string, startBucket, endBucket int64) (map[int64]int64, error) {
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		idx := rs.GetRecordMetaData().GetIndex("usage_sum")
		cursor := rs.ScanIndex(idx,
			rl.TupleRangeBetweenInclusive(
				tuple.Tuple{customerID, meterSlug, startBucket},
				tuple.Tuple{customerID, meterSlug, endBucket}),
			nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}
		buckets := make(map[int64]int64, len(entries))
		for _, e := range entries {
			bucket := e.Key[2].(int64)
			val := e.Value[0].(int64)
			buckets[bucket] = val
		}
		return buckets, nil
	})
	if err != nil {
		return nil, err
	}
	return r.(map[int64]int64), nil
}
