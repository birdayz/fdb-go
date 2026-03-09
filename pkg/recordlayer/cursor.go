package recordlayer

import (
	"context"
	"iter"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// NoNextReason indicates why a cursor stopped producing records
type NoNextReason int

const (
	// SourceExhausted means the cursor reached the end of its data
	SourceExhausted NoNextReason = iota
	// ReturnLimitReached means the cursor hit its row limit
	ReturnLimitReached
	// ByteLimitReached means the cursor hit its byte scan limit
	ByteLimitReached
	// TimeLimitReached means the cursor hit its time limit
	TimeLimitReached
	// ScanLimitReached means the cursor hit its key-value scan limit
	ScanLimitReached
)

// IsOutOfBand returns true if this reason represents an out-of-band completion
// (not solely dependent on the records returned)
func (r NoNextReason) IsOutOfBand() bool {
	return r != SourceExhausted && r != ReturnLimitReached
}

// IsSourceExhausted returns true if there is no more data available
func (r NoNextReason) IsSourceExhausted() bool {
	return r == SourceExhausted
}

// IsLimitReached returns true if the cursor stopped due to any kind of limit
func (r NoNextReason) IsLimitReached() bool {
	return r != SourceExhausted
}

// RecordCursorContinuation represents the position of a cursor for resumption
type RecordCursorContinuation interface {
	// ToBytes serializes this continuation to a byte array
	// Returns nil if this is an end continuation
	ToBytes() []byte
	
	// IsEnd returns true if this represents the end of iteration
	IsEnd() bool
}

// BytesContinuation is a simple continuation that wraps a byte array
type BytesContinuation struct {
	bytes []byte
}

// ToBytes returns the continuation bytes
func (c *BytesContinuation) ToBytes() []byte {
	return c.bytes
}

// IsEnd returns true if this is an end continuation
func (c *BytesContinuation) IsEnd() bool {
	return c.bytes == nil
}

// EndContinuation represents the end of a cursor's iteration
type EndContinuation struct{}

// ToBytes always returns nil for end continuations
func (c *EndContinuation) ToBytes() []byte {
	return nil
}

// IsEnd always returns true for end continuations
func (c *EndContinuation) IsEnd() bool {
	return true
}

// RecordCursorResult represents the result of a cursor's OnNext() call
type RecordCursorResult[T any] struct {
	value        *T
	continuation RecordCursorContinuation
	noNextReason NoNextReason
	hasNext      bool
}

// NewResultWithValue creates a result with a value
func NewResultWithValue[T any](value T, continuation RecordCursorContinuation) RecordCursorResult[T] {
	return RecordCursorResult[T]{
		value:        &value,
		continuation: continuation,
		hasNext:      true,
	}
}

// NewResultNoNext creates a result indicating no more records
func NewResultNoNext[T any](reason NoNextReason, continuation RecordCursorContinuation) RecordCursorResult[T] {
	return RecordCursorResult[T]{
		continuation: continuation,
		noNextReason: reason,
		hasNext:      false,
	}
}

// HasNext returns true if this result contains a value
func (r RecordCursorResult[T]) HasNext() bool {
	return r.hasNext
}

// GetValue returns the value. Panics if HasNext() is false — callers must check HasNext() first.
// This matches Java's behavior of throwing IllegalResultValueAccessException.
func (r RecordCursorResult[T]) GetValue() T {
	if !r.hasNext {
		panic("GetValue called on RecordCursorResult with no value (check HasNext() first)")
	}
	return *r.value
}

// GetContinuation returns the continuation for resuming the cursor
func (r RecordCursorResult[T]) GetContinuation() RecordCursorContinuation {
	return r.continuation
}

// GetNoNextReason returns the reason why there's no next record (valid when HasNext is false)
func (r RecordCursorResult[T]) GetNoNextReason() NoNextReason {
	return r.noNextReason
}

// HasStoppedBeforeEnd returns true if the cursor stopped before exhausting all records.
// This means the cursor can be resumed with the continuation to get more results.
// Matches Java's RecordCursorResult.hasStoppedBeforeEnd().
func (r RecordCursorResult[T]) HasStoppedBeforeEnd() bool {
	return !r.hasNext && !r.noNextReason.IsSourceExhausted()
}

// RecordCursor is a generic async iterator over records
type RecordCursor[T any] interface {
	// OnNext asynchronously returns the next result from this cursor
	OnNext(ctx context.Context) (RecordCursorResult[T], error)
	
	// Close releases any resources held by this cursor
	Close() error
	
	// Seq returns an iterator sequence over values only
	Seq(ctx context.Context) iter.Seq[T]
	
	// Seq2 returns an iterator sequence over (value, error) pairs
	Seq2(ctx context.Context) iter.Seq2[T, error]
	
	// SeqWithContinuation returns an iterator sequence over (value, continuation) pairs
	SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation]
}

