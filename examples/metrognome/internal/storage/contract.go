package storage

import (
	"context"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type ContractStore struct {
	db *DB
}

func (s *ContractStore) Create(ctx context.Context, c *storev1.Contract) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(c)
		return nil, err
	})
	return err
}

func (s *ContractStore) Get(ctx context.Context, id string) (*storev1.Contract, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("Contract", id))
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
	return result.(*storev1.Contract), nil
}

func (s *ContractStore) ListByCustomer(ctx context.Context, customerID string) ([]*storev1.Contract, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		cursor := rs.ScanIndexRecords("contract_by_customer",
			rl.TupleRangeAllOf(tuple.Tuple{customerID}), nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.Contract, len(entries))
		for i, e := range entries {
			items[i] = e.Record.Record.(*storev1.Contract)
		}
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return result.([]*storev1.Contract), nil
}

// End terminates a contract by setting active=false and end_at.
func (s *ContractStore) End(ctx context.Context, id string, endAt int64) (*storev1.Contract, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("Contract", id))
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, ErrNotFound
		}
		contract := rec.Record.(*storev1.Contract)
		if endAt == 0 {
			endAt = time.Now().UnixMilli()
		}
		contract.EndAt = proto.Int64(endAt)
		contract.Active = proto.Bool(false)
		if _, err := rs.SaveRecord(contract); err != nil {
			return nil, err
		}
		return contract, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.Contract), nil
}
