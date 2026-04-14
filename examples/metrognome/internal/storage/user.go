package storage

import (
	"context"

	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type UserStore struct {
	db *DB
}

func (s *UserStore) Create(ctx context.Context, u *storev1.User) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(u)
		return nil, err
	})
	return err
}

func (s *UserStore) Save(ctx context.Context, u *storev1.User) error {
	return s.Create(ctx, u) // SaveRecord is upsert
}

func (s *UserStore) Get(ctx context.Context, id string) (*storev1.User, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("User", id))
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
	return result.(*storev1.User), nil
}
