package fdb

import (
	"bytes"

	"fdb.dev/pkg/fdbgo/client"
)

// MappedKeyValue includes the primary row and the server-resolved secondary read.
// The aliases preserve both union arms without allocating wrapper objects per row.
type (
	MappedKeyValue   = client.MappedKeyValue
	MappedResultKind = client.MappedResultKind
	MappedGetValue   = client.MappedGetValue
	MappedGetRange   = client.MappedGetRange
)

const (
	MappedResultNone     = client.MappedResultNone
	MappedResultGetValue = client.MappedResultGetValue
	MappedResultGetRange = client.MappedResultGetRange
)

// MappedRangeResult is a serializable mapped read over an exact primary range.
type MappedRangeResult interface {
	GetSliceWithError() ([]MappedKeyValue, error)
	GetSliceOrPanic() []MappedKeyValue
	Iterator() MappedRangeIterator
}

// MappedRangeIterator advances primary rows in scan order. Secondary range rows
// always arrive forward. Returned bytes remain valid after subsequent batches.
type MappedRangeIterator interface {
	Advance() bool
	Get() (MappedKeyValue, error)
	MustGet() MappedKeyValue
	SetTraceLog(fn func(iteration, requested, returned int, more bool, err error))
}

var (
	_ MappedRangeReader     = Transaction{}
	_ MappedRangeSkipReader = Transaction{}
)

// GetMappedRange starts a mapped read. Mapping is performed by the storage server;
// snapshot isolation and disabled read-your-writes are not supported by FDB.
func (tr Transaction) GetMappedRange(r ExactRange, mapper []byte, options RangeOptions) MappedRangeResult {
	return tr.GetMappedRangeWithSkip(r, mapper, options, 0)
}

// GetMappedRangeWithSkip skips primary rows on batch one only, resolving the
// position client-side. Nonpositive values behave like zero; Skip never wraps.
func (tr Transaction) GetMappedRangeWithSkip(r ExactRange, mapper []byte, options RangeOptions, skip int32) MappedRangeResult {
	begin, end := cloneRangeKeys(r)
	rr := goMappedRangeResult{tx: tr.t, begin: begin, end: end, mapper: bytes.Clone(mapper), options: options, limit: effectiveLimit(options.Limit)}
	if options.Mode == StreamingModeExact && (options.Limit == 0 || options.Limit == -1) {
		rr.err = Error{Code: 2210}
		return rr
	}
	byteTarget, err := modeTargetBytes(options.Mode, 1)
	if err != nil {
		rr.err = err
		return rr
	}
	rr.first = tr.t.inner.StartGetMappedRangeWithSkip(tr.t.ctx, begin, end, rr.mapper, rr.limit, byteTarget, skip, options.Reverse)
	return rr
}

type goMappedRangeResult struct {
	tx                 *transaction
	begin, end, mapper []byte
	options            RangeOptions
	limit              int
	first              *client.PendingMappedRange
	err                error
}

func (rr goMappedRangeResult) read(begin, end []byte, limit, byteTarget int) ([]MappedKeyValue, bool, error) {
	rows, more, err := rr.tx.inner.GetMappedRangeWithByteTarget(rr.tx.ctx, begin, end, rr.mapper, limit, byteTarget, rr.options.Reverse)
	return rows, more, convertError(err)
}

func (rr goMappedRangeResult) GetSliceWithError() ([]MappedKeyValue, error) {
	if rr.err != nil {
		return nil, rr.err
	}
	rows, more, err := rr.first.Get()
	if err != nil {
		return nil, convertError(err)
	}
	// Copy the batch header before append, keeping the shared first future intact.
	rows = append([]MappedKeyValue(nil), rows...)
	begin, end, remaining := rr.begin, rr.end, rr.limit-len(rows)
	for more && remaining > 0 && len(rows) > 0 {
		last := rows[len(rows)-1].Key
		if rr.options.Reverse {
			end = last
		} else {
			begin = keyAfter(last)
		}
		var rest []MappedKeyValue
		rest, more, err = rr.read(begin, end, remaining, client.ByteLimitUnlimited)
		if err != nil {
			return nil, err
		}
		if len(rest) == 0 {
			break
		}
		rows = append(rows, rest...)
		remaining -= len(rest)
	}
	return rows, nil
}

func (rr goMappedRangeResult) GetSliceOrPanic() []MappedKeyValue {
	rows, err := rr.GetSliceWithError()
	if err != nil {
		panic(err)
	}
	return rows
}

func (rr goMappedRangeResult) Iterator() MappedRangeIterator {
	return &goMappedRangeIterator{rr: rr, begin: rr.begin, end: rr.end, remaining: rr.limit, iteration: 1, index: -1, first: rr.first, err: rr.err}
}

type goMappedRangeIterator struct {
	rr                   goMappedRangeResult
	begin, end           []byte
	remaining, iteration int
	rows                 []MappedKeyValue
	pos, index           int
	exhausted            bool
	first                *client.PendingMappedRange
	err                  error
	traceLog             func(iteration, requested, returned int, more bool, err error)
}

func (it *goMappedRangeIterator) SetTraceLog(fn func(iteration, requested, returned int, more bool, err error)) {
	it.traceLog = fn
}

func (it *goMappedRangeIterator) Advance() bool {
	if it.err != nil {
		return false
	}
	if it.pos < len(it.rows) {
		it.index = it.pos
		it.pos++
		return true
	}
	if it.first == nil && (it.exhausted || it.remaining <= 0) {
		return false
	}
	var rows []MappedKeyValue
	var more bool
	var err error
	if it.first != nil {
		rows, more, err = it.first.Get()
		it.first = nil
		err = convertError(err)
	} else {
		var byteTarget int
		byteTarget, err = modeTargetBytes(it.rr.options.Mode, it.iteration)
		if err == nil {
			rows, more, err = it.rr.read(it.begin, it.end, it.remaining, byteTarget)
		}
	}
	if it.traceLog != nil {
		it.traceLog(it.iteration, it.remaining, len(rows), more, err)
	}
	it.iteration++
	if err != nil {
		it.err = err
		return false
	}
	if len(rows) == 0 {
		it.exhausted = true
		return false
	}
	it.rows = rows
	it.index, it.pos = 0, 1
	it.remaining -= len(rows)
	last := rows[len(rows)-1].Key
	if it.rr.options.Reverse {
		it.end = bytes.Clone(last)
	} else {
		it.begin = keyAfter(last)
	}
	it.exhausted = !more
	return true
}

func (it *goMappedRangeIterator) Get() (MappedKeyValue, error) {
	if it.err != nil {
		return MappedKeyValue{}, it.err
	}
	if it.index < 0 || it.index >= len(it.rows) {
		return MappedKeyValue{}, nil
	}
	return it.rows[it.index], nil
}

func (it *goMappedRangeIterator) MustGet() MappedKeyValue {
	row, err := it.Get()
	if err != nil {
		panic(err)
	}
	return row
}
