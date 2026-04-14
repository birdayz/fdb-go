package services

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

type EventService struct {
	metrognomev1connect.UnimplementedEventServiceHandler
	events *storage.EventStore
}

func NewEventService(events *storage.EventStore) *EventService {
	return &EventService{events: events}
}

func (s *EventService) IngestEvents(ctx context.Context, req *connect.Request[metrognomev1.IngestEventsRequest]) (*connect.Response[metrognomev1.IngestEventsResponse], error) {
	now := time.Now().UnixMilli()

	records := make([]*storev1.UsageEvent, len(req.Msg.GetEvents()))
	for i, evt := range req.Msg.GetEvents() {
		ts := evt.GetTimestampMs()
		if ts == 0 {
			ts = now
		}
		records[i] = &storev1.UsageEvent{
			Id:              proto.String(newID("evt")),
			CustomerId:      proto.String(evt.GetCustomerId()),
			EventType:       proto.String(evt.GetEventType()),
			MeterSlug:       proto.String(""), // resolved later based on event_type matching
			TimestampMs:     proto.Int64(ts),
			Value:           proto.Int64(evt.GetValue()),
			IdempotencyKey:  proto.String(evt.GetIdempotencyKey()),
			PropertiesJson:  proto.String(evt.GetPropertiesJson()),
			IngestedAt:      proto.Int64(now),
			TimestampBucket: proto.Int64(billing.BucketHour(ts)),
		}
	}

	accepted, duplicates, err := s.events.Ingest(ctx, records)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ingest events: %w", err))
	}

	return connect.NewResponse(&metrognomev1.IngestEventsResponse{
		Accepted:   accepted,
		Duplicates: duplicates,
	}), nil
}

func (s *EventService) GetUsage(ctx context.Context, req *connect.Request[metrognomev1.GetUsageRequest]) (*connect.Response[metrognomev1.GetUsageResponse], error) {
	startBucket := billing.BucketHour(req.Msg.GetStartMs())
	endBucket := billing.BucketHour(req.Msg.GetEndMs())

	total, err := s.events.GetUsage(ctx, req.Msg.GetCustomerId(), req.Msg.GetMeterSlug(), startBucket, endBucket)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get usage: %w", err))
	}

	resp := &metrognomev1.GetUsageResponse{
		TotalValue: total,
	}

	// If windowed, return per-bucket breakdown
	if req.Msg.GetWindowSize() != metrognomev1.WindowSize_WINDOW_SIZE_UNSPECIFIED {
		buckets, err := s.events.GetUsageBuckets(ctx, req.Msg.GetCustomerId(), req.Msg.GetMeterSlug(), startBucket, endBucket)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get usage buckets: %w", err))
		}

		windowMs := int64(3600 * 1000) // hour
		if req.Msg.GetWindowSize() == metrognomev1.WindowSize_WINDOW_SIZE_DAY {
			windowMs = 24 * 3600 * 1000
		}

		// Aggregate hourly buckets into requested window size
		windowAgg := make(map[int64]int64)
		for bucket, val := range buckets {
			window := (bucket / windowMs) * windowMs
			windowAgg[window] += val
		}

		for window, val := range windowAgg {
			resp.Buckets = append(resp.Buckets, &metrognomev1.UsageBucket{
				StartMs: window,
				EndMs:   window + windowMs,
				Value:   val,
			})
		}
	}

	return connect.NewResponse(resp), nil
}
