package storage

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

const unsplitRecord = int64(0)

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
// Deduplicates by primary key — PK is (type, customer_id, timestamp_ms, idempotency_key)
// so duplicate idempotency keys for the same customer/time map to the same record.
// Uses pipelined point Gets (faster than the old index prefix scan).
func (s *EventStore) Ingest(ctx context.Context, events []*storev1.UsageEvent) (*IngestResult, error) {
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		result := &IngestResult{}
		tx := rs.Context().Transaction()

		// Record subspace: [store_ss][RecordKey][pk...][unsplit=0]
		recordSS := rs.Subspace().Sub(rl.RecordKey)
		typeKey := int64(rs.GetRecordMetaData().GetRecordType("UsageEvent").RecordTypeIndex)

		// Phase 1: Fire N pipelined point Gets to check PK existence.
		// Point Get is faster than the old GetRange(limit=1) on index prefix.
		dedupFutures := make([]fdb.FutureByteSlice, len(events))
		for i, evt := range events {
			key := fdb.Key(recordSS.Pack(tuple.Tuple{
				typeKey,
				evt.GetCustomerId(),
				evt.GetTimestampMs(),
				evt.GetIdempotencyKey(),
				unsplitRecord,
			}))
			dedupFutures[i] = tx.Snapshot().Get(key)
		}

		// Phase 2: Resolve — collect non-duplicates.
		var toSave []proto.Message
		var toSaveIndexes []int
		for i := range events {
			existing, err := dedupFutures[i].Get()
			if err != nil {
				return nil, err
			}
			if existing != nil {
				result.Duplicates++
				continue
			}
			toSave = append(toSave, events[i])
			toSaveIndexes = append(toSaveIndexes, i)
		}

		// Phase 3: InsertBatch non-duplicates.
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

// ListEventsPage is the result of a paginated event scan.
type ListEventsPage struct {
	Events            []*storev1.UsageEvent
	ContinuationToken []byte // nil when no more pages
}

// ListEvents returns paginated events using cursor-based pagination.
// Exploits the PK structure (type, customer_id, timestamp_ms, idempotency_key)
// for efficient prefix scans:
//   - No customer filter → scan all UsageEvent records (type prefix)
//   - Customer filter → PK prefix (type, customer_id)
//   - Customer + time range → PK range within customer prefix
//
// Default: reverse (newest first). One FDB range read per page.
func (s *EventStore) ListEvents(ctx context.Context, customerID string, startMs, endMs int64, pageSize int, continuation []byte, reverse bool) (*ListEventsPage, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}

	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		typeKey := int64(rs.GetRecordMetaData().GetRecordType("UsageEvent").RecordTypeIndex)

		scanProps := rl.NewScanProperties(
			rl.DefaultExecuteProperties().WithReturnedRowLimit(pageSize),
		).WithReverse(reverse)

		var cursor rl.RecordCursor[*rl.FDBStoredRecord[proto.Message]]

		// Endpoint types: match ScanRecordsByType pattern (RangeInclusive).
		// For continuation, replace the appropriate endpoint based on direction.
		lowEP := rl.EndpointTypeRangeInclusive
		highEP := rl.EndpointTypeRangeInclusive
		if len(continuation) > 0 {
			if reverse {
				highEP = rl.EndpointTypeContinuation
			} else {
				lowEP = rl.EndpointTypeContinuation
			}
		}

		if customerID != "" && startMs > 0 && endMs > 0 {
			// Customer + time range: range scan within customer's events.
			// Override endpoints for explicit range bounds.
			low := tuple.Tuple{typeKey, customerID, startMs}
			high := tuple.Tuple{typeKey, customerID, endMs}
			if len(continuation) == 0 {
				lowEP = rl.EndpointTypeRangeInclusive
				highEP = rl.EndpointTypeRangeExclusive
			}
			cursor = rs.ScanRecordsInRange(low, high,
				lowEP, highEP,
				continuation, scanProps)
		} else if customerID != "" {
			// Customer only: PK prefix scan (type, customer_id).
			// RangeInclusive on a prefix tuple gives all keys starting with it.
			prefix := tuple.Tuple{typeKey, customerID}
			cursor = rs.ScanRecordsInRange(prefix, prefix,
				lowEP, highEP,
				continuation, scanProps)
		} else {
			// All events: scan by record type (handles endpoints internally).
			cursor = rs.ScanRecordsByType("UsageEvent", continuation, scanProps)
		}

		page := &ListEventsPage{}
		for {
			result, err := cursor.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !result.HasNext() {
				// Grab continuation if there are more pages
				cont := result.GetContinuation()
				if cont != nil && !cont.IsEnd() {
					page.ContinuationToken, _ = cont.ToBytes()
				}
				break
			}
			rec := result.GetValue()
			evt, ok := rec.Record.(*storev1.UsageEvent)
			if !ok {
				continue
			}
			page.Events = append(page.Events, evt)
		}
		return page, nil
	})
	if err != nil {
		return nil, err
	}
	return r.(*ListEventsPage), nil
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