// emptyCursor is a cursor that immediately returns no results.
// Matches Java's RecordCursor.empty().
type emptyCursor[T any] struct{}

// Empty returns a cursor that produces no results (source exhausted immediately).
func Empty[T any]() RecordCursor[T] {
	return &emptyCursor[T]{}
}

func (c *emptyCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
}

func (c *emptyCursor[T]) Close() error { return nil }

func (c *emptyCursor[T]) Seq(_ context.Context) iter.Seq[T] {
	return func(func(T) bool) {}
}

func (c *emptyCursor[T]) Seq2(_ context.Context) iter.Seq2[T, error] {
	return func(func(T, error) bool) {}
}

func (c *emptyCursor[T]) SeqWithContinuation(_ context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(func(T, RecordCursorContinuation) bool) {}
}

// errorCursor is a cursor that immediately returns an error on every OnNext call.
// Used when a cursor cannot be created (e.g., scanning a non-readable index).
type errorCursor[T any] struct {
	err error
}

func (c *errorCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	return RecordCursorResult[T]{}, c.err
}

func (c *errorCursor[T]) Close() error { return nil }

func (c *errorCursor[T]) Seq(ctx context.Context) iter.Seq[T] {
	return func(func(T) bool) {}
}

func (c *errorCursor[T]) Seq2(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		yield(zero, c.err)
	}
}

func (c *errorCursor[T]) SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(func(T, RecordCursorContinuation) bool) {}
}

// listCursor wraps a slice as a cursor. Matches Java's RecordCursor.fromList().
type listCursor[T any] struct {
	items  []T
	pos    int
	closed bool
}

// FromList creates a cursor from a slice. Matches Java's RecordCursor.fromList().
func FromList[T any](items []T) RecordCursor[T] {
	return &listCursor[T]{items: items}
}

func (c *listCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	if c.closed || c.pos >= len(c.items) {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}
	value := c.items[c.pos]
	c.pos++
	return NewResultWithValue(value, &BytesContinuation{}), nil
}

func (c *listCursor[T]) Close() error {
	c.closed = true
	return nil
}

func (c *listCursor[T]) Seq(ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *listCursor[T]) Seq2(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(*new(T), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *listCursor[T]) SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(yield func(T, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// ForEach applies a function to each record in the cursor
func ForEach[T any](ctx context.Context, cursor RecordCursor[T], fn func(T) error) error {
	defer func() { _ = cursor.Close() }()
	
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return err
		}
		
		if !result.HasNext() {
			return nil
		}
		
		if err := fn(result.GetValue()); err != nil {
			return err
		}
	}
}

// AsList collects all records from the cursor into a slice
func AsList[T any](ctx context.Context, cursor RecordCursor[T]) ([]T, error) {
	var results []T
	err := ForEach(ctx, cursor, func(record T) error {
		results = append(results, record)
		return nil
	})
	return results, err
}

// Sequence transformation functions that work with iter.Seq/Seq2
// These are more idiomatic Go than cursor-specific transformations

// Filter returns a filtered sequence
func Filter[T any](seq iter.Seq[T], predicate func(T) bool) iter.Seq[T] {
	return func(yield func(T) bool) {
		for value := range seq {
			if predicate(value) {
				if !yield(value) {
					return
				}
			}
		}
	}
}

// Map transforms values in a sequence
func Map[T, R any](seq iter.Seq[T], fn func(T) R) iter.Seq[R] {
	return func(yield func(R) bool) {
		for value := range seq {
			if !yield(fn(value)) {
				return
			}
		}
	}
}

// MapErr transforms values in a sequence with error handling
func MapErr[T, R any](seq iter.Seq2[T, error], fn func(T) (R, error)) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		for value, err := range seq {
			if err != nil {
				if !yield(*new(R), err) {
					return
				}
				continue
			}
			
			mapped, mappingErr := fn(value)
			if !yield(mapped, mappingErr) {
				return
			}
		}
	}
}

