package recordlayer

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestPrefixMergeChildStopsResumeWithoutLoss(t *testing.T) {
	t.Parallel()
	for _, reason := range []NoNextReason{ScanLimitReached, ByteLimitReached, TimeLimitReached} {
		for _, probable := range []bool{false, true} {
			t.Run(string(rune('a'+reason))+map[bool]string{false: "/union", true: "/intersection"}[probable], func(t *testing.T) {
				t.Parallel()
				lists := [][]int64{{1, 3, 5, 7}, {2, 3, 6, 7}, {3, 7, 8}}
				var factories []CursorFunction[int64]
				for _, list := range lists {
					factories = append(factories, func(cont []byte) RecordCursor[int64] {
						return fakeOutOfBandCursorWithReason(FromListWithContinuation(list, cont), 1, reason)
					})
				}
				var continuation []byte
				var got []int64
				stops := 0
				for page := 0; page < 30; page++ {
					var cur RecordCursor[int64]
					var err error
					if probable {
						cur, err = ProbableIntersection(int64Key, factories, 50, 1e-9, continuation)
					} else {
						cur, err = UnorderedUnion(factories, continuation)
					}
					if err != nil {
						t.Fatal(err)
					}
					for {
						r, err := cur.OnNext(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						if r.HasNext() {
							got = append(got, r.GetValue())
							continue
						}
						continuation, err = r.GetContinuation().ToBytes()
						if err != nil {
							t.Fatal(err)
						}
						if continuation != nil {
							stops++
							if r.GetNoNextReason() != reason {
								t.Fatalf("lost child reason: %v", r.GetNoNextReason())
							}
						} else if r.GetNoNextReason() != SourceExhausted {
							t.Fatalf("nil continuation without exhaustion: %v", r.GetNoNextReason())
						}
						again, err := cur.OnNext(t.Context())
						if err != nil || again.GetNoNextReason() != r.GetNoNextReason() {
							t.Fatal("terminal stop was not stable")
						}
						break
					}
					if err := cur.Close(); err != nil {
						t.Fatal(err)
					}
					if continuation == nil {
						break
					}
					if page == 29 {
						t.Fatal("child-stop continuation did not progress")
					}
				}
				if stops == 0 {
					t.Fatal("no actual child-stop page")
				}
				want := []int64{1, 2, 3, 3, 3, 5, 6, 7, 7, 7, 8}
				if probable {
					want = []int64{3, 7}
				}
				// Child-read scheduling may interleave union legs across pages.
				if !probable {
					slices.Sort(got)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("resumed results%v, want%v", got, want)
				}
			})
		}
	}
}

type prefixErrorChild struct {
	RecordCursor[int64]
	cause error
	calls int
}

func (c *prefixErrorChild) OnNext(ctx context.Context) (RecordCursorResult[int64], error) {
	c.calls++
	if c.calls == 2 {
		return RecordCursorResult[int64]{}, c.cause
	}
	return c.RecordCursor.OnNext(ctx)
}

func TestPrefixMergePropagatesChildErrors(t *testing.T) {
	t.Parallel()
	for _, probable := range []bool{false, true} {
		cause := &RecordCoreError{Message: "injected child failure"}
		var children []*prefixErrorChild
		factories := make([]CursorFunction[int64], 2)
		for i := range factories {
			factories[i] = func(cont []byte) RecordCursor[int64] {
				c := &prefixErrorChild{RecordCursor: FromListWithContinuation([]int64{1, 2}, cont), cause: cause}
				children = append(children, c)
				return c
			}
		}
		var cur RecordCursor[int64]
		var err error
		if probable {
			cur, err = ProbableIntersection(func(v int64) (tuple.Tuple, error) { return tuple.Tuple{v}, nil }, factories, 50, 0.01, nil)
		} else {
			cur, err = UnorderedUnion(factories, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		var gotErr error
		for range 10 {
			_, gotErr = cur.OnNext(t.Context())
			if gotErr != nil {
				break
			}
		}
		if !errors.Is(gotErr, cause) {
			t.Fatalf("child cause lost: %v", gotErr)
		}
		if err := cur.Close(); err != nil {
			t.Fatal(err)
		}
		for _, child := range children {
			if !child.IsClosed() {
				t.Fatal("child not closed")
			}
		}
	}
}
