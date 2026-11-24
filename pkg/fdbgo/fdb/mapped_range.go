package fdb

import (
	"bytes"
	"fmt"
	"unsafe"

	"fdb.dev/pkg/fdbgo/client"
)

// MappedResultKind names which arm of the per-row union is populated. It mirrors
// the std::variant index in MappedReqAndResultRef, and the underlying values are
// the flatbuffers union tags (1-based, 0 = unset) so a decode is a direct read.
type MappedResultKind uint8

const (
	// MappedResultNone means the row carried no secondary result at all. The
	// storage server does not produce this, but the tag is representable on the
	// wire (tag 0), so it is named rather than silently conflated with a
	// point-lookup miss — those are different facts.
	MappedResultNone MappedResultKind = 0
	// MappedResultGetValue: the mapper resolved to a single key.
	MappedResultGetValue MappedResultKind = 1
	// MappedResultGetRange: the mapper ended in a "{...}" range descriptor and
	// resolved to a key range.
	MappedResultGetRange MappedResultKind = 2
)

func (k MappedResultKind) String() string {
	switch k {
	case MappedResultNone:
		return "none"
	case MappedResultGetValue:
		return "getValue"
	case MappedResultGetRange:
		return "getRange"
	}
	return fmt.Sprintf("MappedResultKind(%d)", uint8(k))
}

// MappedKeySelector mirrors C++ KeySelectorRef (FDBTypes.h:631-700) for the
// resolved secondary-range selector bounds returned in MappedGetRange.
type MappedKeySelector struct {
	Key     []byte
	OrEqual bool
	Offset  int32
}

// MappedGetValue is the point-lookup arm (C++ GetValueReqAndResultRef,
// FDBTypes.h:855). Present distinguishes "the mapped key holds an empty value"
// from "the mapped key does not exist" — the C++ field is Optional<ValueRef> and
// collapsing the two would silently invent a record.
type MappedGetValue struct {
	Key     []byte
	Value   []byte
	Present bool
}

// MappedGetRange is the range arm (C++ GetRangeReqAndResultRef,
// FDBTypes.h:873). Begin/End are the selectors the server actually resolved, so
// a range that matched nothing still reports what it probed.
type MappedGetRange struct {
	Begin MappedKeySelector
	End   MappedKeySelector
	Rows  []KeyValue
	More  bool
}

// MappedKeyValue is one row of a mapped range: the primary row (Key/Value —
// the index entry that was scanned) plus the secondary read the mapper resolved
// it to. Mirrors C++ MappedKeyValueRef (FDBTypes.h:895), whose KeyValueRef base
// carries the primary row.
type MappedKeyValue struct {
	Key      []byte
	Value    []byte
	Kind     MappedResultKind
	GetValue MappedGetValue
	GetRange MappedGetRange
}

