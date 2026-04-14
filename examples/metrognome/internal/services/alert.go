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
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

type AlertService struct {
	metrognomev1connect.UnimplementedAlertServiceHandler
	store *storage.AlertStore
}

func NewAlertService(store *storage.AlertStore) *AlertService {
	return &AlertService{store: store}
}

func (s *AlertService) CreateAlert(ctx context.Context, req *connect.Request[metrognomev1.CreateAlertRequest]) (*connect.Response[metrognomev1.CreateAlertResponse], error) {
	if err := validateCreateAlert(req.Msg); err != nil {
		return nil, err
	}
	id := newID("alrt")
	now := time.Now().UnixMilli()
	record := &storev1.Alert{
		Id:         proto.String(id),
		CustomerId: proto.String(req.Msg.GetCustomerId()),
		MeterSlug:  proto.String(req.Msg.GetMeterSlug()),
		Threshold:  proto.Int64(req.Msg.GetThreshold()),
		AlertType:  convertAlertType(req.Msg.GetAlertType()).Enum(),
		Triggered:  proto.Bool(false),
		CreatedAt:  proto.Int64(now),
		WebhookUrl: proto.String(req.Msg.GetWebhookUrl()),
	}
	if err := s.store.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create alert: %w", err))
	}
	return connect.NewResponse(&metrognomev1.CreateAlertResponse{
		Alert: alertToAPI(record),
	}), nil
}

func (s *AlertService) ListAlerts(ctx context.Context, req *connect.Request[metrognomev1.ListAlertsRequest]) (*connect.Response[metrognomev1.ListAlertsResponse], error) {
	items, err := s.store.ListByCustomer(ctx, req.Msg.GetCustomerId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	alerts := make([]*metrognomev1.Alert, len(items))
	for i, item := range items {
		alerts[i] = alertToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListAlertsResponse{
		Alerts: alerts,
	}), nil
}

func alertToAPI(s *storev1.Alert) *metrognomev1.Alert {
	return &metrognomev1.Alert{
		Id:          s.GetId(),
		CustomerId:  s.GetCustomerId(),
		MeterSlug:   s.GetMeterSlug(),
		Threshold:   s.GetThreshold(),
		AlertType:   metrognomev1.AlertType(s.GetAlertType()),
		Triggered:   s.GetTriggered(),
		CreatedAt:   s.GetCreatedAt(),
		WebhookUrl:  s.GetWebhookUrl(),
		TriggeredAt: s.GetTriggeredAt(),
	}
}

func convertAlertType(t metrognomev1.AlertType) storev1.AlertType {
	switch t {
	case metrognomev1.AlertType_ALERT_TYPE_USAGE:
		return storev1.AlertType_ALERT_TYPE_USAGE
	case metrognomev1.AlertType_ALERT_TYPE_SPEND:
		return storev1.AlertType_ALERT_TYPE_SPEND
	default:
		return storev1.AlertType_ALERT_TYPE_USAGE
	}
}
