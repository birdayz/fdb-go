package storage

import (
	"context"
	"errors"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type EventStore struct {
	db *DB
}

// Ingest saves a batch of usage events in a single transaction.
// Deduplicates by idempotency_key using the unique index.
// Returns counts of accepted and duplicate events.
func (s *EventStore) Ingest(ctx context.Context, events []*storev1.UsageEvent) (int32, int32, error) {
	type counts struct {
		accepted   int32
		duplicates int32
	}
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		var c counts
		for _, evt := range events {
			_, err := rs.SaveRecord(evt)
			if err != nil {
				// Unique index violation means duplicate idempotency key
				var uv *rl.RecordIndexUniquenessViolationError
				if errors.As(err, &uv) {
					c.duplicates++
					continue
				}
				return nil, err
			}
			c.accepted++
		}
		return &c, nil
	})
	if err != nil {
		return 0, 0, err
	}
	res := r.(*counts)
	return res.accepted, res.duplicates, nil
}

// GetUsage returns the total aggregated value for a customer/meter across a bucket range.
// Uses the SUM atomic index for O(1) per-bucket reads.
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
			// Index key: [customer_id, meter_slug, timestamp_bucket]
			// Index value: little-endian int64 (sum)
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