// Filter2 filters a Seq2 sequence
func Filter2[T any](seq iter.Seq2[T, error], predicate func(T) bool) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for value, err := range seq {
			if err != nil {
				if !yield(*new(T), err) {
					return
				}
				continue
			}
			
			if predicate(value) {
				if !yield(value, nil) {
					return
				}
			}
		}
	}
}

// Limit returns at most n values from a sequence
func Limit[T any](seq iter.Seq[T], n int) iter.Seq[T] {
	return func(yield func(T) bool) {
		count := 0
		for value := range seq {
			if count >= n {
				return
			}
			if !yield(value) {
				return
			}
			count++
		}
	}
}

// filterCursor wraps another cursor and filters records by a predicate.
// Filtered records are skipped silently; continuations are forwarded from the inner cursor.
type filterCursor[T any] struct {
	inner     RecordCursor[T]
	predicate func(T) bool
}

func (c *filterCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	for {
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			return result, nil
		}
		if c.predicate(result.GetValue()) {
			return result, nil
		}
		// Skip this record, keep iterating
	}
}

func (c *filterCursor[T]) Close() error {
	return c.inner.Close()
}

func (c *filterCursor[T]) Seq(ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *filterCursor[T]) Seq2(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(*new(T), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *filterCursor[T]) SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(yield func(T, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// RecordCursorProto is a convenience type for cursors over protobuf messages
type RecordCursorProto = RecordCursor[*FDBStoredRecord[proto.Message]]

// TypedRecordCursor is a convenience type for typed record cursors
type TypedRecordCursor[T proto.Message] RecordCursor[*FDBStoredRecord[T]]

// First returns the first element from a cursor, or nil if empty.
// Matches Java's RecordCursor.first().
func First[T any](ctx context.Context, cursor RecordCursor[T]) (*T, error) {
	defer func() { _ = cursor.Close() }()
	result, err := cursor.OnNext(ctx)
	if err != nil {
		return nil, err
	}
	if !result.HasNext() {
		return nil, nil
	}
	v := result.GetValue()
	return &v, nil
}

// GetCount returns the number of elements in a cursor by consuming it.
// Matches Java's RecordCursor.getCount().
func GetCount[T any](ctx context.Context, cursor RecordCursor[T]) (int, error) {
	defer func() { _ = cursor.Close() }()
	count := 0
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return count, err
		}
		if !result.HasNext() {
			return count, nil
		}
		count++
	}
}

// Reduce folds all cursor values into a single result using the given function.
// Matches Java's RecordCursor.reduce().
func Reduce[T any, R any](ctx context.Context, cursor RecordCursor[T], initial R, fn func(R, T) R) (R, error) {
	defer func() { _ = cursor.Close() }()
	acc := initial
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return acc, err
		}
		if !result.HasNext() {
			return acc, nil
		}
		acc = fn(acc, result.GetValue())
	}
}

// SkipCursor wraps a cursor and skips the first n elements.
// Matches Java's RecordCursor.skip().
func SkipCursor[T any](cursor RecordCursor[T], n int) RecordCursor[T] {
	if n <= 0 {
		return cursor
	}
	return &skipCursor[T]{inner: cursor, remaining: n}
}

