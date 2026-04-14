package storage

import (
	"context"

	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type SessionStore struct {
	db *DB
}

func (s *SessionStore) Create(ctx context.Context, sess *storev1.Session) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(sess)
		return nil, err
	})
	return err
}

func (s *SessionStore) Get(ctx context.Context, id string) (*storev1.Session, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("Session", id))
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
	return result.(*storev1.Session), nil
}
