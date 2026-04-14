package storage

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

type InvoiceStore struct {
	db *DB
}

func (s *InvoiceStore) Create(ctx context.Context, inv *storev1.Invoice) error {
	_, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(inv)
		return nil, err
	})
	return err
}

func (s *InvoiceStore) Get(ctx context.Context, id string) (*storev1.Invoice, error) {
	result, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.db.pk("Invoice", id))
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
	return result.(*storev1.Invoice), nil
}

func (s *InvoiceStore) ListByCustomer(ctx context.Context, customerID string, pageSize int, continuation []byte) ([]*storev1.Invoice, []byte, error) {
	type result struct {
		items []*storev1.Invoice
		cont  []byte
	}
	r, err := s.db.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		props := rl.ForwardScan()
		if pageSize > 0 {
			props.ExecuteProperties.ReturnedRowLimit = pageSize
		}
		cursor := rs.ScanIndexRecords("invoice_by_customer",
			rl.TupleRangeAllOf(tuple.Tuple{customerID}), continuation, props)
		entries, cont, err := rl.AsListWithContinuation(ctx, cursor)
		if err != nil {
			return nil, err
		}
		items := make([]*storev1.Invoice, len(entries))
		for i, e := range entries {
			items[i] = e.Record.Record.(*storev1.Invoice)
		}
		return &result{items: items, cont: cont}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	res := r.(*result)
	return res.items, res.cont, nil
}
