package simfdb

import (
	"bytes"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// selector_range.go models a range read whose selectors are not both trivial (an offset other
// than 1), as the pure-Go client issues it: one getRange that resolves the selectors within the
// read, with the read-conflict range of C++ (RFC-262 D10). Line numbers are FoundationDB 7.3.77:
// ReadYourWrites.actor.cpp (RYW) and NativeAPI.actor.cpp (NAPI).
//
// The rows are the selectors resolved over the merged view and read between, as before. What
// differs from a pair of getKeys plus an exact range is the conflict:
//
//   - RYW: addConflictRange(GetRangeReq<false/true>) (RYW:245-319) over the request's selectors
//     and the result, filtered through the write map (updateConflictMap, RYW:334-351).
//   - RYW disabled: getRangeFinished's range (NAPI:4558-4586), after readThrough resolves a
//     reverse read's end of offset > 1 with getKey, which takes getKey's own range
//     (RYW:191-199, NAPI:5761-5787).

// simSelectors is what is left of a selector range read. The client keeps the selectors and
// re-resolves them in every batch, continuing with FirstGreaterThan(last) forward and
// FirstGreaterOrEqual(last) reverse while keeping the other selector, as Apple's binding does
// (bindings/go/src/fdb/range.go:229-233; fdb/range_result.go rangeBounds.advance).
type simSelectors struct {
	on         bool
	begin, end fdb.KeySelector
}

func (s *simSelectors) advance(last []byte, reverse bool) {
	if reverse {
		s.end = fdb.FirstGreaterOrEqual(fdb.Key(bytes.Clone(last)))
	} else {
		s.begin = fdb.FirstGreaterThan(fdb.Key(bytes.Clone(last)))
	}
}

// isTrivialSelector reports a selector the client turns into a byte key without reading:
// firstGreaterOrEqual(k) is k and firstGreaterThan(k) is k+\x00 (FDBTypes.h:645-650). A range
// whose two selectors are both trivial is an exact range (fdb/range_result.go isTrivialSelector).
func isTrivialSelector(ks fdb.KeySelector) bool { return ks.Offset == 1 }

// removeOrEqual is KeySelectorRef::removeOrEqual (FDBTypes.h:645-650).
func removeOrEqual(ks fdb.KeySelector) fdb.KeySelector {
	if ks.OrEqual {
		return fdb.KeySelector{Key: fdb.Key(keyAfter([]byte(ks.Key.FDBKey()))), Offset: ks.Offset}
	}
	return fdb.KeySelector{Key: fdb.Key(bytes.Clone(ks.Key.FDBKey())), Offset: ks.Offset}
}

// selectorsInverted is the empty-range check of RYW getRange (RYW:1758-1761) and NativeAPI
// getRangeInternal (NAPI:5830-5833), on selectors after removeOrEqual. Such a read records no
// conflict.
func selectorsInverted(begin, end fdb.KeySelector) bool {
	return begin.Offset >= end.Offset && bytes.Compare(begin.Key.FDBKey(), end.Key.FDBKey()) >= 0
}

// selectorFetch is one getRange over selectors: one batch, its rows and more, and its read
// conflict when the read is not a snapshot.
func (tx *simTxn) selectorFetch(begin, end fdb.KeySelector, limit, byteTarget int, reverse, snapshot bool) ([]fdb.KeyValue, bool, error) {
	if tx.cancelled.Load() {
		return nil, false, fdb.Error{Code: 1025}
	}
	tx.ensureReadVersion()
	begin, end = removeOrEqual(begin), removeOrEqual(end)
	if selectorsInverted(begin, end) {
		return nil, false, nil
	}
	view := tx.buildView()
	if tx.rywDisabled && reverse && end.Offset > 1 {
		// readThrough (RYW:191-199): getKey(end) under the read's snapshot flag, clamped to
		// getMaxReadKey, then the range up to firstGreaterOrEqual of it.
		key := resolveRangeBound(view, end)
		if !snapshot {
			tx.addGetKeyConflictRange(end, key)
		}
		end = fdb.FirstGreaterOrEqual(fdb.Key(key))
		if selectorsInverted(begin, end) {
			return nil, false, nil
		}
	}
	bIdx, eIdx := resolveSelector(view, begin), resolveSelector(view, end)
	b, e := resolveRangeBound(view, begin), resolveRangeBound(view, end)
	var kvs []fdb.KeyValue
	var more bool
	if bytes.Compare(b, e) < 0 {
		var err error
		if kvs, more, _, _, err = tx.fetchRange(b, e, limit, byteTarget, reverse); err != nil {
			return nil, false, err
		}
	}
	if snapshot { // a snapshot read adds no conflict (RYW:387, NAPI:5841-5844)
		return kvs, more, nil
	}
	if tx.rywDisabled {
		// NativeAPI sets readToBegin in getRange's prologue (NAPI:4623-4626) or when
		// getRangeFallback resolves begin to allKeys.begin (:4482-4483): here, a begin that
		// resolves before the first key. Its readThroughEnd needs end to resolve to \xff\xff
		// (:4490-4491), past the system keyspace this simulator does not hold.
		readToBegin := (len(begin.Key.FDBKey()) == 0 && begin.Offset < 1) || (bIdx < 0 && (!reverse || !more))
		cb, ce := getRangeFinishedConflict(begin, end, kvs, more, readToBegin, false, reverse)
		tx.addFilteredReadConflict(cb, ce) // adds the range as is: RYW is disabled
		return kvs, more, nil
	}
	// The walk sets readToBegin when a selector resolves before the first key and
	// readThroughEnd when one resolves more than one key past the last
	// (resolveKeySelectorFromCache, RYW:458-470); the epilogue keeps the scan direction's flag
	// only when the read did not stop on a limit (RYW:799-811, 1111-1120).
	readToBegin := bIdx < 0 || eIdx < 0
	readThroughEnd := bIdx > len(view) || eIdx > len(view)
	// Crossed anchors resolve the far bound with getKey in the prologue (RYW:621-638,
	// 925-942). Its maxKey result sets readThroughEnd even exactly one past the last key.
	if bytes.Compare(begin.Key.FDBKey(), end.Key.FDBKey()) > 0 {
		if reverse && !isTrivialSelector(begin) && bIdx >= len(view) ||
			!reverse && !isTrivialSelector(end) && eIdx >= len(view) {
			readThroughEnd = true
		}
	}
	if reverse {
		readToBegin = readToBegin && !more
	} else {
		readThroughEnd = readThroughEnd && !more
	}
	cb, ce := rywRangeConflictSpan(begin, end, kvs, more, readToBegin, readThroughEnd, reverse, endKeyMarker)
	tx.addFilteredReadConflict(cb, ce)
	return kvs, more, nil
}

// rywRangeConflictSpan is addConflictRange(GetRangeReq<false>) (RYW:245-281) and
// addConflictRange(GetRangeReq<true>) (:284-319) before updateConflictMap, from the request's
// selectors (after removeOrEqual) and its result in scan order. It is the client's
// rywRangeConflictSpan (pkg/fdbgo/client/range_selector_issue.go).
func rywRangeConflictSpan(begin, end fdb.KeySelector, kvs []fdb.KeyValue, more, readToBegin, readThroughEnd, reverse bool, maxKey []byte) (rangeBegin, rangeEnd []byte) {
	bk, ek := []byte(begin.Key.FDBKey()), []byte(end.Key.FDBKey())
	if bytes.Compare(bk, ek) < 0 {
		rangeBegin, rangeEnd = bk, ek
		if !reverse && end.Offset > 0 && more { // :257
			rangeEnd = bk
		}
		if reverse && begin.Offset <= 1 && more { // :295
			rangeBegin = ek
		}
	} else {
		rangeBegin, rangeEnd = ek, bk
	}
	if readToBegin && begin.Offset <= 0 {
		rangeBegin = []byte{}
	}
	if readThroughEnd && end.Offset > 0 {
		rangeEnd = maxKey
	}
	n := len(kvs)
	if n == 0 {
		return rangeBegin, rangeEnd
	}
	if !reverse { // :268-275
		if first := []byte(kvs[0].Key); begin.Offset <= 0 && bytes.Compare(first, rangeBegin) < 0 {
			rangeBegin = first
		}
		if last := []byte(kvs[n-1].Key); bytes.Compare(rangeEnd, last) <= 0 {
			rangeEnd = keyAfter(last)
		}
		return rangeBegin, rangeEnd
	}
	// :307-313: the result is descending, so its last row is the smallest key.
	if last := []byte(kvs[n-1].Key); bytes.Compare(last, rangeBegin) < 0 {
		rangeBegin = last
	}
	if first := []byte(kvs[0].Key); end.Offset > 0 && bytes.Compare(rangeEnd, first) <= 0 {
		rangeEnd = keyAfter(first)
	}
	return rangeBegin, rangeEnd
}

// allKeysEnd is \xff\xff, NativeAPI's allKeys.end.
var allKeysEnd = []byte{0xff, 0xff}

// getRangeFinishedConflict is getRangeFinished's read-conflict range (NAPI:4558-4586), from the
// call's selectors (after removeOrEqual) and its result in scan order. It is the client's
// getRangeFinishedConflict (pkg/fdbgo/client/native_range.go).
func getRangeFinishedConflict(begin, end fdb.KeySelector, kvs []fdb.KeyValue, more, readToBegin, readThroughEnd, reverse bool) (rangeBegin, rangeEnd []byte) {
	bk, ek := []byte(begin.Key.FDBKey()), []byte(end.Key.FDBKey())
	n := len(kvs)
	switch {
	case readToBegin:
		rangeBegin = []byte{}
	case ((!reverse || !more || begin.Offset > 1) && begin.Offset > 0) || n == 0:
		rangeBegin = bk
	case reverse:
		rangeBegin = []byte(kvs[n-1].Key)
	default:
		rangeBegin = []byte(kvs[0].Key)
	}
	if end.Offset > begin.Offset && bytes.Compare(ek, rangeBegin) < 0 {
		rangeBegin = ek
	}
	switch {
	case readThroughEnd:
		rangeEnd = allKeysEnd
	case ((reverse || !more || end.Offset <= 0) && end.Offset <= 1) || n == 0:
		rangeEnd = ek
	case reverse:
		rangeEnd = keyAfter([]byte(kvs[0].Key))
	default:
		rangeEnd = keyAfter([]byte(kvs[n-1].Key))
	}
	if begin.Offset < end.Offset && bytes.Compare(bk, rangeEnd) > 0 {
		rangeEnd = bk
	}
	return rangeBegin, rangeEnd
}
