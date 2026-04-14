package storage

import (
	"context"

	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type KafkaOffsetStore struct {
	db *DB
}

func (s *KafkaOffsetStore) Get(ctx context.Context, topic string, partition int32) (*storev1.KafkaOffset, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("KafkaOffset", topic, int64(partition)))
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
	return result.(*storev1.KafkaOffset), nil
}

func (s *KafkaOffsetStore) Save(ctx context.Context, o *storev1.KafkaOffset) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(o)
		return nil, err
	})
	return err
}
