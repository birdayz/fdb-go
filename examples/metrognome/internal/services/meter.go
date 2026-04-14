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
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/meter"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

type MeterService struct {
	metrognomev1connect.UnimplementedMeterServiceHandler
	store       *storage.MeterStore
	meterEngine *meter.Engine
}

func NewMeterService(store *storage.MeterStore, meterEngine *meter.Engine) *MeterService {
	return &MeterService{store: store, meterEngine: meterEngine}
}

func (s *MeterService) CreateMeter(ctx context.Context, req *connect.Request[metrognomev1.CreateMeterRequest]) (*connect.Response[metrognomev1.CreateMeterResponse], error) {
	if err := validateCreateMeter(req.Msg); err != nil {
		return nil, err
	}
	id := newID("mtr")
	now := time.Now().UnixMilli()

	record := &storev1.Meter{
		Id:                proto.String(id),
		Slug:              proto.String(req.Msg.GetSlug()),
		Name:              proto.String(req.Msg.GetName()),
		AggregationType:   convertAggType(req.Msg.GetAggregationType()).Enum(),
		ValueProperty:     proto.String(req.Msg.GetValueProperty()),
		GroupByProperties: req.Msg.GetGroupByProperties(),
		EventTypeFilter:   proto.String(req.Msg.GetEventTypeFilter()),
		CreatedAt:         proto.Int64(now),
	}

	if err := s.store.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create meter: %w", err))
	}

	// Register in dynamic meter engine for per-meter aggregation
	if s.meterEngine != nil {
		if err := s.meterEngine.Register(record); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("register meter: %w", err))
		}
	}

	return connect.NewResponse(&metrognomev1.CreateMeterResponse{
		Meter: meterToAPI(record),
	}), nil
}

func (s *MeterService) GetMeter(ctx context.Context, req *connect.Request[metrognomev1.GetMeterRequest]) (*connect.Response[metrognomev1.GetMeterResponse], error) {
	record, err := s.store.GetBySlug(ctx, req.Msg.GetSlug())
	if err != nil {
		return nil, storageError("meter", err)
	}
	return connect.NewResponse(&metrognomev1.GetMeterResponse{
		Meter: meterToAPI(record),
	}), nil
}

func (s *MeterService) ListMeters(ctx context.Context, req *connect.Request[metrognomev1.ListMetersRequest]) (*connect.Response[metrognomev1.ListMetersResponse], error) {
	items, cont, err := s.store.List(ctx, int(req.Msg.GetPageSize()), req.Msg.GetContinuation())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	meters := make([]*metrognomev1.Meter, len(items))
	for i, item := range items {
		meters[i] = meterToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListMetersResponse{
		Meters:       meters,
		Continuation: cont,
	}), nil
}

func meterToAPI(s *storev1.Meter) *metrognomev1.Meter {
	return &metrognomev1.Meter{
		Id:                s.GetId(),
		Slug:              s.GetSlug(),
		Name:              s.GetName(),
		AggregationType:   metrognomev1.AggregationType(s.GetAggregationType()),
		ValueProperty:     s.GetValueProperty(),
		GroupByProperties: s.GetGroupByProperties(),
		EventTypeFilter:   s.GetEventTypeFilter(),
		CreatedAt:         s.GetCreatedAt(),
	}
}

func convertAggType(t metrognomev1.AggregationType) storev1.AggregationType {
	switch t {
	case metrognomev1.AggregationType_AGGREGATION_TYPE_COUNT:
		return storev1.AggregationType_AGGREGATION_TYPE_COUNT
	case metrognomev1.AggregationType_AGGREGATION_TYPE_SUM:
		return storev1.AggregationType_AGGREGATION_TYPE_SUM
	case metrognomev1.AggregationType_AGGREGATION_TYPE_MAX:
		return storev1.AggregationType_AGGREGATION_TYPE_MAX
	case metrognomev1.AggregationType_AGGREGATION_TYPE_UNIQUE:
		return storev1.AggregationType_AGGREGATION_TYPE_UNIQUE
	case metrognomev1.AggregationType_AGGREGATION_TYPE_LATEST:
		return storev1.AggregationType_AGGREGATION_TYPE_LATEST
	default:
		return storev1.AggregationType_AGGREGATION_TYPE_COUNT
	}
}
