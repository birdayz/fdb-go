package recordlayer

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/gen"
)

func TestDedupCursorBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Adjacent duplicates removed
	cursor := Dedup(
		func(cont []byte) RecordCursor[int] { return FromList([]int{1, 1, 2, 2, 2, 3, 3, 1}) },
		intEqual, packInt, unpackInt, nil,
	)

	var results []int
	for v, err := range Seq2(cursor, ctx) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, v)
	}

	// Adjacent dedup: [1, 2, 3, 1] (last 1 is not adjacent to first 1)
	expected := []int{1, 2, 3, 1}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestDedupCursorAllSame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Dedup(
		func(cont []byte) RecordCursor[int] { return FromList([]int{5, 5, 5, 5}) },
		intEqual, packInt, unpackInt, nil,
	)

	var results []int
	for v, err := range Seq2(cursor, ctx) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, v)
	}

	if len(results) != 1 || results[0] != 5 {
		t.Fatalf("expected [5], got %v", results)
	}
}

func TestDedupCursorNoDuplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Dedup(
		func(cont []byte) RecordCursor[int] { return FromList([]int{1, 2, 3, 4}) },
		intEqual, packInt, unpackInt, nil,
	)

	var results []int
	for v, err := range Seq2(cursor, ctx) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, v)
	}

	expected := []int{1, 2, 3, 4}
	if len(results) != len(expected) {
		t.Fatalf("got %v, want %v", results, expected)
	}
}

func TestDedupCursorEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Dedup(
		func(cont []byte) RecordCursor[int] { return Empty[int]() },
		intEqual, packInt, unpackInt, nil,
	)

	result, err := cursor.OnNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.HasNext() {
		t.Fatal("expected no results")
	}
}

func TestDedupCursorSingle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Dedup(
		func(cont []byte) RecordCursor[int] { return FromList([]int{42}) },
		intEqual, packInt, unpackInt, nil,
	)

	var results []int
	for v, err := range Seq2(cursor, ctx) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, v)
	}

	if len(results) != 1 || results[0] != 42 {
		t.Fatalf("expected [42], got %v", results)
	}
}

func TestDedupCursorContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	data := []int{1, 1, 2, 2, 3, 3}

	cursor := Dedup(
		func(cont []byte) RecordCursor[int] {
			return FromListWithContinuation(data, cont)
		},
		intEqual, packInt, unpackInt, nil,
	)

	// Read first result
	r1, err := cursor.OnNext(ctx)
	if err != nil || !r1.HasNext() {
		t.Fatal("expected result 1")
	}
	if r1.GetValue() != 1 {
		t.Fatalf("result 1: got %d, want 1", r1.GetValue())
	}

	// Read second result
	r2, err := cursor.OnNext(ctx)
	if err != nil || !r2.HasNext() {
		t.Fatal("expected result 2")
	}
	if r2.GetValue() != 2 {
		t.Fatalf("result 2: got %d, want 2", r2.GetValue())
	}

	// Get continuation and resume
	cont := r2.GetContinuation()
	if cont == nil || cont.IsEnd() {
		t.Fatal("continuation should not be end")
	}

	// Resume from continuation
	contBytes, contBytesErr := cont.ToBytes()
	if contBytesErr != nil {
		t.Fatalf("cont.ToBytes() error: %v", contBytesErr)
	}
	cursor2 := Dedup(
		func(c []byte) RecordCursor[int] {
			return FromListWithContinuation(data, c)
		},
		intEqual, packInt, unpackInt, contBytes,
	)

	r3, err := cursor2.OnNext(ctx)
	if err != nil || !r3.HasNext() {
		t.Fatal("expected result 3")
	}
	if r3.GetValue() != 3 {
		t.Fatalf("result 3: got %d, want 3", r3.GetValue())
	}
}

func TestDedupCursorRestoredBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  []byte
		omit bool
	}{
		{name: "noncanonical", raw: []byte{0, 1}},
		{name: "present empty", raw: []byte{}},
		{name: "nil pack", raw: []byte{0, 1}, omit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := (&gen.DedupContinuation{InnerContinuation: []byte{}, LastValue: tc.raw}).MarshalVT()
			if err != nil {
				t.Fatal(err)
			}
			pack := func(v int) []byte { return []byte{byte(v)} }
			if tc.omit {
				pack = nil
			}
			cursor := Dedup(func([]byte) RecordCursor[int] { return FromListWithContinuation([]int{2}, nil) },
				intEqual, pack, func(b []byte) (int, bool) {
					if len(b) > 0 {
						b[0] = 0xff
					}
					return 1, true
				}, raw)
			defer cursor.Close()
			old, err := cursor.(*dedupCursor[int]).wrapContinuation(&BytesContinuation{bytes: []byte{1}})
			if err != nil {
				t.Fatal(err)
			}
			check := func(cont RecordCursorContinuation, want []byte) {
				t.Helper()
				encoded, err := cont.ToBytes()
				if err != nil {
					t.Fatal(err)
				}
				var decoded gen.DedupContinuation
				if err := decoded.UnmarshalVT(encoded); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(decoded.LastValue, want) || (decoded.LastValue == nil) != (want == nil) {
					t.Fatalf("lastValue=%x (nil=%t), want %x (nil=%t)", decoded.LastValue, decoded.LastValue == nil, want, want == nil)
				}
			}
			wantOld, wantNew := tc.raw, []byte{2}
			if tc.omit {
				wantOld, wantNew = nil, nil
			}
			check(old, wantOld)
			next, err := cursor.OnNext(context.Background())
			if err != nil || !next.HasNext() || next.GetValue() != 2 {
				t.Fatalf("distinct replacement: result=%+v, err=%v", next, err)
			}
			check(next.GetContinuation(), wantNew)
			check(old, wantOld)
		})
	}
}

func TestDedupCursorNilPack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// nil pack function — continuation won't have lastValue
	cursor := Dedup(
		func(cont []byte) RecordCursor[int] { return FromList([]int{1, 1, 2, 2}) },
		intEqual, nil, nil, nil,
	)

	var results []int
	for v, err := range Seq2(cursor, ctx) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, v)
	}

	expected := []int{1, 2}
	if len(results) != len(expected) {
		t.Fatalf("got %v, want %v", results, expected)
	}
}

func TestDedupCursorSeq2(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Dedup(
		func(cont []byte) RecordCursor[int] { return FromList([]int{1, 1, 2}) },
		intEqual, packInt, unpackInt, nil,
	)

	var results []int
	for v, err := range Seq2(cursor, ctx) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, v)
	}

	expected := []int{1, 2}
	if len(results) != len(expected) {
		t.Fatalf("got %v, want %v", results, expected)
	}
}
