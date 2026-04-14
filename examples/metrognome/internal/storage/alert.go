package storage

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type AlertStore struct {
	db *DB
}

func (s *AlertStore) Create(ctx context.Context, a *storev1.Alert) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(a)
		return nil, err
	})
	return err
}

func (s *AlertStore) ListByCustomer(ctx context.Context, customerID string) ([]*storev1.Alert, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		cursor := rs.ScanIndexRecords("alert_by_customer",
			rl.TupleRangeAllOf(tuple.Tuple{customerID}), nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.Alert, len(entries))
		for i, e := range entries {
			items[i] = e.Record.Record.(*storev1.Alert)
		}
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return result.([]*storev1.Alert), nil
}
