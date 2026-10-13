package client

import (
	"bytes"
	"context"
	"sync"

	"fdb.dev/pkg/fdbgo/wire"
)

// PendingMappedRange is a started mapped read. Repeated Get calls return the same
// caller-owned result; its bytes are never recycled by a later read.
type PendingMappedRange struct {
	once sync.Once
	run  func() ([]MappedKeyValue, bool, error)
	rows []MappedKeyValue
	more bool
	err  error
}

func (p *PendingMappedRange) Get() ([]MappedKeyValue, bool, error) {
	p.once.Do(func() {
		p.rows, p.more, p.err = p.run()
		p.run = nil
	})
	return p.rows, p.more, p.err
}

// StartGetMappedRange captures the transaction incarnation and arguments before
// starting I/O, so Reset cannot move the pending read into the new transaction.
func (tx *Transaction) StartGetMappedRange(parent context.Context, begin, end, mapper []byte, limit, byteTarget int, reverse bool) *PendingMappedRange {
	return tx.StartGetMappedRangeWithSkip(parent, begin, end, mapper, limit, byteTarget, 0, reverse)
}

// StartGetMappedRangeWithSkip captures Skip with the original incarnation and
// arguments. The pending read does not introduce an issued-read Commit barrier.
func (tx *Transaction) StartGetMappedRangeWithSkip(parent context.Context, begin, end, mapper []byte, limit, byteTarget int, skip int32, reverse bool) *PendingMappedRange {
	ctx, release := tx.opContext(parent)
	begin, end, mapper = bytes.Clone(begin), bytes.Clone(end), bytes.Clone(mapper)
	p := &PendingMappedRange{run: func() (rows []MappedKeyValue, more bool, err error) {
		defer release()
		if tx.rangeReadBackstop().run(func() {
			rows, more, err = tx.GetMappedRangeWithByteTargetAndSkip(ctx, begin, end, mapper, limit, byteTarget, skip, reverse)
		}) {
			err = tx.trackReadOperation(ctx, &wire.FDBError{Code: ErrInternalError})
			return nil, false, err
		}
		return rows, more, err
	}}
	go p.Get()
	return p
}
