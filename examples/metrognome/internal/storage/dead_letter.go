package storage

import (
	"context"

	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type DeadLetterStore struct {
	db *DB
}

func (s *DeadLetterStore) Create(ctx context.Context, dl *storev1.DeadLetter) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(dl)
		return nil, err
	})
	return err
}

func (s *DeadLetterStore) List(ctx context.Context, pageSize int, continuation []byte) ([]*storev1.DeadLetter, []byte, error) {
	type result struct {
		items []*storev1.DeadLetter
		cont  []byte
	}
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		props := rl.ForwardScan()
		if pageSize > 0 {
			props.ExecuteProperties.ReturnedRowLimit = pageSize
		}
		cursor := rs.ScanRecordsByType("DeadLetter", continuation, props)
		entries, cont, err := rl.AsListWithContinuation(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.DeadLetter, len(entries))
		for i, e := range entries {
			items[i] = e.Record.(*storev1.DeadLetter)
		}
		return &result{items: items, cont: cont}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	res := r.(*result)
	return res.items, res.cont, nil
}
