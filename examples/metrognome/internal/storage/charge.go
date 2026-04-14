package storage

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type ChargeStore struct {
	db *DB
}

func (s *ChargeStore) Create(ctx context.Context, c *storev1.Charge) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(c)
		return nil, err
	})
	return err
}

func (s *ChargeStore) ListByPlan(ctx context.Context, planID string) ([]*storev1.Charge, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		cursor := rs.ScanIndexRecords("charge_by_plan",
			rl.TupleRangeAllOf(tuple.Tuple{planID}), nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.Charge, len(entries))
		for i, e := range entries {
			items[i] = e.Record.Record.(*storev1.Charge)
		}
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return result.([]*storev1.Charge), nil
}
