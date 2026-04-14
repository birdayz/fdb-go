package services

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/meter"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

type EventService struct {
	metrognomev1connect.UnimplementedEventServiceHandler
	events      *storage.EventStore
	alerts      *storage.AlertStore
	meterEngine *meter.Engine
}

func NewEventService(events *storage.EventStore, alerts *storage.AlertStore, meterEngine *meter.Engine) *EventService {
	return &EventService{events: events, alerts: alerts, meterEngine: meterEngine}
}

func (s *EventService) IngestEvents(ctx context.Context, req *connect.Request[metrognomev1.IngestEventsRequest]) (*connect.Response[metrognomev1.IngestEventsResponse], error) {
	if err := validateIngestEvents(req.Msg); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()

	records := make([]*storev1.UsageEvent, len(req.Msg.GetEvents()))
	for i, evt := range req.Msg.GetEvents() {
		ts := evt.GetTimestampMs()
		if ts == 0 {
			ts = now
		}
		meterSlug := evt.GetEventType()
		records[i] = &storev1.UsageEvent{
			Id:              proto.String(newID("evt")),
			CustomerId:      proto.String(evt.GetCustomerId()),
			EventType:       proto.String(evt.GetEventType()),
			MeterSlug:       proto.String(meterSlug),
			TimestampMs:     proto.Int64(ts),
			Value:           proto.Int64(evt.GetValue()),
			IdempotencyKey:  proto.String(evt.GetIdempotencyKey()),
			PropertiesJson:  proto.String(evt.GetPropertiesJson()),
			IngestedAt:      proto.Int64(now),
			TimestampBucket: proto.Int64(billing.BucketHour(ts)),
		}
	}

	// Write to static store (dedup, VALUE indexes, static SUM/COUNT)
	result, err := s.events.Ingest(ctx, records)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ingest events: %w", err))
	}

	// Route only accepted events to dynamic meter stores
	if s.meterEngine != nil {
		for _, idx := range result.AcceptedIndexes {
			evt := req.Msg.GetEvents()[idx]
			ts := evt.GetTimestampMs()
			if ts == 0 {
				ts = now
			}
			groupValues := parseProperties(evt.GetPropertiesJson())
			_ = s.meterEngine.IngestEvent(ctx, evt.GetEventType(), evt.GetCustomerId(),
				billing.BucketHour(ts), evt.GetValue(), groupValues)
		}
	}

	// Evaluate alerts for affected customers/meters
	if s.alerts != nil && result.Accepted > 0 {
		s.evaluateAlerts(ctx, req.Msg.GetEvents(), result.AcceptedIndexes)
	}

	return connect.NewResponse(&metrognomev1.IngestEventsResponse{
		Accepted:   result.Accepted,
		Duplicates: result.Duplicates,
	}), nil
}

// evaluateAlerts checks if any alerts have been breached after event ingestion.
func (s *EventService) evaluateAlerts(ctx context.Context, events []*metrognomev1.Event, acceptedIndexes []int) {
	// Collect unique customer/meter pairs from accepted events
	type key struct{ customerID, meterSlug string }
	seen := make(map[key]bool)
	for _, idx := range acceptedIndexes {
		evt := events[idx]
		k := key{evt.GetCustomerId(), evt.GetEventType()}
		seen[k] = true
	}

	for k := range seen {
		alerts, err := s.alerts.ListByCustomer(ctx, k.customerID)
		if err != nil {
			continue
		}
		for _, alert := range alerts {
			if alert.GetTriggered() {
				continue // already triggered
			}
			if alert.GetMeterSlug() != k.meterSlug {
				continue // different meter
			}
			// Check usage against threshold
			if alert.GetAlertType() == storev1.AlertType_ALERT_TYPE_USAGE {
				usage, err := s.getUsageForAlert(ctx, k.customerID, k.meterSlug)
				if err != nil {
					continue
				}
				if usage >= alert.GetThreshold() {
					alert.Triggered = proto.Bool(true)
					_ = s.alerts.Save(ctx, alert)
				}
			}
		}
	}
}

