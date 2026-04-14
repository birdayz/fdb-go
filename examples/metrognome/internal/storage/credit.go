package storage

import (
	"context"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type CreditStore struct {
	db *DB
}

func (s *CreditStore) Create(ctx context.Context, c *storev1.Credit) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(c)
		return nil, err
	})
	return err
}

// ListByCustomer returns credits ordered by priority (ascending) then expiry (ascending).
// This ordering ensures credits are applied in the right order during invoicing.
func (s *CreditStore) ListByCustomer(ctx context.Context, customerID string) ([]*storev1.Credit, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		cursor := rs.ScanIndexRecords("credit_by_customer",
			rl.TupleRangeAllOf(tuple.Tuple{customerID}), nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.Credit, len(entries))
		for i, e := range entries {
			items[i] = e.Record.Record.(*storev1.Credit)
		}
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return result.([]*storev1.Credit), nil
}

// GetBalance returns total remaining credit balance and all active (non-expired) credits.
func (s *CreditStore) GetBalance(ctx context.Context, customerID string) (int64, []*storev1.Credit, error) {
	type result struct {
		total   int64
		credits []*storev1.Credit
	}
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		cursor := rs.ScanIndexRecords("credit_by_customer",
			rl.TupleRangeAllOf(tuple.Tuple{customerID}), nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}
		now := time.Now().UnixMilli()
		var total int64
		var active []*storev1.Credit
		for _, e := range entries {
			c := e.Record.Record.(*storev1.Credit)
			if c.GetRemainingCents() <= 0 {
				continue
			}
			if c.GetExpiresAt() > 0 && c.GetExpiresAt() < now {
				continue
			}
			total += c.GetRemainingCents()
			active = append(active, c)
		}
		return &result{total: total, credits: active}, nil
	})
	if err != nil {
		return 0, nil, err
	}
	res := r.(*result)
	return res.total, res.credits, nil
}

// Save updates an existing credit (e.g. after drawdown during invoicing).
func (s *CreditStore) Save(ctx context.Context, c *storev1.Credit) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(c)
		return nil, err
	})
	return err
}
