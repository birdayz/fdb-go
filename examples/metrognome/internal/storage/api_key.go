package storage

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type ApiKeyStore struct {
	db *DB
}

func (s *ApiKeyStore) Create(ctx context.Context, k *storev1.ApiKey) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(k)
		return nil, err
	})
	return err
}

func (s *ApiKeyStore) Save(ctx context.Context, k *storev1.ApiKey) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(k)
		return nil, err
	})
	return err
}

func (s *ApiKeyStore) List(ctx context.Context) ([]*storev1.ApiKey, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		cursor := rs.ScanRecordsByType("ApiKey", nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.ApiKey, len(entries))
		for i, e := range entries {
			items[i] = e.Record.(*storev1.ApiKey)
		}
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return result.([]*storev1.ApiKey), nil
}

func (s *ApiKeyStore) GetByKeyHash(ctx context.Context, keyHash string) (*storev1.ApiKey, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		idx := rs.GetRecordMetaData().GetIndex("apikey_by_hash")
		entries, err := rl.AsList(ctx, rs.ScanIndex(idx,
			rl.TupleRangeAllOf(tuple.Tuple{keyHash}), nil, rl.ForwardScan()))
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, ErrNotFound
		}
		pk := entries[0].PrimaryKey()
		rec, err := rs.LoadRecord(pk)
		if err != nil {
			return nil, err
		}
		return rec.Record.(*storev1.ApiKey), nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.ApiKey), nil
}
