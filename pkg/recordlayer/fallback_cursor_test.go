package recordlayer

import (
	"context"
	"fmt"
	"testing"
)

// errorAfterNCursor returns N results then errors.
type errorAfterNCursor[T any] struct {
	items []T
	pos   int
	err   error
}

func (c *errorAfterNCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	if c.pos >= len(c.items) {
		return RecordCursorResult[T]{}, c.err
	}
	val := c.items[c.pos]
	c.pos++
	return NewResultWithValue(val, &BytesContinuation{bytes: []byte{byte(c.pos)}}), nil
}
func (c *errorAfterNCursor[T]) Close() error { return nil }

func (c *errorAfterNCursor[T]) IsClosed() bool { return false }

func TestFallbackCursorNoError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := FromList([]int{1, 2, 3})
	fallbackCalled := false
	cursor := Fallback(inner, func(last *RecordCursorResult[int]) RecordCursor[int] {
		fallbackCalled = true
		return FromList([]int{99})
	})

	var results []int
	for v := range Seq(cursor, ctx) {
		results = append(results, v)
	}

	if fallbackCalled {
		t.Fatal("fallback should not have been called")
	}
	expected := []int{1, 2, 3}
	if len(results) != len(expected) {
		t.Fatalf("got %v, want %v", results, expected)
	}
}

func TestFallbackCursorImmediateError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := &errorCursor[int]{err: fmt.Errorf("inner failed")}
	var receivedLast *RecordCursorResult[int]
	cursor := Fallback[int](inner, func(last *RecordCursorResult[int]) RecordCursor[int] {
		receivedLast = last
		return FromList([]int{10, 20})
	})

	var results []int
	for v := range Seq(cursor, ctx) {
		results = append(results, v)
	}

	if receivedLast != nil {
		t.Fatal("expected nil last result for immediate error")
	}
	expected := []int{10, 20}
	if len(results) != len(expected) {
		t.Fatalf("got %v, want %v", results, expected)
	}
}

func TestFallbackCursorErrorAfterSomeResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := &errorAfterNCursor[int]{items: []int{1, 2}, err: fmt.Errorf("boom")}
	var receivedLast *RecordCursorResult[int]
	cursor := Fallback[int](inner, func(last *RecordCursorResult[int]) RecordCursor[int] {
		receivedLast = last
		return FromList([]int{3, 4, 5})
	})

	var results []int
	for v := range Seq(cursor, ctx) {
		results = append(results, v)
	}

	if receivedLast == nil {
		t.Fatal("expected last result")
	}
	if receivedLast.GetValue() != 2 {
		t.Fatalf("last result: got %d, want 2", receivedLast.GetValue())
	}
	// Should get 1, 2 from inner, then 3, 4, 5 from fallback
	expected := []int{1, 2, 3, 4, 5}
	if len(results) != len(expected) {
		t.Fatalf("got %v, want %v", results, expected)
	}
}

func TestFallbackCursorBothFail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := &errorCursor[int]{err: fmt.Errorf("inner failed")}
	cursor := Fallback[int](inner, func(last *RecordCursorResult[int]) RecordCursor[int] {
		return &errorCursor[int]{err: fmt.Errorf("fallback also failed")}
	})

	// First call: inner fails → switches to fallback → fallback fails
	_, err := cursor.OnNext(ctx)
	if err == nil {
		t.Fatal("expected error from fallback")
	}

	// Second call: alreadyFailed is true → wrapped error
	_, err = cursor.OnNext(ctx)
	if err == nil {
		t.Fatal("expected wrapped error")
	}
}

func TestFallbackCursorSeq2Error(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := &errorCursor[int]{err: fmt.Errorf("fail")}
	cursor := Fallback[int](inner, func(last *RecordCursorResult[int]) RecordCursor[int] {
		return &errorCursor[int]{err: fmt.Errorf("fallback fail")}
	})

	for _, err := range Seq2(cursor, ctx) {
		if err != nil {
			return // Expected
		}
	}
	t.Fatal("expected error from Seq2")
}
