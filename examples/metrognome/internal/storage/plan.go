package storage

import (
	"context"

	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type PlanStore struct {
	db *DB
}

func (s *PlanStore) Create(ctx context.Context, p *storev1.Plan) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(p)
		return nil, err
	})
	return err
}

func (s *PlanStore) Get(ctx context.Context, id string) (*storev1.Plan, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("Plan", id))
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, ErrNotFound
		}
		return rec.Record, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.Plan), nil
}

func (s *PlanStore) List(ctx context.Context, pageSize int, continuation []byte) ([]*storev1.Plan, []byte, error) {
	type result struct {
		items []*storev1.Plan
		cont  []byte
	}
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		props := rl.ForwardScan()
		if pageSize > 0 {
			props.ExecuteProperties.ReturnedRowLimit = pageSize
		}
		cursor := rs.ScanRecordsByType("Plan", continuation, props)
		entries, cont, err := rl.AsListWithContinuation(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.Plan, len(entries))
		for i, e := range entries {
			items[i] = e.Record.(*storev1.Plan)
		}
		return &result{items: items, cont: cont}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	res := r.(*result)
	return res.items, res.cont, nil
}
