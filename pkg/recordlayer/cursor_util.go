package recordlayer

import (
	"context"
	"iter"
)

// errIfDrainTruncated returns a *ScanLimitReachedError when a non-paginating drain's
// cursor stopped OUT-OF-BAND — i.e. a scan/byte/time resource limit cut it off, not
// true exhaustion or a clean ReturnLimitReached. A value-only drain (ForEach / First /
// GetCount / Reduce) discards the continuation and cannot paginate, so an out-of-band
// stop means the materialized/aggregated result is INCOMPLETE; surfacing it prevents a
// silently-truncated value (e.g. a partial CountRecords) (codex RFC-106a; mirrors Java's
// RecordCursor.NoNextReason.isOutOfBand()). Inert when no scan limit is set — leaf
// cursors then only emit SourceExhausted/ReturnLimitReached. AsListWithContinuation, the
// paginating variant, instead returns the continuation and must NOT use this.
func errIfDrainTruncated[T any](result RecordCursorResult[T]) error {
	if result.GetNoNextReason().IsOutOfBand() {
		return &ScanLimitReachedError{Reason: result.GetNoNextReason()}
	}
	return nil
}

// ForEach applies a function to each record in the cursor
func ForEach[T any](ctx context.Context, cursor RecordCursor[T], fn func(T) error) error {
	defer func() { _ = cursor.Close() }()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return err
		}

		if !result.HasNext() {
			return errIfDrainTruncated(result)
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

// AsListWithContinuation collects all records from the cursor into a slice and
// returns the final continuation bytes for pagination. Returns nil continuation
// when the source is exhausted.
// This is the common pattern for paginated APIs: drain page, return token.
func AsListWithContinuation[T any](ctx context.Context, cursor RecordCursor[T]) ([]T, []byte, error) {
	defer func() { _ = cursor.Close() }()
	var results []T
	for {
		if err := ctx.Err(); err != nil {
			return results, nil, err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return results, nil, err
		}
		if !result.HasNext() {
			cont := result.GetContinuation()
			if cont != nil && !cont.IsEnd() {
				contBytes, contErr := cont.ToBytes()
				if contErr != nil {
					return results, nil, contErr
				}
				return results, contBytes, nil
			}
			// Source exhausted — no continuation
			return results, nil, nil
		}
		results = append(results, result.GetValue())
	}
}

// First returns the first element from a cursor, or nil if empty.
// Matches Java's RecordCursor.first().
func First[T any](ctx context.Context, cursor RecordCursor[T]) (*T, error) {
	defer func() { _ = cursor.Close() }()
	result, err := cursor.OnNext(ctx)
	if err != nil {
		return nil, err
	}
	if !result.HasNext() {
		// Out-of-band before the first row → truncated; can't report "empty".
		return nil, errIfDrainTruncated(result)
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
		if err := ctx.Err(); err != nil {
			return count, err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return count, err
		}
		if !result.HasNext() {
			return count, errIfDrainTruncated(result)
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
		if err := ctx.Err(); err != nil {
			return acc, err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return acc, err
		}
		if !result.HasNext() {
			return acc, errIfDrainTruncated(result)
		}
		acc = fn(acc, result.GetValue())
	}
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
