package simfdb

import (
	"bytes"
	"sort"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// rememberRead retains logical read knowledge for selector reduction, separately from conflicts.
// The fixed-version view supplies values; local writes remain an overlay until reset.
func (tx *simTxn) rememberRead(begin, end []byte) {
	if tx.rywDisabled || bytes.Compare(begin, end) >= 0 {
		return
	}
	merged := keyRange{begin: bytes.Clone(begin), end: bytes.Clone(end)}
	ranges := make([]keyRange, 0, len(tx.knownReads)+1)
	inserted := false
	for _, r := range tx.knownReads {
		switch {
		case bytes.Compare(r.end, merged.begin) < 0:
			ranges = append(ranges, r)
		case bytes.Compare(merged.end, r.begin) < 0:
			if !inserted {
				ranges = append(ranges, merged)
				inserted = true
			}
			ranges = append(ranges, r)
		default:
			if bytes.Compare(r.begin, merged.begin) < 0 {
				merged.begin = r.begin
			}
			if bytes.Compare(r.end, merged.end) > 0 {
				merged.end = r.end
			}
		}
	}
	if !inserted {
		ranges = append(ranges, merged)
	}
	tx.knownReads = ranges
}

func (tx *simTxn) readKnown(key []byte) bool {
	i := sort.Search(len(tx.knownReads), func(i int) bool {
		return bytes.Compare(tx.knownReads[i].end, key) > 0
	})
	return i < len(tx.knownReads) && bytes.Compare(tx.knownReads[i].begin, key) <= 0
}

// A filled batch knows only its consumed prefix or suffix, not the unfetched tail.
func (tx *simTxn) rememberRangeRead(begin, end []byte, rows []fdb.KeyValue, more, reverse bool) {
	if more {
		if len(rows) == 0 {
			return
		}
		if reverse {
			begin = rows[len(rows)-1].Key
		} else {
			end = keyAfter(rows[len(rows)-1].Key)
		}
	}
	tx.rememberRead(begin, end)
}

// selectorReadSpan is the logical span traversed by GetKey, after removeOrEqual.
// Backward selectors exclude their anchor; forward selectors include their resolved key.
func selectorReadSpan(view []fdb.KeyValue, sel fdb.KeySelector) (begin, end []byte) {
	anchor := []byte(sel.Key.FDBKey())
	idx := resolveSelector(view, sel)
	if sel.Offset <= 0 {
		begin, end = nil, anchor
		if idx >= 0 && idx < len(view) {
			begin = view[idx].Key
		}
		return begin, end
	}
	begin, end = anchor, endKeyMarker
	if idx >= 0 && idx < len(view) {
		end = keyAfter(view[idx].Key)
	}
	return begin, end
}