type skipCursor[T any] struct {
	inner     RecordCursor[T]
	remaining int
}

func (c *skipCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	for c.remaining > 0 {
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			return result, nil
		}
		c.remaining--
	}
	return c.inner.OnNext(ctx)
}

func (c *skipCursor[T]) Close() error { return c.inner.Close() }

func (c *skipCursor[T]) Seq(ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *skipCursor[T]) Seq2(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(*new(T), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *skipCursor[T]) SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(yield func(T, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// LimitRowsCursor wraps a cursor and limits to at most n elements.
// Matches Java's RecordCursor.limitRowsTo().
func LimitRowsCursor[T any](cursor RecordCursor[T], n int) RecordCursor[T] {
	if n <= 0 {
		return Empty[T]()
	}
	return &limitRowsCursor[T]{inner: cursor, remaining: n}
}

type limitRowsCursor[T any] struct {
	inner     RecordCursor[T]
	remaining int
}

func (c *limitRowsCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.remaining <= 0 {
		return NewResultNoNext[T](ReturnLimitReached, &EndContinuation{}), nil
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return result, err
	}
	if result.HasNext() {
		c.remaining--
	}
	return result, nil
}

func (c *limitRowsCursor[T]) Close() error { return c.inner.Close() }

func (c *limitRowsCursor[T]) Seq(ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *limitRowsCursor[T]) Seq2(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(*new(T), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *limitRowsCursor[T]) SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(yield func(T, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// ConcatCursor concatenates two cursors: returns all results from the first cursor,
// then all results from the second. Matches Java's ConcatCursor.
//
// Uses cursor factories (not raw cursors) so that on continuation resumption,
// only the relevant cursor is created. Continuation tokens are proto-wrapped
// with ConcatContinuation for wire compatibility with Java.
type concatCursor[T any] struct {
	firstFactory  CursorFactory[T]
	secondFactory CursorFactory[T]
	current       RecordCursor[T]
	onSecond      bool
	closed        bool
}

// CursorFactory creates a cursor from an optional continuation.
// nil continuation means start from the beginning.
type CursorFactory[T any] func(continuation []byte) RecordCursor[T]

// ConcatCursors concatenates two cursor factories: results from first, then second.
// Continuation tokens are wire-compatible with Java's ConcatCursor.
func ConcatCursors[T any](first, second CursorFactory[T], continuation []byte) RecordCursor[T] {
	c := &concatCursor[T]{
		firstFactory:  first,
		secondFactory: second,
	}

	if len(continuation) > 0 {
		var cont gen.ConcatContinuation
		if err := proto.Unmarshal(continuation, &cont); err == nil {
			if cont.GetSecond() {
				c.onSecond = true
				c.current = second(cont.GetContinuation())
			} else {
				c.current = first(cont.GetContinuation())
			}
		} else {
			// Invalid continuation — start fresh
			c.current = first(nil)
		}
	} else {
		c.current = first(nil)
	}

	return c
}

func (c *concatCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.closed {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	result, err := c.current.OnNext(ctx)
	if err != nil {
		return result, err
	}

	if result.HasNext() {
		// Wrap the inner continuation with ConcatContinuation
		innerCont := result.GetContinuation()
		wrapped := c.wrapContinuation(innerCont)
		return NewResultWithValue(result.GetValue(), wrapped), nil
	}

	// Current cursor exhausted
	if !c.onSecond && result.GetNoNextReason() == SourceExhausted {
		// First cursor done — switch to second
		_ = c.current.Close()
		c.onSecond = true
		c.current = c.secondFactory(nil)
		return c.OnNext(ctx)
	}

	// Second cursor done or first stopped for non-exhaustion reason
	innerCont := result.GetContinuation()
	wrapped := c.wrapContinuation(innerCont)
	return NewResultNoNext[T](result.GetNoNextReason(), wrapped), nil
}

func (c *concatCursor[T]) wrapContinuation(inner RecordCursorContinuation) RecordCursorContinuation {
	if inner == nil || inner.IsEnd() {
		if c.onSecond {
			return &EndContinuation{}
		}
		// First cursor at end but haven't switched yet
		return &concatContinuationWrapper{onSecond: false, inner: nil}
	}
	return &concatContinuationWrapper{onSecond: c.onSecond, inner: inner.ToBytes()}
}

type concatContinuationWrapper struct {
	onSecond bool
	inner    []byte
}

func (c *concatContinuationWrapper) ToBytes() []byte {
	cont := &gen.ConcatContinuation{
		Second:       proto.Bool(c.onSecond),
		Continuation: c.inner,
	}
	data, _ := proto.Marshal(cont)
	return data
}

func (c *concatContinuationWrapper) IsEnd() bool {
	return false
}

func (c *concatCursor[T]) Close() error {
	c.closed = true
	if c.current != nil {
		return c.current.Close()
	}
	return nil
}

func (c *concatCursor[T]) Seq(ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *concatCursor[T]) Seq2(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(*new(T), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *concatCursor[T]) SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(yield func(T, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// MapResultCursor applies a transformation function to each cursor result.
// Unlike Map (which operates on iter.Seq), this operates at the RecordCursor level
// and preserves continuations. Matches Java's MapResultCursor.
type mapResultCursor[T, R any] struct {
	inner RecordCursor[T]
	fn    func(T) R
}

// MapCursor creates a cursor that transforms each value using the given function.
// Continuations from the inner cursor are passed through transparently.
func MapCursor[T, R any](cursor RecordCursor[T], fn func(T) R) RecordCursor[R] {
	return &mapResultCursor[T, R]{inner: cursor, fn: fn}
}

func (c *mapResultCursor[T, R]) OnNext(ctx context.Context) (RecordCursorResult[R], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return RecordCursorResult[R]{}, err
	}
	if !result.HasNext() {
		return NewResultNoNext[R](result.GetNoNextReason(), result.GetContinuation()), nil
	}
	mapped := c.fn(result.GetValue())
	return NewResultWithValue(mapped, result.GetContinuation()), nil
}

func (c *mapResultCursor[T, R]) Close() error { return c.inner.Close() }

func (c *mapResultCursor[T, R]) Seq(ctx context.Context) iter.Seq[R] {
	return func(yield func(R) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *mapResultCursor[T, R]) Seq2(ctx context.Context) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(*new(R), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *mapResultCursor[T, R]) SeqWithContinuation(ctx context.Context) iter.Seq2[R, RecordCursorContinuation] {
	return func(yield func(R, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// flatMapCursor takes an outer cursor and, for each outer value, creates
// an inner cursor via a function, flattening all inner results into a single stream.
// Matches Java's FlatMapPipelinedCursor. The pipelineSize parameter is accepted for
// API compatibility but Go's synchronous FDB bindings process one outer value at a time.
//
// The continuation format uses FlatMapContinuation proto for wire compatibility with Java:
//   - outer_continuation: position in the outer cursor
//   - inner_continuation: position in the current inner cursor (if stopped mid-inner)
//   - check_value: optional outer identity check for safe resumption
type flatMapCursor[T, V any] struct {
	innerFactory   func(T, []byte) RecordCursor[V]
	checkValueFunc func(T) []byte
	outer          RecordCursor[T]
	inner          RecordCursor[V]
	outerValue     *T                       // current outer value
	priorOuterCont RecordCursorContinuation // outer continuation BEFORE current outer value
	outerCont      RecordCursorContinuation // outer continuation AFTER current outer value
	pendingInner   []byte                   // inner continuation for resume (nil = none pending)
	pendingCheck   []byte                   // check value for resume validation
	hasPending     bool                     // true if we're resuming with inner continuation
	outerExhausted bool
	closed         bool
}

// FlatMapPipelined creates a cursor that flat-maps an outer cursor through an inner cursor factory.
// Matches Java's RecordCursor.flatMapPipelined().
//
// Parameters:
//   - outerFactory: creates the outer cursor from a continuation
//   - innerFactory: given an outer value and optional inner continuation, creates an inner cursor
//   - continuation: serialized FlatMapContinuation for resumption (nil to start fresh)
//   - pipelineSize: accepted for API compat (Go processes sequentially)
func FlatMapPipelined[T, V any](
	outerFactory CursorFactory[T],
	innerFactory func(T, []byte) RecordCursor[V],
	continuation []byte,
	pipelineSize int,
) RecordCursor[V] {
	return FlatMapPipelinedWithCheck(outerFactory, innerFactory, nil, continuation, pipelineSize)
}

// FlatMapPipelinedWithCheck is like FlatMapPipelined but with an optional check value function.
// The check value validates that the outer record hasn't changed between transactions when
// resuming from a continuation mid-inner-cursor.
func FlatMapPipelinedWithCheck[T, V any](
	outerFactory CursorFactory[T],
	innerFactory func(T, []byte) RecordCursor[V],
	checkValueFunc func(T) []byte,
	continuation []byte,
	pipelineSize int,
) RecordCursor[V] {
	c := &flatMapCursor[T, V]{
		innerFactory:   innerFactory,
		checkValueFunc: checkValueFunc,
	}

	if len(continuation) > 0 {
		var cont gen.FlatMapContinuation
		if err := proto.Unmarshal(continuation, &cont); err == nil {
			c.outer = outerFactory(cont.GetOuterContinuation())
			if cont.InnerContinuation != nil {
				// Resuming mid-inner — need to advance outer once, then create inner with continuation
				c.hasPending = true
				c.pendingInner = cont.InnerContinuation
				c.pendingCheck = cont.CheckValue
			}
		} else {
			c.outer = outerFactory(nil)
		}
	} else {
		c.outer = outerFactory(nil)
	}

	return c
}

func (c *flatMapCursor[T, V]) OnNext(ctx context.Context) (RecordCursorResult[V], error) {
	if c.closed {
		return NewResultNoNext[V](SourceExhausted, &EndContinuation{}), nil
	}

	for {
		// If we have an active inner cursor, try to get from it
		if c.inner != nil {
			result, err := c.inner.OnNext(ctx)
			if err != nil {
				return RecordCursorResult[V]{}, err
			}

			if result.HasNext() {
				wrapped := c.wrapContinuation(result.GetContinuation())
				return NewResultWithValue(result.GetValue(), wrapped), nil
			}

			// Inner cursor stopped
			if result.GetNoNextReason().IsSourceExhausted() {
				_ = c.inner.Close()
				c.inner = nil
				continue
			}

			// Inner stopped for non-exhaustion reason (limit, time, etc.)
			wrapped := c.wrapContinuation(result.GetContinuation())
			return NewResultNoNext[V](result.GetNoNextReason(), wrapped), nil
		}

		// No inner cursor — advance outer
		if c.outerExhausted {
			return NewResultNoNext[V](SourceExhausted, &EndContinuation{}), nil
		}

		outerResult, err := c.outer.OnNext(ctx)
		if err != nil {
			return RecordCursorResult[V]{}, err
		}

		if !outerResult.HasNext() {
			c.outerExhausted = true
			if outerResult.GetNoNextReason().IsSourceExhausted() {
				return NewResultNoNext[V](SourceExhausted, &EndContinuation{}), nil
			}
			// Outer stopped for non-exhaustion reason
			wrapped := c.wrapOuterStopContinuation(outerResult.GetContinuation())
			return NewResultNoNext[V](outerResult.GetNoNextReason(), wrapped), nil
		}

		// Got an outer value — track continuation state
		c.priorOuterCont = c.outerCont // continuation before this outer value
		c.outerCont = outerResult.GetContinuation()
		v := outerResult.GetValue()
		c.outerValue = &v

		// Check if we're resuming with a pending inner continuation
		var innerCont []byte
		if c.hasPending {
			c.hasPending = false
			// Validate check value if provided
			if c.checkValueFunc != nil && c.pendingCheck != nil {
				currentCheck := c.checkValueFunc(v)
				if !bytesEqual(currentCheck, c.pendingCheck) {
					// Outer record changed — restart inner from beginning
					innerCont = nil
				} else {
					innerCont = c.pendingInner
				}
			} else {
				innerCont = c.pendingInner
			}
			c.pendingInner = nil
			c.pendingCheck = nil
		}

		c.inner = c.innerFactory(v, innerCont)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *flatMapCursor[T, V]) wrapContinuation(innerCont RecordCursorContinuation) RecordCursorContinuation {
	return &flatMapContinuationWrapper[T]{
		priorOuterCont: c.priorOuterCont,
		outerCont:      c.outerCont,
		outerValue:     c.outerValue,
		innerCont:      innerCont,
		checkValueFunc: c.checkValueFunc,
	}
}

func (c *flatMapCursor[T, V]) wrapOuterStopContinuation(outerCont RecordCursorContinuation) RecordCursorContinuation {
	fm := &gen.FlatMapContinuation{
		OuterContinuation: outerCont.ToBytes(),
	}
	data, _ := proto.Marshal(fm)
	return &BytesContinuation{bytes: data}
}

type flatMapContinuationWrapper[T any] struct {
	priorOuterCont RecordCursorContinuation
	outerCont      RecordCursorContinuation
	outerValue     *T
	innerCont      RecordCursorContinuation
	checkValueFunc func(T) []byte
}

func (w *flatMapContinuationWrapper[T]) ToBytes() []byte {
	fm := &gen.FlatMapContinuation{}

	if w.innerCont == nil || w.innerCont.IsEnd() {
		// Inner cursor exhausted — resume from outer's current position (after this value)
		if w.outerCont != nil {
			fm.OuterContinuation = w.outerCont.ToBytes()
		}
	} else {
		// Inner cursor NOT exhausted — resume from prior outer position + inner position
		if w.priorOuterCont != nil {
			fm.OuterContinuation = w.priorOuterCont.ToBytes()
		}
		fm.InnerContinuation = w.innerCont.ToBytes()
		if w.checkValueFunc != nil && w.outerValue != nil {
			fm.CheckValue = w.checkValueFunc(*w.outerValue)
		}
	}

	data, _ := proto.Marshal(fm)
	return data
}

func (w *flatMapContinuationWrapper[T]) IsEnd() bool {
	return false
}

func (c *flatMapCursor[T, V]) Close() error {
	c.closed = true
	var firstErr error
	if c.inner != nil {
		if err := c.inner.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.outer != nil {
		if err := c.outer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *flatMapCursor[T, V]) Seq(ctx context.Context) iter.Seq[V] {
	return func(yield func(V) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *flatMapCursor[T, V]) Seq2(ctx context.Context) iter.Seq2[V, error] {
	return func(yield func(V, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(*new(V), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *flatMapCursor[T, V]) SeqWithContinuation(ctx context.Context) iter.Seq2[V, RecordCursorContinuation] {
	return func(yield func(V, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// autoContinuingCursor wraps a cursor generator and automatically creates new
// transactions when the inner cursor stops due to limits (time, scan, byte, row).
// This enables seamless scanning of large datasets across FDB's 5-second transaction
// boundary. Matches Java's AutoContinuingCursor.
//
// Unlike other cursors, AutoContinuingCursor manages its own transaction lifecycle —
// it is NOT used within a transaction, but spans across multiple transactions.
type autoContinuingCursor[T any] struct {
	runner     *FDBDatabaseRunner
	generator  func(*FDBRecordContext, []byte) RecordCursor[T]
	maxRetries int

	currentCtx    *FDBRecordContext
	currentCursor RecordCursor[T]
	lastResult    *RecordCursorResult[T]
	closed        bool
}

// NewAutoContinuingCursor creates a cursor that automatically creates new transactions
// when the inner cursor stops before exhaustion (e.g., time limit, scan limit).
// Matches Java's AutoContinuingCursor.
//
// Parameters:
//   - runner: provides transaction creation and context configuration
//   - generator: creates a cursor within a transaction context from a continuation
//   - maxRetries: maximum retries on transient FDB errors (0 = no retries)
func NewAutoContinuingCursor[T any](
	runner *FDBDatabaseRunner,
	generator func(*FDBRecordContext, []byte) RecordCursor[T],
	maxRetries int,
) RecordCursor[T] {
	return &autoContinuingCursor[T]{
		runner:     runner,
		generator:  generator,
		maxRetries: maxRetries,
	}
}

func (c *autoContinuingCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.closed {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}

	for {
		result, err := c.onNextWithRetry(ctx, 0)
		if err != nil {
			return RecordCursorResult[T]{}, err
		}

		if result.HasStoppedBeforeEnd() {
			// Inner cursor stopped due to a limit — create new transaction and continue
			contBytes := result.GetContinuation().ToBytes()
			if err := c.openContextAndGenerateCursor(ctx, contBytes); err != nil {
				return RecordCursorResult[T]{}, err
			}
			continue
		}

		// Either has a value or source is exhausted
		if result.HasNext() {
			c.lastResult = &result
		}
		return result, nil
	}
}

func (c *autoContinuingCursor[T]) onNextWithRetry(ctx context.Context, attempt int) (RecordCursorResult[T], error) {
	if c.currentCursor == nil {
		if err := c.openContextAndGenerateCursor(ctx, c.lastContinuation()); err != nil {
			return RecordCursorResult[T]{}, err
		}
	}

	result, err := c.currentCursor.OnNext(ctx)
	if err != nil {
		if !isRetryableError(err) || attempt >= c.maxRetries {
			return RecordCursorResult[T]{}, err
		}
		// Retryable error — create new cursor from last successful position
		if openErr := c.openContextAndGenerateCursor(ctx, c.lastContinuation()); openErr != nil {
			return RecordCursorResult[T]{}, openErr
		}
		return c.onNextWithRetry(ctx, attempt+1)
	}

	return result, nil
}

func (c *autoContinuingCursor[T]) lastContinuation() []byte {
	if c.lastResult == nil {
		return nil
	}
	return c.lastResult.GetContinuation().ToBytes()
}

func (c *autoContinuingCursor[T]) openContextAndGenerateCursor(ctx context.Context, continuation []byte) error {
	// Close previous cursor and context
	if c.currentCursor != nil {
		_ = c.currentCursor.Close()
		c.currentCursor = nil
	}
	if c.currentCtx != nil {
		c.currentCtx.Transaction().Cancel()
		c.currentCtx = nil
	}

	recordCtx, err := c.runner.OpenContext(ctx)
	if err != nil {
		return err
	}

	c.currentCtx = recordCtx
	c.currentCursor = c.generator(recordCtx, continuation)
	return nil
}

func (c *autoContinuingCursor[T]) Close() error {
	c.closed = true
	var firstErr error
	if c.currentCursor != nil {
		if err := c.currentCursor.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.currentCursor = nil
	}
	if c.currentCtx != nil {
		c.currentCtx.Transaction().Cancel()
		c.currentCtx = nil
	}
	return firstErr
}

func (c *autoContinuingCursor[T]) Seq(ctx context.Context) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *autoContinuingCursor[T]) Seq2(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(*new(T), err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *autoContinuingCursor[T]) SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation] {
	return func(yield func(T, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}

// Note: Most sequence utilities are available in Go 1.23+ standard library:
// - slices.Collect() for collecting sequences
// - Use range loops directly for counting, filtering, etc.
// - Many iterator utilities in the "iter" package
//
// We only provide Record Layer specific cursor transformations here.