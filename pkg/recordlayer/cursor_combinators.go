package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// filterCursor wraps another cursor and filters records by a predicate.
// Filtered records are skipped silently; continuations are forwarded from the inner cursor.
type filterCursor[T any] struct {
	inner     RecordCursor[T]
	predicate func(T) bool
}

func (c *filterCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[T]{}, err
		}
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

func (c *filterCursor[T]) IsClosed() bool { return c.inner.IsClosed() }

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
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[T]{}, err
		}
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

func (c *skipCursor[T]) IsClosed() bool { return c.inner.IsClosed() }

// LimitRowsCursor wraps a cursor and limits to at most n elements.
// Matches Java's RecordCursor.limitRowsTo().
func LimitRowsCursor[T any](cursor RecordCursor[T], n int) RecordCursor[T] {
	if n <= 0 {
		// Close inner cursor to prevent resource leaks (FDB iterators, etc.)
		_ = cursor.Close()
		return Empty[T]()
	}
	return &limitRowsCursor[T]{inner: cursor, remaining: n}
}

type limitRowsCursor[T any] struct {
	inner      RecordCursor[T]
	remaining  int
	lastResult *RecordCursorResult[T] // cached from last inner read
}

func (c *limitRowsCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	// Already returned a terminal result — return it again.
	// Matches Java's RowLimitedCursor early return for cached !hasNext results.
	if c.lastResult != nil && !c.lastResult.HasNext() {
		return *c.lastResult, nil
	}

	if c.remaining <= 0 {
		_ = c.inner.Close()
		// Determine correct reason: if inner already signaled SOURCE_EXHAUSTED
		// (EndContinuation), propagate that. Otherwise, it's a limit.
		// Matches Java's RowLimitedCursor logic.
		var reason NoNextReason
		var cont RecordCursorContinuation
		if c.lastResult != nil && !c.lastResult.HasNext() && c.lastResult.GetContinuation().IsEnd() {
			reason = c.lastResult.GetNoNextReason()
			cont = c.lastResult.GetContinuation()
		} else if c.lastResult != nil {
			reason = ReturnLimitReached
			cont = c.lastResult.GetContinuation()
		} else {
			reason = ReturnLimitReached
			cont = &StartContinuation{}
		}
		result := NewResultNoNext[T](reason, cont)
		c.lastResult = &result
		return result, nil
	}

	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return result, err
	}
	c.lastResult = &result
	if result.HasNext() {
		c.remaining--
	}
	return result, nil
}

func (c *limitRowsCursor[T]) Close() error { return c.inner.Close() }

func (c *limitRowsCursor[T]) IsClosed() bool { return c.inner.IsClosed() }

// SkipThenLimit is a convenience that skips n elements then limits to m.
// Matches Java's RecordCursor.skipThenLimit().
func SkipThenLimit[T any](cursor RecordCursor[T], skip, limit int) RecordCursor[T] {
	return LimitRowsCursor(SkipCursor(cursor, skip), limit)
}

// OrElse returns the primary cursor if it has results, otherwise falls back
// to the alternative cursor. Matches Java's RecordCursor.orElse().
func OrElse[T any](primary RecordCursor[T], alternative func() RecordCursor[T]) RecordCursor[T] {
	return OrElseWithContinuation(
		func(cont []byte) RecordCursor[T] { return primary },
		func(cont []byte) RecordCursor[T] { return alternative() },
		nil,
	)
}

// OrElseWithContinuation creates an OrElse cursor with continuation support.
// Matches Java's OrElseCursor with OrElseContinuation proto serialization.
// Both primary and alternative are cursor factories that accept continuation
// bytes for cross-transaction resume.
func OrElseWithContinuation[T any](
	primaryFactory CursorFactory[T],
	alternativeFactory CursorFactory[T],
	continuation []byte,
) RecordCursor[T] {
	c := &orElseCursor[T]{
		alternativeFactory: alternativeFactory,
		state:              gen.OrElseContinuation_UNDECIDED,
	}

	if len(continuation) > 0 {
		var cont gen.OrElseContinuation
		if err := cont.UnmarshalVT(continuation); err == nil && cont.State != nil {
			c.state = *cont.State
			switch c.state {
			case gen.OrElseContinuation_USE_INNER:
				c.primary = primaryFactory(cont.Continuation)
				c.active = c.primary
			case gen.OrElseContinuation_USE_OTHER:
				c.active = alternativeFactory(cont.Continuation)
			default:
				c.primary = primaryFactory(cont.Continuation)
			}
		} else {
			c.primary = primaryFactory(nil)
		}
	} else {
		c.primary = primaryFactory(nil)
	}

	return c
}

