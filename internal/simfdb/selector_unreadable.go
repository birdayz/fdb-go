package simfdb

import (
	"bytes"
	"sort"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// selectorUnreadableError models demanded iterator access, not just the returned data extent.
// RYW reduction stops at unreadable segments; only a subsequently required access throws 1036.
func (tx *simTxn) selectorUnreadableError(view []fdb.KeyValue, begin, end fdb.KeySelector, known []selectorWriteSegment, rows []fdb.KeyValue, more, reverse bool) error {
	if tx.rywDisabled || tx.bypassUnreadable || !tx.hasUnreadable() {
		return nil
	}
	unreadable := fdb.Error{Code: 1036}
	initialBegin, actualBegin := reduceSelectorKnown(begin, known)
	initialEnd, actualEnd := reduceSelectorKnown(end, known)
	empty := func(b, e fdb.KeySelector, ab, ae int) bool {
		return ab >= ae && bytes.Compare(b.Key.FDBKey(), e.Key.FDBKey()) >= 0 ||
			isTrivialSelector(b) && bytes.Equal(b.Key.FDBKey(), endKeyMarker) ||
			isTrivialSelector(e) && len(e.Key.FDBKey()) == 0
	}
	if empty(initialBegin, initialEnd, actualBegin, actualEnd) {
		return nil
	}
	// The real-cache prologue precedes any new storage knowledge (RYW:621-638,925-942).
	if bytes.Compare(initialBegin.Key.FDBKey(), initialEnd.Key.FDBKey()) > 0 {
		sel := initialEnd
		if reverse {
			sel = initialBegin
		}
		if !isTrivialSelector(sel) {
			lo, hi := selectorReadSpan(view, sel)
			if tx.unreadableScanCap(lo, hi, false) != nil {
				return unreadable
			}
		}
	}
	// Readable storage is available to this demand check, but it does not populate the cache.
	segments := tx.selectorSegments(view, true)
	begin, actualBegin = reduceSelectorKnown(begin, segments)
	end, actualEnd = reduceSelectorKnown(end, segments)
	if empty(begin, end, actualBegin, actualEnd) {
		return nil
	}
	origin, far := begin, end
	step, lookahead := 1, 1-end.Offset
	if reverse {
		origin, far = end, begin
		step, lookahead = -1, begin.Offset-1
	}
	if !isTrivialSelector(origin) {
		return unreadable // reduction could not reach the scan origin without unreadable data
	}
	key := origin.Key.FDBKey()
	i := sort.Search(len(segments), func(i int) bool { return bytes.Compare(segments[i].end, key) > 0 })
	if reverse && (i == len(segments) || bytes.Equal(segments[i].begin, key)) {
		i--
	}
	present := make(map[string]bool, len(view))
	for _, row := range view {
		present[string(row.Key)] = true
	}
	seen, past := 0, 0
	reached := false
	for ; i >= 0 && i < len(segments); i += step {
		s := segments[i]
		if isTrivialSelector(far) && ((!reverse && bytes.Compare(s.begin, far.Key.FDBKey()) >= 0) ||
			(reverse && bytes.Compare(s.end, far.Key.FDBKey()) <= 0)) {
			return nil
		}
		// Limits and offset lookahead precede type()/kv(), including rowless barriers.
		if reached && past >= lookahead {
			return nil
		}
		if s.unreadable {
			return unreadable
		}
		if s.kv && present[string(s.begin)] {
			if reached {
				past++
			} else {
				seen++
				reached = more && seen >= len(rows)
			}
		}
	}
	return nil
}
