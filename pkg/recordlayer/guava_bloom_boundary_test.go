package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestGuavaBloomFilterHashCountBoundary(t *testing.T) {
	t.Parallel()
	for _, hashes := range []int{1, 255} {
		f, err := newGuavaBloomFilter(500, math.Ldexp(1, -hashes))
		if err != nil {
			t.Fatalf("create %d hashes: %v", hashes, err)
		}
		if f.numHashFunctions != hashes {
			t.Fatalf("hash count = %d, want %d", f.numHashFunctions, hashes)
		}
		key := tuple.Tuple{int64(42)}
		if err := f.put(key); err != nil {
			t.Fatal(err)
		}
		encoded := f.writeTo()
		if int(encoded[1]) != hashes {
			t.Fatalf("encoded hash count = %d, want %d", encoded[1], hashes)
		}
		restored, err := readGuavaBloomFilter(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if restored.numHashFunctions != hashes || !bytes.Equal(restored.writeTo(), encoded) {
			t.Fatalf("%d-hash filter changed on round trip", hashes)
		}
		if contains, err := restored.mightContain(key); err != nil || !contains {
			t.Fatalf("inserted key: contains=%v, err=%v", contains, err)
		}
		if contains, err := restored.mightContain(tuple.Tuple{int64(43)}); err != nil || contains {
			t.Fatalf("absent key: contains=%v, err=%v", contains, err)
		}
	}

	for _, hashes := range []int{256, 1074} {
		f, err := newGuavaBloomFilter(500, math.Ldexp(1, -hashes))
		var argumentErr *RecordCoreArgumentError
		if f != nil || !errors.As(err, &argumentErr) {
			t.Fatalf("%d hashes: filter=%v, want RecordCoreArgumentError, got %v", hashes, f, err)
		}
		for _, context := range []string{"numHashFunctions", "255", "expectedInsertions=500", "fpp="} {
			if !strings.Contains(argumentErr.Message, context) {
				t.Fatalf("missing %q context in %q", context, argumentErr.Message)
			}
		}
	}
}

func TestGuavaBloomFilterRejectsZeroHashCount(t *testing.T) {
	t.Parallel()
	for _, strategy := range []byte{bloomStrategyMitz32, bloomStrategyMitz64} {
		// A complete one-word filter, not a truncated-header/bit-array failure.
		encoded := []byte{strategy, 0, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		f, err := readGuavaBloomFilter(encoded)
		var coreErr *RecordCoreError
		var argumentErr *RecordCoreArgumentError
		if f != nil || !errors.As(err, &coreErr) || !errors.As(err, &argumentErr) {
			t.Fatalf("strategy %d: filter=%v, want typed decode error and argument cause, got %v", strategy, f, err)
		}
		if coreErr.Message != "unable to deserialize bloom filter" || !strings.Contains(argumentErr.Message, "numHashFunctions (0)") {
			t.Fatalf("strategy %d: decode error=%v, cause=%v", strategy, coreErr, argumentErr)
		}
	}
}

func TestProbableIntersectionHashCountBoundaryPaging(t *testing.T) {
	t.Parallel()
	key := func(v int64) (tuple.Tuple, error) { return tuple.Tuple{v}, nil }
	fns := []CursorFunction[int64]{
		func(cont []byte) RecordCursor[int64] {
			return FromListWithContinuation([]int64{1, 2, 3, 4, 5}, cont)
		},
		func(cont []byte) RecordCursor[int64] {
			return FromListWithContinuation([]int64{1, 3, 5, 7, 9}, cont)
		},
	}
	var continuation []byte
	var got []int64
	resumes := 0
	for pages := 0; ; pages++ {
		if pages == 20 {
			t.Fatal("probable intersection did not exhaust")
		}
		cursor, err := ProbableIntersection(key, fns, 500, math.Ldexp(1, -255), continuation)
		if err != nil {
			t.Fatal(err)
		}
		page := LimitRowsCursor(cursor, 1)
		t.Cleanup(func() { _ = page.Close() })
		for {
			result, err := page.OnNext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.HasNext() {
				got = append(got, result.GetValue())
				continue
			}
			continuation, err = result.GetContinuation().ToBytes()
			if err != nil {
				t.Fatal(err)
			}
			if continuation != nil {
				if result.GetNoNextReason() != ReturnLimitReached {
					t.Fatalf("page stopped with %v", result.GetNoNextReason())
				}
				states, err := decodeProbableIntersectionContinuation(continuation, len(fns))
				if err != nil {
					t.Fatal(err)
				}
				for i, state := range states {
					if len(state.bloom) < 6 || state.bloom[1] != 255 {
						t.Fatalf("child %d did not preserve 255 hashes", i)
					}
				}
				resumes++
			} else if result.GetNoNextReason() != SourceExhausted {
				t.Fatalf("end stopped with %v", result.GetNoNextReason())
			}
			break
		}
		if err := page.Close(); err != nil {
			t.Fatal(err)
		}
		if continuation == nil {
			break
		}
	}
	if !reflect.DeepEqual(got, []int64{1, 3, 5}) || resumes < 2 {
		t.Fatalf("paged intersection=%v, resumes=%d; want [1 3 5] across multiple resumes", got, resumes)
	}
}

type bloomFactoryCursor struct {
	RecordCursor[int64]
	closes int
}

func (c *bloomFactoryCursor) Close() error {
	c.closes++
	return c.RecordCursor.Close()
}

func TestProbableIntersectionValidatesAllFiltersBeforeOpeningChildren(t *testing.T) {
	t.Parallel()
	valid := []byte{bloomStrategyMitz64, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
	invalid := bytes.Clone(valid)
	invalid[1] = 0
	children := []*bloomFactoryCursor{{RecordCursor: Empty[int64]()}, {RecordCursor: Empty[int64]()}}
	opened := [2]int{}
	fns := make([]CursorFunction[int64], 2)
	for i := range fns {
		fns[i] = func([]byte) RecordCursor[int64] {
			opened[i]++
			return children[i]
		}
	}
	key := func(v int64) (tuple.Tuple, error) { return tuple.Tuple{v}, nil }
	encode := func(second []byte) []byte {
		t.Helper()
		data, err := (&gen.ProbableIntersectionContinuation{
			ChildState: []*gen.ProbableIntersectionContinuation_CursorState{
				{BloomFilter: valid}, {BloomFilter: second},
			},
		}).MarshalVT()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	cur, err := ProbableIntersection(key, fns, 500, 0.01, encode(invalid))
	var core *RecordCoreError
	var argument *RecordCoreArgumentError
	if cur != nil || !errors.As(err, &core) || !errors.As(err, &argument) {
		t.Fatalf("invalid second filter: cursor=%v, error=%v", cur, err)
	}
	if opened != [2]int{} || children[0].closes != 0 || children[1].closes != 0 {
		t.Fatalf("invalid filter opened factory resources: opened=%v, closes=%d/%d", opened, children[0].closes, children[1].closes)
	}
	cur, err = ProbableIntersection(key, fns, 500, 0.01, encode(valid))
	if err != nil {
		t.Fatal(err)
	}
	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}
	if opened != [2]int{1, 1} || children[0].closes != 1 || children[1].closes != 1 {
		t.Fatalf("valid filters lost ownership: opened=%v, closes=%d/%d", opened, children[0].closes, children[1].closes)
	}
}

func TestProbableIntersectionRejectsInvalidHashCounts(t *testing.T) {
	t.Parallel()
	opened := false
	key := func(v int64) (tuple.Tuple, error) { return tuple.Tuple{v}, nil }
	fn := func(cont []byte) RecordCursor[int64] {
		opened = true
		return FromListWithContinuation([]int64{1}, cont)
	}
	fns := []CursorFunction[int64]{fn, fn}
	cursor, err := ProbableIntersection(key, fns, 500, math.Ldexp(1, -256), nil)
	var argumentErr *RecordCoreArgumentError
	if cursor != nil || !errors.As(err, &argumentErr) || opened {
		t.Fatalf("256 hashes: cursor=%v, err=%v, opened=%v", cursor, err, opened)
	}

	// This is the zero byte that an overflowing 256-hash filter used to write.
	encoded := []byte{bloomStrategyMitz64, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
	continuation := &gen.ProbableIntersectionContinuation{
		ChildState: []*gen.ProbableIntersectionContinuation_CursorState{
			{BloomFilter: encoded},
			{BloomFilter: encoded},
		},
	}
	data, err := continuation.MarshalVT()
	if err != nil {
		t.Fatal(err)
	}
	cursor, err = ProbableIntersection(key, fns, 500, math.Ldexp(1, -255), data)
	var coreErr *RecordCoreError
	if cursor != nil || !errors.As(err, &coreErr) || !errors.As(err, &argumentErr) || opened {
		t.Fatalf("zero-hash continuation: cursor=%v, err=%v, opened=%v", cursor, err, opened)
	}
}