// MappedKeyValue and client.MappedKeyValue (plus nested structs) share identical
// memory layouts so ownedMappedKeyValues reinterprets slice headers with zero allocations.
const (
	_ = unsafe.Sizeof(MappedResultKind(0)) - unsafe.Sizeof(client.MappedResultKind(0))
	_ = unsafe.Sizeof(client.MappedResultKind(0)) - unsafe.Sizeof(MappedResultKind(0))

	_ = unsafe.Sizeof(MappedKeySelector{}) - unsafe.Sizeof(client.KeySelector{})
	_ = unsafe.Sizeof(client.KeySelector{}) - unsafe.Sizeof(MappedKeySelector{})
	_ = unsafe.Alignof(MappedKeySelector{}) - unsafe.Alignof(client.KeySelector{})
	_ = unsafe.Alignof(client.KeySelector{}) - unsafe.Alignof(MappedKeySelector{})
	_ = unsafe.Offsetof(MappedKeySelector{}.Key) - unsafe.Offsetof(client.KeySelector{}.Key)
	_ = unsafe.Offsetof(client.KeySelector{}.Key) - unsafe.Offsetof(MappedKeySelector{}.Key)
	_ = unsafe.Offsetof(MappedKeySelector{}.OrEqual) - unsafe.Offsetof(client.KeySelector{}.OrEqual)
	_ = unsafe.Offsetof(client.KeySelector{}.OrEqual) - unsafe.Offsetof(MappedKeySelector{}.OrEqual)
	_ = unsafe.Offsetof(MappedKeySelector{}.Offset) - unsafe.Offsetof(client.KeySelector{}.Offset)
	_ = unsafe.Offsetof(client.KeySelector{}.Offset) - unsafe.Offsetof(MappedKeySelector{}.Offset)

	_ = unsafe.Sizeof(MappedGetValue{}) - unsafe.Sizeof(client.MappedGetValue{})
	_ = unsafe.Sizeof(client.MappedGetValue{}) - unsafe.Sizeof(MappedGetValue{})
	_ = unsafe.Alignof(MappedGetValue{}) - unsafe.Alignof(client.MappedGetValue{})
	_ = unsafe.Alignof(client.MappedGetValue{}) - unsafe.Alignof(MappedGetValue{})
	_ = unsafe.Offsetof(MappedGetValue{}.Key) - unsafe.Offsetof(client.MappedGetValue{}.Key)
	_ = unsafe.Offsetof(client.MappedGetValue{}.Key) - unsafe.Offsetof(MappedGetValue{}.Key)
	_ = unsafe.Offsetof(MappedGetValue{}.Value) - unsafe.Offsetof(client.MappedGetValue{}.Value)
	_ = unsafe.Offsetof(client.MappedGetValue{}.Value) - unsafe.Offsetof(MappedGetValue{}.Value)
	_ = unsafe.Offsetof(MappedGetValue{}.Present) - unsafe.Offsetof(client.MappedGetValue{}.Present)
	_ = unsafe.Offsetof(client.MappedGetValue{}.Present) - unsafe.Offsetof(MappedGetValue{}.Present)

	_ = unsafe.Sizeof(MappedGetRange{}) - unsafe.Sizeof(client.MappedGetRange{})
	_ = unsafe.Sizeof(client.MappedGetRange{}) - unsafe.Sizeof(MappedGetRange{})
	_ = unsafe.Alignof(MappedGetRange{}) - unsafe.Alignof(client.MappedGetRange{})
	_ = unsafe.Alignof(client.MappedGetRange{}) - unsafe.Alignof(MappedGetRange{})
	_ = unsafe.Offsetof(MappedGetRange{}.Begin) - unsafe.Offsetof(client.MappedGetRange{}.Begin)
	_ = unsafe.Offsetof(client.MappedGetRange{}.Begin) - unsafe.Offsetof(MappedGetRange{}.Begin)
	_ = unsafe.Offsetof(MappedGetRange{}.End) - unsafe.Offsetof(client.MappedGetRange{}.End)
	_ = unsafe.Offsetof(client.MappedGetRange{}.End) - unsafe.Offsetof(MappedGetRange{}.End)
	_ = unsafe.Offsetof(MappedGetRange{}.Rows) - unsafe.Offsetof(client.MappedGetRange{}.Rows)
	_ = unsafe.Offsetof(client.MappedGetRange{}.Rows) - unsafe.Offsetof(MappedGetRange{}.Rows)
	_ = unsafe.Offsetof(MappedGetRange{}.More) - unsafe.Offsetof(client.MappedGetRange{}.More)
	_ = unsafe.Offsetof(client.MappedGetRange{}.More) - unsafe.Offsetof(MappedGetRange{}.More)

	_ = unsafe.Sizeof(MappedKeyValue{}) - unsafe.Sizeof(client.MappedKeyValue{})
	_ = unsafe.Sizeof(client.MappedKeyValue{}) - unsafe.Sizeof(MappedKeyValue{})
	_ = unsafe.Alignof(MappedKeyValue{}) - unsafe.Alignof(client.MappedKeyValue{})
	_ = unsafe.Alignof(client.MappedKeyValue{}) - unsafe.Alignof(MappedKeyValue{})
	_ = unsafe.Offsetof(MappedKeyValue{}.Key) - unsafe.Offsetof(client.MappedKeyValue{}.Key)
	_ = unsafe.Offsetof(client.MappedKeyValue{}.Key) - unsafe.Offsetof(MappedKeyValue{}.Key)
	_ = unsafe.Offsetof(MappedKeyValue{}.Value) - unsafe.Offsetof(client.MappedKeyValue{}.Value)
	_ = unsafe.Offsetof(client.MappedKeyValue{}.Value) - unsafe.Offsetof(MappedKeyValue{}.Value)
	_ = unsafe.Offsetof(MappedKeyValue{}.Kind) - unsafe.Offsetof(client.MappedKeyValue{}.Kind)
	_ = unsafe.Offsetof(client.MappedKeyValue{}.Kind) - unsafe.Offsetof(MappedKeyValue{}.Kind)
	_ = unsafe.Offsetof(MappedKeyValue{}.GetValue) - unsafe.Offsetof(client.MappedKeyValue{}.GetValue)
	_ = unsafe.Offsetof(client.MappedKeyValue{}.GetValue) - unsafe.Offsetof(MappedKeyValue{}.GetValue)
	_ = unsafe.Offsetof(MappedKeyValue{}.GetRange) - unsafe.Offsetof(client.MappedKeyValue{}.GetRange)
	_ = unsafe.Offsetof(client.MappedKeyValue{}.GetRange) - unsafe.Offsetof(MappedKeyValue{}.GetRange)
)

func ownedMappedKeyValues(rows []client.MappedKeyValue) []MappedKeyValue {
	if len(rows) == 0 {
		return nil
	}
	return unsafe.Slice((*MappedKeyValue)(unsafe.Pointer(unsafe.SliceData(rows))), len(rows))
}

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
	return ownedMappedKeyValues(rows), more, convertError(err)
}

func (rr goMappedRangeResult) GetSliceWithError() ([]MappedKeyValue, error) {
	if rr.err != nil {
		return nil, rr.err
	}
	rawRows, more, err := rr.first.Get()
	if err != nil {
		return nil, convertError(err)
	}
	// Copy the batch header before append, keeping the shared first future intact.
	rows := append([]MappedKeyValue(nil), ownedMappedKeyValues(rawRows)...)
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
		var rawRows []client.MappedKeyValue
		rawRows, more, err = it.first.Get()
		it.first = nil
		rows = ownedMappedKeyValues(rawRows)
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