type orElseCursor[T any] struct {
	primary            RecordCursor[T]
	alternativeFactory CursorFactory[T]
	active             RecordCursor[T]
	state              gen.OrElseContinuation_State
}

func (c *orElseCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.state == gen.OrElseContinuation_UNDECIDED {
		result, err := c.primary.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if result.HasNext() {
			c.state = gen.OrElseContinuation_USE_INNER
			c.active = c.primary
			cont := c.wrapContinuation(gen.OrElseContinuation_USE_INNER, result.GetContinuation())
			return NewResultWithValue(result.GetValue(), cont), nil
		}
		if !result.GetNoNextReason().IsSourceExhausted() {
			cont := c.wrapContinuation(gen.OrElseContinuation_UNDECIDED, result.GetContinuation())
			return NewResultNoNext[T](result.GetNoNextReason(), cont), nil
		}
		c.state = gen.OrElseContinuation_USE_OTHER
		_ = c.primary.Close()
		c.active = c.alternativeFactory(nil)
		return c.advanceActive(ctx)
	}
	return c.advanceActive(ctx)
}

func (c *orElseCursor[T]) advanceActive(ctx context.Context) (RecordCursorResult[T], error) {
	result, err := c.active.OnNext(ctx)
	if err != nil {
		return result, err
	}
	if result.HasNext() {
		cont := c.wrapContinuation(c.state, result.GetContinuation())
		return NewResultWithValue(result.GetValue(), cont), nil
	}
	if result.GetContinuation().IsEnd() {
		return result, nil
	}
	cont := c.wrapContinuation(c.state, result.GetContinuation())
	return NewResultNoNext[T](result.GetNoNextReason(), cont), nil
}

func (c *orElseCursor[T]) wrapContinuation(state gen.OrElseContinuation_State, inner RecordCursorContinuation) RecordCursorContinuation {
	return &orElseContinuationWrapper{state: state, inner: inner}
}

func (c *orElseCursor[T]) Close() error {
	if c.active != nil {
		return c.active.Close()
	}
	if c.primary != nil {
		return c.primary.Close()
	}
	return nil
}

func (c *orElseCursor[T]) IsClosed() bool {
	if c.active != nil {
		return c.active.IsClosed()
	}
	if c.primary != nil {
		return c.primary.IsClosed()
	}
	return true
}

type orElseContinuationWrapper struct {
	state gen.OrElseContinuation_State
	inner RecordCursorContinuation
}

func (w *orElseContinuationWrapper) ToBytes() ([]byte, error) {
	if w.inner == nil || w.inner.IsEnd() {
		return nil, nil
	}
	innerBytes, err := w.inner.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("orelse continuation inner: %w", err)
	}
	if len(innerBytes) == 0 {
		return nil, nil
	}
	oc := &gen.OrElseContinuation{
		State:        w.state.Enum(),
		Continuation: innerBytes,
	}
	return oc.MarshalVT()
}

