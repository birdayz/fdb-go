package simfdb

import (
	"bytes"
	"sort"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// versionedValue is one committed version of a key. A nil value marks a tombstone (the key
// was cleared at this commit version). Within an entry, versions are strictly ascending.
type versionedValue struct {
	version int64
	value   []byte
}

// storeEntry is a key plus its full version history (ascending by version). History is never
// GC'd in v1: RFC-199 Tier 1 item 5 — retaining the full history is strictly safer than a real
// cluster (SimFDB can always prove a conflict exactly and never GCs below the MVCC window, so
// it never falsely reports "committed"). The transaction_too_old(1007) verdict is emulated by
// pure version arithmetic in conflict.go, not by actually discarding history.
type storeEntry struct {
	key     []byte
	history []versionedValue
}

// mvccStore is a sorted in-memory multi-version keyspace. Entries are kept sorted by key so
// range reads are ordered deterministically — deliberately a sorted slice, never a Go map,
// whose iteration order is randomized. Point lookup and range bounds are binary searches;
// insertion of a brand-new key is O(n) (fine for the sim's scale — correctness over speed in
// v1; a btree is a drop-in optimization later).
//
// The store holds only COMMITTED data. A transaction's uncommitted writes live in its RYW
// buffer (txn.go) and are merged over a snapshot read here.
type mvccStore struct {
	entries []*storeEntry
}

// search returns the index where key is or would be inserted, and whether it is present.
func (s *mvccStore) search(key []byte) (int, bool) {
	i := sort.Search(len(s.entries), func(i int) bool {
		return bytes.Compare(s.entries[i].key, key) >= 0
	})
	if i < len(s.entries) && bytes.Equal(s.entries[i].key, key) {
		return i, true
	}
	return i, false
}

// valueAt returns the value of key visible at readVersion — the newest version whose commit
// version is <= readVersion — or nil if the key is absent or tombstoned as of that version.
// The returned slice is the stored slice; callers must not mutate it.
func (s *mvccStore) valueAt(key []byte, readVersion int64) []byte {
	i, ok := s.search(key)
	if !ok {
		return nil
	}
	h := s.entries[i].history
	for j := len(h) - 1; j >= 0; j-- {
		if h[j].version <= readVersion {
			return h[j].value // may be nil (tombstone)
		}
	}
	return nil
}

// rangeAtLimited is rangeAt bounded by demand: it returns at most `want` live rows taken
// from the `reverse` end of [begin, end), always sorted ascending, together with the
// sub-window those rows fully account for and whether the whole range was consumed.
// want <= 0 is unbounded and reproduces rangeAt exactly.
//
// The sub-window is what makes a bounded read safe to merge: rows were taken from one end,
// so [coveredBegin, coveredEnd) is precisely the span in which this result is COMPLETE, and
// a caller may fold in mutations restricted to that span and get the same answer it would
// have got from the whole range. Outside it nothing is known and nothing may be assumed.
//
// rowsTouched counts store entries examined, including tombstoned ones — the instrument for
// "a page costs the page, not the tail".
func (s *mvccStore) rangeAtLimited(
	begin, end []byte, readVersion int64, want int, reverse bool,
) (out []fdb.KeyValue, coveredBegin, coveredEnd []byte, exhausted bool, rowsTouched int) {
	if bytes.Compare(begin, end) >= 0 {
		return nil, begin, end, true, 0
	}
	if reverse {
		hi := sort.Search(len(s.entries), func(i int) bool {
			return bytes.Compare(s.entries[i].key, end) >= 0
		})
		for i := hi - 1; i >= 0; i-- {
			e := s.entries[i]
			if bytes.Compare(e.key, begin) < 0 {
				break
			}
			rowsTouched++
			if v := s.valueAtEntry(e, readVersion); v != nil {
				out = append(out, fdb.KeyValue{
					Key: append(fdb.Key(nil), e.key...),
					// cloneVal, not append(nil, v...): appending nothing to a nil slice yields
					// nil, which is this package's spelling of "absent" — so a present,
					// zero-length value (what Set(k, nil) and Set(k, []byte{}) both write) would
					// come back out of the store looking like a tombstone. The v != nil gate
					// above lets it through precisely because it IS present; the copy must not
					// undo that. Same on the forward arm below, and in rangeAt.
					Value: cloneVal(v),
				})
				if want > 0 && len(out) >= want {
					// Complete from this key up to end; below it, unknown.
					lowest := append([]byte(nil), e.key...)
					reverseKVs(out)
					return out, lowest, end, false, rowsTouched
				}
			}
		}
		reverseKVs(out)
		return out, begin, end, true, rowsTouched
	}
	lo, _ := s.search(begin)
	for i := lo; i < len(s.entries); i++ {
		e := s.entries[i]
		if bytes.Compare(e.key, end) >= 0 {
			break
		}
		rowsTouched++
		if v := s.valueAtEntry(e, readVersion); v != nil {
			out = append(out, fdb.KeyValue{
				Key:   append(fdb.Key(nil), e.key...),
				Value: cloneVal(v),
			})
			if want > 0 && len(out) >= want {
				// Complete from begin through this key inclusive; above it, unknown.
				return out, begin, append(append([]byte(nil), e.key...), 0), false, rowsTouched
			}
		}
	}
	return out, begin, end, true, rowsTouched
}

// rangeAt returns the key/value pairs in [begin, end) visible at readVersion, sorted
// ascending by key, tombstones and absent keys skipped. Callers apply reverse/limit.
func (s *mvccStore) rangeAt(begin, end []byte, readVersion int64) []fdb.KeyValue {
	var out []fdb.KeyValue
	lo, _ := s.search(begin)
	for i := lo; i < len(s.entries); i++ {
		e := s.entries[i]
		if bytes.Compare(e.key, end) >= 0 {
			break
		}
		if v := s.valueAtEntry(e, readVersion); v != nil {
			out = append(out, fdb.KeyValue{
				Key: append(fdb.Key(nil), e.key...),
				// cloneVal, not append(nil, v...): appending nothing to a nil slice yields nil,
				// which is this package's spelling of "absent" — so a present, zero-length value
				// (what Set(k, nil) and Set(k, []byte{}) both write) would come back out of the
				// store looking like a tombstone.
				Value: cloneVal(v),
			})
		}
	}
	return out
}

// valueAtEntry is valueAt for an entry already located.
func (s *mvccStore) valueAtEntry(e *storeEntry, readVersion int64) []byte {
	for j := len(e.history) - 1; j >= 0; j-- {
		if e.history[j].version <= readVersion {
			return e.history[j].value
		}
	}
	return nil
}

// put appends a new committed version for key at commitVersion (value nil = tombstone),
// inserting a new sorted entry if the key is new. commitVersion must exceed any existing
// version for the key (guaranteed by the monotonic commit counter).
func (s *mvccStore) put(key, value []byte, commitVersion int64) {
	i, ok := s.search(key)
	vv := versionedValue{version: commitVersion, value: value}
	if ok {
		s.entries[i].history = append(s.entries[i].history, vv)
		return
	}
	e := &storeEntry{key: append([]byte(nil), key...), history: []versionedValue{vv}}
	s.entries = append(s.entries, nil)
	copy(s.entries[i+1:], s.entries[i:])
	s.entries[i] = e
}

// clearRange writes tombstones at commitVersion for every currently-live key in [begin, end).
// A tombstone on an already-absent key is unnecessary, so only live keys are tombstoned.
func (s *mvccStore) clearRange(begin, end []byte, commitVersion int64) {
	lo, _ := s.search(begin)
	for i := lo; i < len(s.entries); i++ {
		e := s.entries[i]
		if bytes.Compare(e.key, end) >= 0 {
			break
		}
		if s.valueAtEntry(e, commitVersion) != nil {
			e.history = append(e.history, versionedValue{version: commitVersion, value: nil})
		}
	}
}
