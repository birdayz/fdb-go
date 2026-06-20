package executor

import (
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// RFC-130: statement-wide memory byte budget. Cardinality-growing buffers in
// this package are bounded by ROW COUNT (MaterializationLimit, default 100k)
// but not by BYTES, so 100k large rows can still OOM. The budget lives in a
// statement-scoped recordlayer.ExecuteState (mutable counter held by pointer
// inside the value-struct ExecuteProperties, shared statement-wide), and is
// charged at every cardinality-growing buffer through the accounted container
// types below. Accounting is a property of the container type, not a
// reviewer's vigilance: a buffer literally cannot be constructed without an
// *ExecuteState, so a missed wiring is a compile/test failure, not a silent
// nil-no-op (the state is always present — see recordlayer.NewExecuteState).

// boundedBuffer is an accounted append-only slice. Append charges the byte
// estimate of an item against the statement's ExecuteState BEFORE keeping it,
// and also enforces the existing MaterializationLimit row check, so a buffer
// can't exist without both bounds. The byte budget and the row count are
// charged on the SAME append, in that order: the byte budget is the
// statement-wide ceiling, the row count is the per-buffer backstop.
type boundedBuffer[T any] struct {
	items    []T
	st       *recordlayer.ExecuteState
	rowLimit int
	opName   string
	est      func(T) int64
}

// newBoundedBuffer constructs an accounted buffer. st is non-optional — the
// caller must supply the statement's always-present ExecuteState (RFC-130
// §2.3). rowLimit is the per-buffer MaterializationLimit (<=0 means no row
// cap); opName labels the buffer in the MaterializationLimitExceededError. est
// computes a row's approximate resident bytes — it is invoked ONLY when a
// budget is active (zero-overhead-when-off, RFC-130).
func newBoundedBuffer[T any](st *recordlayer.ExecuteState, rowLimit int, opName string, est func(T) int64) *boundedBuffer[T] {
	return &boundedBuffer[T]{st: st, rowLimit: rowLimit, opName: opName, est: est}
}

// Append charges the item's estimated bytes against the statement budget (only
// when a budget is active — the estimate is not computed otherwise), enforces
// the row cap, then appends. On either limit it returns the corresponding error
// and does NOT keep the item, so the buffer that would breach a bound is never
// materialized.
//
// The row-cap boundary matches the pre-RFC-130 CollectAllBounded exactly: it
// errors on the item that would make the buffer reach rowLimit rows (i.e. it
// holds at most rowLimit-1 rows successfully). rowLimit<=0 disables the row
// cap.
func (b *boundedBuffer[T]) Append(item T) error {
	if b.st.HasMemLimit() {
		if err := b.st.ChargeMemory(b.est(item)); err != nil {
			return err
		}
	}
	if b.rowLimit > 0 && len(b.items)+1 >= b.rowLimit {
		return &MaterializationLimitExceededError{Limit: b.rowLimit, Context: b.opName}
	}
	b.items = append(b.items, item)
	return nil
}

// Items returns the underlying slice. The buffer is append-only; callers must
// not mutate the returned slice's length expecting the buffer to track it.
func (b *boundedBuffer[T]) Items() []T {
	return b.items
}

// Len reports the number of buffered items.
func (b *boundedBuffer[T]) Len() int {
	return len(b.items)
}

// boundedSet is an accounted set keyed by K. Add charges the supplied byte
// estimate ONLY when the key is new (a duplicate add is free — it stores
// nothing new), and reports whether the key was newly inserted. A nil/zero
// budget makes the charge a no-op.
type boundedSet[K comparable] struct {
	m  map[K]struct{}
	st *recordlayer.ExecuteState
}

// newBoundedSet constructs an accounted set. st is non-optional (RFC-130
// §2.3).
func newBoundedSet[K comparable](st *recordlayer.ExecuteState) *boundedSet[K] {
	return &boundedSet[K]{m: make(map[K]struct{}), st: st}
}

// Contains reports whether key is present.
func (s *boundedSet[K]) Contains(key K) bool {
	_, ok := s.m[key]
	return ok
}

// Add inserts key, charging estBytes against the statement budget only when
// the key is NEW. Returns (added, err): added is false for a duplicate (no
// charge, no error). On a budget breach the key is NOT inserted and the error
// is returned.
func (s *boundedSet[K]) Add(key K, estBytes int64) (added bool, err error) {
	if _, ok := s.m[key]; ok {
		return false, nil
	}
	if err := s.st.ChargeMemory(estBytes); err != nil {
		return false, err
	}
	s.m[key] = struct{}{}
	return true, nil
}

// Len reports the number of distinct keys.
func (s *boundedSet[K]) Len() int {
	return len(s.m)
}

// estimateQueryResultBytes returns an approximate resident-byte estimate for a
// QueryResult, for charging against the statement memory budget (RFC-130). It
// is intentionally NOT exact heap size — a ceiling signal, not a measurement —
// and must never panic on any QueryResult shape (the relational-layer
// estimateRowBytes cannot be reused here: the executor cannot import the
// relational layer, query_result.go:18). Shapes:
//
//   - stored row (Record != nil): the proto wire size of the record plus the
//     length of the tuple-encoded primary key;
//   - computed row (Record == nil): the approximate size of the Datum — a
//     map[string]any (sum of key lengths + per-value estimate) or a scalar
//     value;
//   - empty row (Record == nil, Datum == nil): a small constant.
func estimateQueryResultBytes(qr QueryResult) int64 {
	if qr.Record != nil {
		var n int64
		// Record.Record is the proto message M; it can be nil even when the
		// FDBStoredRecord wrapper is not.
		if msg := qr.Record.Record; msg != nil {
			n += int64(proto.Size(msg))
		}
		n += int64(len(qr.PrimaryKey.Pack()))
		if n == 0 {
			return emptyRowBytes
		}
		return n
	}
	if qr.Datum == nil {
		return emptyRowBytes
	}
	return estimateDatumBytes(qr.Datum)
}

// emptyRowBytes is the small constant charged for a row that carries no
// payload (no stored record, no datum). A row is never free — even an empty
// QueryResult occupies a slot.
const emptyRowBytes int64 = 16

// estimateDatumBytes approximates the resident bytes of a computed-row datum.
// Handles the map[string]any rows the executor flows, slices, and scalar
// values; recurses one level for nested maps/slices and falls back to a fixed
// per-value cost for everything else. Never panics.
func estimateDatumBytes(d any) int64 {
	switch v := d.(type) {
	case nil:
		return 1
	case map[string]any:
		var n int64
		for k, val := range v {
			n += int64(len(k))
			n += scalarValueBytes(val)
		}
		if n == 0 {
			return emptyRowBytes
		}
		return n
	case []any:
		var n int64
		for _, e := range v {
			n += scalarValueBytes(e)
		}
		if n == 0 {
			return emptyRowBytes
		}
		return n
	default:
		return scalarValueBytes(d)
	}
}

// scalarValueBytes estimates the bytes of a single datum value. Strings and
// byte slices cost their length; nested maps/slices recurse one level via
// estimateDatumBytes; everything else (numbers, bools, time, proto messages,
// unknown types) costs a fixed estimate. Never panics.
func scalarValueBytes(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 1
	case string:
		return int64(len(x))
	case []byte:
		return int64(len(x))
	case map[string]any:
		return estimateDatumBytes(x)
	case []any:
		return estimateDatumBytes(x)
	default:
		return 8
	}
}
