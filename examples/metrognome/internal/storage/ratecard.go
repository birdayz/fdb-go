package storage

import (
	"context"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

type RateCardStore struct {
	db *DB
}

func (s *RateCardStore) Create(ctx context.Context, rc *storev1.RateCard) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(rc)
		return nil, err
	})
	return err
}

func (s *RateCardStore) Get(ctx context.Context, id string) (*storev1.RateCard, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("RateCard", id))
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
	return result.(*storev1.RateCard), nil
}

func (s *RateCardStore) List(ctx context.Context, pageSize int, continuation []byte) ([]*storev1.RateCard, []byte, error) {
	type result struct {
		items []*storev1.RateCard
		cont  []byte
	}
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		props := rl.ForwardScan()
		if pageSize > 0 {
			props.ExecuteProperties.ReturnedRowLimit = pageSize
		}
		cursor := rs.ScanRecordsByType("RateCard", continuation, props)
		entries, cont, err := rl.AsListWithContinuation(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.RateCard, len(entries))
		for i, e := range entries {
			items[i] = e.Record.(*storev1.RateCard)
		}
		return &result{items: items, cont: cont}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	res := r.(*result)
	return res.items, res.cont, nil
}

// Rate storage

type RateStore struct {
	db *DB
}

func (s *RateStore) Create(ctx context.Context, r *storev1.Rate) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(r)
		return nil, err
	})
	return err
}

func (s *RateStore) ListByRateCard(ctx context.Context, rateCardID string) ([]*storev1.Rate, error) {
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		props := rl.ForwardScan()
		cursor := rs.ScanRecordsByType("Rate", nil, props)
		entries, _, err := rl.AsListWithContinuation(ctx, cursor)
		if err != nil {
			return nil, err
		}
		var items []*storev1.Rate
		for _, e := range entries {
			rate := e.Record.(*storev1.Rate)
			if rate.GetRateCardId() == rateCardID {
				items = append(items, rate)
			}
		}
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return r.([]*storev1.Rate), nil
}