func (s *EventService) getUsageForAlert(ctx context.Context, customerID, meterSlug string) (int64, error) {
	// Use max int64 as end bucket to cover all time — alerts check lifetime usage
	const maxBucket = int64(1<<62 - 1)
	// Try dynamic engine first
	if s.meterEngine != nil {
		total, err := s.meterEngine.GetUsage(ctx, meterSlug, customerID, 0, maxBucket, nil)
		if err == nil {
			return total, nil
		}
	}
	// Fallback to static
	return s.events.GetUsage(ctx, customerID, meterSlug, 0, maxBucket)
}

func (s *EventService) GetUsage(ctx context.Context, req *connect.Request[metrognomev1.GetUsageRequest]) (*connect.Response[metrognomev1.GetUsageResponse], error) {
	startBucket := billing.BucketHour(req.Msg.GetStartMs())
	endBucket := billing.BucketHour(req.Msg.GetEndMs())

	// Try dynamic meter engine first (has user-defined group-by)
	if s.meterEngine != nil {
		total, err := s.meterEngine.GetUsage(ctx, req.Msg.GetMeterSlug(),
			req.Msg.GetCustomerId(), startBucket, endBucket, nil)
		if err == nil {
			resp := &metrognomev1.GetUsageResponse{TotalValue: total}

			// Add windowed breakdown if requested
			if req.Msg.GetWindowSize() != metrognomev1.WindowSize_WINDOW_SIZE_UNSPECIFIED {
				buckets, err := s.meterEngine.GetUsageBuckets(ctx, req.Msg.GetMeterSlug(),
					req.Msg.GetCustomerId(), startBucket, endBucket, nil)
				if err == nil {
					windowMs := int64(3600 * 1000)
					if req.Msg.GetWindowSize() == metrognomev1.WindowSize_WINDOW_SIZE_DAY {
						windowMs = 24 * 3600 * 1000
					}
					windowAgg := make(map[int64]int64)
					for bucket, val := range buckets {
						window := (bucket / windowMs) * windowMs
						windowAgg[window] += val
					}
					for window, val := range windowAgg {
						resp.Buckets = append(resp.Buckets, &metrognomev1.UsageBucket{
							StartMs: window, EndMs: window + windowMs, Value: val,
						})
					}
				}
			}

			return connect.NewResponse(resp), nil
		}
		// Fall through to static store if meter not registered in dynamic engine
	}

	// Fallback: static store
	total, err := s.events.GetUsage(ctx, req.Msg.GetCustomerId(), req.Msg.GetMeterSlug(), startBucket, endBucket)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get usage: %w", err))
	}

	resp := &metrognomev1.GetUsageResponse{TotalValue: total}

	if req.Msg.GetWindowSize() != metrognomev1.WindowSize_WINDOW_SIZE_UNSPECIFIED {
		buckets, err := s.events.GetUsageBuckets(ctx, req.Msg.GetCustomerId(), req.Msg.GetMeterSlug(), startBucket, endBucket)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get usage buckets: %w", err))
		}

		windowMs := int64(3600 * 1000)
		if req.Msg.GetWindowSize() == metrognomev1.WindowSize_WINDOW_SIZE_DAY {
			windowMs = 24 * 3600 * 1000
		}

		windowAgg := make(map[int64]int64)
		for bucket, val := range buckets {
			window := (bucket / windowMs) * windowMs
			windowAgg[window] += val
		}

		for window, val := range windowAgg {
			resp.Buckets = append(resp.Buckets, &metrognomev1.UsageBucket{
				StartMs: window, EndMs: window + windowMs, Value: val,
			})
		}
	}

	return connect.NewResponse(resp), nil
}

func parseProperties(jsonStr string) map[string]string {
	if jsonStr == "" || jsonStr == "{}" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil
	}
	result := make(map[string]string, len(raw))
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			result[k] = val
		case float64:
			if val == float64(int64(val)) {
				result[k] = fmt.Sprintf("%d", int64(val))
			} else {
				result[k] = fmt.Sprintf("%g", val)
			}
		case bool:
			result[k] = fmt.Sprintf("%t", val)
		default:
			b, err := json.Marshal(v)
			if err == nil {
				result[k] = string(b)
			}
		}
	}
	return result
}
