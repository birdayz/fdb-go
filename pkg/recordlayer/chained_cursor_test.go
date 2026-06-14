package recordlayer

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
)

func encodeInt64(v int64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(v))
	return buf
}

func decodeInt64(b []byte) (int64, bool) {
	if len(b) < 8 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(b)), true
}

func TestChainedCursorBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Generate 1, 2, 3, 4, 5
	cursor := Chained(
		func(prev *int64) (*int64, error) {
			var next int64
			if prev == nil {
				next = 1
			} else if *prev >= 5 {
				return nil, nil // exhausted
			} else {
				next = *prev + 1
			}
			return &next, nil
		},
		encodeInt64, decodeInt64, nil,
	)

	var results []int64
	for v, iterErr := range Seq2(cursor, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	expected := []int64{1, 2, 3, 4, 5}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestChainedCursorEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Chained(
		func(prev *int64) (*int64, error) {
			return nil, nil // immediately exhausted
		},
		encodeInt64, decodeInt64, nil,
	)

	result, err := cursor.OnNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.HasNext() {
		t.Fatal("expected no results")
	}
}

func TestChainedCursorContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	gen := func(prev *int64) (*int64, error) {
		var next int64
		if prev == nil {
			next = 1
		} else if *prev >= 5 {
			return nil, nil
		} else {
			next = *prev + 1
		}
		return &next, nil
	}

	// Read first 3 values
	cursor := Chained(gen, encodeInt64, decodeInt64, nil)

	var lastCont RecordCursorContinuation
	for i := 0; i < 3; i++ {
		result, err := cursor.OnNext(ctx)
		if err != nil || !result.HasNext() {
			t.Fatalf("expected result %d", i+1)
		}
		lastCont = result.GetContinuation()
	}

	// Resume from continuation
	contBytes, contBytesErr := lastCont.ToBytes()
	if contBytesErr != nil {
		t.Fatalf("lastCont.ToBytes() error: %v", contBytesErr)
	}
	cursor2 := Chained(gen, encodeInt64, decodeInt64, contBytes)

	var results []int64
	for v, iterErr := range Seq2(cursor2, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	// Should get 4, 5
	expected := []int64{4, 5}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestChainedCursorError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Chained(
		func(prev *int64) (*int64, error) {
			return nil, fmt.Errorf("generator error")
		},
		encodeInt64, decodeInt64, nil,
	)

	_, err := cursor.OnNext(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestChainedCursorSeq2(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Chained(
		func(prev *int64) (*int64, error) {
			var next int64
			if prev == nil {
				next = 10
			} else if *prev >= 12 {
				return nil, nil
			} else {
				next = *prev + 1
			}
			return &next, nil
		},
		encodeInt64, decodeInt64, nil,
	)

	var results []int64
	for v, err := range Seq2(cursor, ctx) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, v)
	}

	expected := []int64{10, 11, 12}
	if len(results) != len(expected) {
		t.Fatalf("got %v, want %v", results, expected)
	}
}

func TestChainedCursorNilEncode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// nil encode — continuations will be StartContinuation (no position info available)
	cursor := Chained[int64](
		func(prev *int64) (*int64, error) {
			if prev == nil {
				v := int64(1)
				return &v, nil
			}
			return nil, nil
		},
		nil, nil, nil,
	)

	result, err := cursor.OnNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !result.HasNext() || result.GetValue() != 1 {
		t.Fatalf("expected 1, got %v", result)
	}
	// With nil encode, continuation is StartContinuation (not end — we have a value,
	// so EndContinuation would violate the invariant).
	if result.GetContinuation().IsEnd() {
		t.Fatal("expected non-end continuation (StartContinuation) with nil encode")
	}
}