func (w *orElseContinuationWrapper) IsEnd() bool {
	return false
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

// ConcatCursors concatenates two cursor factories: results from first, then second.
// Continuation tokens are wire-compatible with Java's ConcatCursor.
func ConcatCursors[T any](first, second CursorFactory[T], continuation []byte) RecordCursor[T] {
	c := &concatCursor[T]{
		firstFactory:  first,
		secondFactory: second,
	}

	if len(continuation) > 0 {
		var cont gen.ConcatContinuation
		if err := cont.UnmarshalVT(continuation); err == nil {
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
		wrapped, wrapErr := c.wrapContinuation(innerCont)
		if wrapErr != nil {
			return RecordCursorResult[T]{}, wrapErr
		}
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
	wrapped, wrapErr := c.wrapContinuation(innerCont)
	if wrapErr != nil {
		return RecordCursorResult[T]{}, wrapErr
	}
	return NewResultNoNext[T](result.GetNoNextReason(), wrapped), nil
}

func (c *concatCursor[T]) wrapContinuation(inner RecordCursorContinuation) (RecordCursorContinuation, error) {
	if inner == nil || inner.IsEnd() {
		if c.onSecond {
			return &EndContinuation{}, nil
		}
		// First cursor at end but haven't switched yet
		return &concatContinuationWrapper{onSecond: false, inner: nil}, nil
	}
	innerBytes, err := inner.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("concat continuation: %w", err)
	}
	return &concatContinuationWrapper{onSecond: c.onSecond, inner: innerBytes}, nil
}

type concatContinuationWrapper struct {
	onSecond bool
	inner    []byte
}

func (c *concatContinuationWrapper) ToBytes() ([]byte, error) {
	cont := &gen.ConcatContinuation{
		Second:       proto.Bool(c.onSecond),
		Continuation: c.inner,
	}
	return cont.MarshalVT()
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

func (c *concatCursor[T]) IsClosed() bool { return c.closed }

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

func (c *mapResultCursor[T, R]) IsClosed() bool { return c.inner.IsClosed() }

// mapErrCursor wraps a cursor and transforms each value with a fallible function.
type mapErrCursor[T, R any] struct {
	inner RecordCursor[T]
	fn    func(T) (R, error)
}

// MapErrCursor creates a cursor that transforms each value using a function that
// can return an error. If the transform function returns an error, iteration stops
// with that error. Continuations from the inner cursor are passed through.
// Matches Java's MapResultCursor with checked exceptions.
func MapErrCursor[T, R any](cursor RecordCursor[T], fn func(T) (R, error)) RecordCursor[R] {
	return &mapErrCursor[T, R]{inner: cursor, fn: fn}
}

func (c *mapErrCursor[T, R]) OnNext(ctx context.Context) (RecordCursorResult[R], error) {
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return RecordCursorResult[R]{}, err
	}
	if !result.HasNext() {
		return NewResultNoNext[R](result.GetNoNextReason(), result.GetContinuation()), nil
	}
	mapped, mapErr := c.fn(result.GetValue())
	if mapErr != nil {
		return RecordCursorResult[R]{}, mapErr
	}
	return NewResultWithValue(mapped, result.GetContinuation()), nil
}

func (c *mapErrCursor[T, R]) Close() error { return c.inner.Close() }

func (c *mapErrCursor[T, R]) IsClosed() bool { return c.inner.IsClosed() }

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
		if err := cont.UnmarshalVT(continuation); err == nil {
			c.outer = outerFactory(cont.GetOuterContinuation())
			if cont.InnerContinuation != nil {
				c.hasPending = true
				c.pendingInner = cont.InnerContinuation
				c.pendingCheck = cont.CheckValue
				// Initialize outerCont so that priorOuterCont is set correctly
				// when the first outer value is read. Without this, priorOuterCont
				// would be nil and the next continuation would restart outer from
				// the beginning instead of from the saved position.
				if cont.OuterContinuation != nil {
					c.outerCont = &BytesContinuation{bytes: cont.OuterContinuation}
				}
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
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[V]{}, err
		}
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
			wrapped, wrapErr := c.wrapOuterStopContinuation(outerResult.GetContinuation())
			if wrapErr != nil {
				return RecordCursorResult[V]{}, wrapErr
			}
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
				if !bytes.Equal(currentCheck, c.pendingCheck) {
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

func (c *flatMapCursor[T, V]) wrapContinuation(innerCont RecordCursorContinuation) RecordCursorContinuation {
	return &flatMapContinuationWrapper[T]{
		priorOuterCont: c.priorOuterCont,
		outerCont:      c.outerCont,
		outerValue:     c.outerValue,
		innerCont:      innerCont,
		checkValueFunc: c.checkValueFunc,
	}
}

func (c *flatMapCursor[T, V]) wrapOuterStopContinuation(outerCont RecordCursorContinuation) (RecordCursorContinuation, error) {
	outerBytes, err := outerCont.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("flatmap outer stop continuation: %w", err)
	}
	fm := &gen.FlatMapContinuation{
		OuterContinuation: outerBytes,
	}
	// Preserve pending inner continuation across outer-only resumes.
	// When outer stops before producing a value (e.g., outer limit hit
	// while filter rejects all items), the inner continuation from the
	// original resume must be carried forward.
	if c.hasPending {
		fm.InnerContinuation = c.pendingInner
		fm.CheckValue = c.pendingCheck
	}
	data, err := fm.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("flatmap outer stop continuation marshal: %w", err)
	}
	return &BytesContinuation{bytes: data}, nil
}

type flatMapContinuationWrapper[T any] struct {
	priorOuterCont RecordCursorContinuation
	outerCont      RecordCursorContinuation
	outerValue     *T
	innerCont      RecordCursorContinuation
	checkValueFunc func(T) []byte
}

func (w *flatMapContinuationWrapper[T]) ToBytes() ([]byte, error) {
	fm := &gen.FlatMapContinuation{}

	if w.innerCont == nil || w.innerCont.IsEnd() {
		// Inner cursor exhausted — resume from outer's current position (after this value)
		if w.outerCont != nil {
			outerBytes, err := w.outerCont.ToBytes()
			if err != nil {
				return nil, fmt.Errorf("flatmap continuation outer: %w", err)
			}
			fm.OuterContinuation = outerBytes
		}
	} else {
		// Inner cursor NOT exhausted — resume from prior outer position + inner position
		if w.priorOuterCont != nil {
			priorBytes, err := w.priorOuterCont.ToBytes()
			if err != nil {
				return nil, fmt.Errorf("flatmap continuation prior outer: %w", err)
			}
			fm.OuterContinuation = priorBytes
		}
		innerBytes, err := w.innerCont.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("flatmap continuation inner: %w", err)
		}
		fm.InnerContinuation = innerBytes
		if w.checkValueFunc != nil && w.outerValue != nil {
			fm.CheckValue = w.checkValueFunc(*w.outerValue)
		}
	}

	return fm.MarshalVT()
}

func (w *flatMapContinuationWrapper[T]) IsEnd() bool {
	return false
}

func (c *flatMapCursor[T, V]) Close() error {
	c.closed = true
	var firstErr error
	if c.inner != nil {
		firstErr = c.inner.Close()
	}
	if c.outer != nil {
		if err := c.outer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *flatMapCursor[T, V]) IsClosed() bool { return c.closed }

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
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[T]{}, err
		}
		result, err := c.onNextWithRetry(ctx, 0)
		if err != nil {
			return RecordCursorResult[T]{}, err
		}

		if result.HasStoppedBeforeEnd() {
			// Inner cursor stopped due to a limit — create new transaction and continue
			contBytes, contErr := result.GetContinuation().ToBytes()
			if contErr != nil {
				return RecordCursorResult[T]{}, fmt.Errorf("auto-continuing cursor continuation: %w", contErr)
			}
			// Guard against infinite loop: if continuation is nil/end, the cursor
			// has nothing to resume from. Treat as source exhausted.
			if contBytes == nil || result.GetContinuation().IsEnd() {
				return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
			}
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
		cont, contErr := c.lastContinuation()
		if contErr != nil {
			return RecordCursorResult[T]{}, contErr
		}
		if err := c.openContextAndGenerateCursor(ctx, cont); err != nil {
			return RecordCursorResult[T]{}, err
		}
	}

	result, err := c.currentCursor.OnNext(ctx)
	if err != nil {
		if !c.isRetryableForContinuation(err) || attempt >= c.maxRetries {
			return RecordCursorResult[T]{}, err
		}
		// Retryable error — create new cursor from last successful position
		cont, contErr := c.lastContinuation()
		if contErr != nil {
			return RecordCursorResult[T]{}, contErr
		}
		if openErr := c.openContextAndGenerateCursor(ctx, cont); openErr != nil {
			return RecordCursorResult[T]{}, openErr
		}
		return c.onNextWithRetry(ctx, attempt+1)
	}

	return result, nil
}

// isRetryableForContinuation extends isRetryableError with transaction_timed_out
// (1031). Normally 1031 is not retryable — retrying the same transaction won't
// help. But AutoContinuingCursor creates a NEW transaction with a saved
// continuation, so it's safe to retry from the last successful position. This
// handles the case where a scan hits FDB's 5-second transaction timeout before
// the application-level time limit fires.
func (c *autoContinuingCursor[T]) isRetryableForContinuation(err error) bool {
	if isRetryableError(err) {
		return true
	}
	var fdbErr fdb.Error
	if errors.As(err, &fdbErr) && fdbErr.Code == 1031 { // transaction_timed_out
		return true
	}
	return false
}

func (c *autoContinuingCursor[T]) lastContinuation() ([]byte, error) {
	if c.lastResult == nil {
		return nil, nil
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
		firstErr = c.currentCursor.Close()
		c.currentCursor = nil
	}
	if c.currentCtx != nil {
		c.currentCtx.Transaction().Cancel()
		c.currentCtx = nil
	}
	return firstErr
}

func (c *autoContinuingCursor[T]) IsClosed() bool { return c.closed }
