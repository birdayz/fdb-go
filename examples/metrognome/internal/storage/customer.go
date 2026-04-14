package storage

import (
	"context"

	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type CustomerStore struct {
	db *DB
}

func (s *CustomerStore) Create(ctx context.Context, c *storev1.Customer) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(c)
		return nil, err
	})
	return err
}

func (s *CustomerStore) Get(ctx context.Context, id string) (*storev1.Customer, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("Customer", id))
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
	return result.(*storev1.Customer), nil
}

func (s *CustomerStore) List(ctx context.Context, pageSize int, continuation []byte) ([]*storev1.Customer, []byte, error) {
	type result struct {
		items []*storev1.Customer
		cont  []byte
	}
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		props := rl.ForwardScan()
		if pageSize > 0 {
			props.ExecuteProperties.ReturnedRowLimit = pageSize
		}
		cursor := rs.ScanRecordsByType("Customer", continuation, props)
		entries, cont, err := rl.AsListWithContinuation(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.Customer, len(entries))
		for i, e := range entries {
			items[i] = e.Record.(*storev1.Customer)
		}
		return &result{items: items, cont: cont}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	res := r.(*result)
	return res.items, res.cont, nil
}
