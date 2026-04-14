package storage

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type MeterStore struct {
	db *DB
}

func (s *MeterStore) Create(ctx context.Context, m *storev1.Meter) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(m)
		return nil, err
	})
	return err
}

func (s *MeterStore) Get(ctx context.Context, id string) (*storev1.Meter, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("Meter", id))
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
	return result.(*storev1.Meter), nil
}

func (s *MeterStore) GetBySlug(ctx context.Context, slug string) (*storev1.Meter, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		cursor := rs.ScanIndexRecords("meter_by_slug",
			rl.TupleRangeAllOf(tuple.Tuple{slug}), nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, ErrNotFound
		}
		return entries[0].Record.Record, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.Meter), nil
}

func (s *MeterStore) List(ctx context.Context, pageSize int, continuation []byte) ([]*storev1.Meter, []byte, error) {
	type result struct {
		items []*storev1.Meter
		cont  []byte
	}
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		props := rl.ForwardScan()
		if pageSize > 0 {
			props.ExecuteProperties.ReturnedRowLimit = pageSize
		}
		cursor := rs.ScanRecordsByType("Meter", continuation, props)
		entries, cont, err := rl.AsListWithContinuation(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.Meter, len(entries))
		for i, e := range entries {
			items[i] = e.Record.(*storev1.Meter)
		}
		return &result{items: items, cont: cont}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	res := r.(*result)
	return res.items, res.cont, nil
}
