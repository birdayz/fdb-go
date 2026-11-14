//go:build bazelrunfiles

package main

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

func TestMetricOperationsAwaitRealTransactionResults(t *testing.T) {
	t.Parallel()
	db, ctx := stacktesterDatabase(t)
	for _, op := range []string{"GET_ESTIMATED_RANGE_SIZE", "GET_RANGE_SPLIT_POINTS"} {
		for _, state := range []string{"healthy", "cancelled", "invalid_value", "inverted_range"} {
			t.Run(op+"/"+state, func(t *testing.T) {
				sm := NewStackMachine(db, []byte(op+"/"+state))
				begin, end := []byte("metric/a"), []byte("metric/z")
				code := 0
				switch state {
				case "cancelled":
					sm.currentTr().Cancel()
					code = 1025
				case "invalid_value":
					sm.currentTr().Set([]byte("invalid"), make([]byte, 100001))
					code = 2103
				case "inverted_range":
					begin, end = end, begin
					code = 2005
				}
				if op == "GET_RANGE_SPLIT_POINTS" {
					sm.push(1, int64(10000000))
				}
				sm.push(2, end)
				sm.push(3, begin)
				if err := sm.execute(ctx, 4, op, nil); err != nil {
					t.Fatal(err)
				}
				if len(sm.stack) != 1 || sm.stack[0].idx != 4 {
					t.Fatalf("wrong stack shape: %+v", sm.stack)
				}
				want := []byte("GOT_ESTIMATED_RANGE_SIZE")
				if op == "GET_RANGE_SPLIT_POINTS" {
					want = []byte("GOT_RANGE_SPLIT_POINTS")
				}
				if code != 0 {
					want = tuple.Tuple{[]byte("ERROR"), []byte(strconv.Itoa(code))}.Pack()
				}
				got, ok := sm.pop().value.([]byte)
				if !ok || !bytes.Equal(got, want) {
					t.Fatalf("result=%q want=%q", got, want)
				}
			})
		}
	}
}
