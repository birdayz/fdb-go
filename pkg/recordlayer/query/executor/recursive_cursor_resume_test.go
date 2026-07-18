package executor

// Executor-level (no FDB) resume pins for recursiveCursor: a VALID
// mid-stream RecursiveContinuation must restore the traversal exactly —
// every row after the resume point once, in order, no duplicates, no
// loss. The FDB integration suite proves this end-to-end through SQL;
// this pin holds the same law at `go test ./executor/...` granularity so
// a regression in buildContinuation's level-indexing (LevelCursor[i] ↔
// nodes[i]) or the constructor's node-stack restore is caught without a
// container. Exhaustive: EVERY prefix point of the traversal is resumed,
// in both preorder and postorder.

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// recursiveTestTree drives a deterministic 2-ary tree: roots [1, 2];
// children of node n at parent depth d < 3 are [n*10+1, n*10+2]; depth-3
// nodes are leaves. The check function serializes the node id, so the
// LevelCursor CheckValue plumbing round-trips too (unchanged tree →
// checks pass silently).
func recursiveTestCursor(t *testing.T, preorder bool, cont []byte) *recursiveCursor {
	t.Helper()
	rootRows := []QueryResult{qr("id", int64(1)), qr("id", int64(2))}
	rootFn := func(c []byte) (recordlayer.RecordCursor[QueryResult], error) {
		return recordlayer.FromListWithContinuation(rootRows, c), nil
	}
	childFn := func(value QueryResult, depth int, c []byte) (recordlayer.RecordCursor[QueryResult], error) {
		if depth >= 3 {
			return recordlayer.FromListWithContinuation[QueryResult](nil, c), nil
		}
		id := fieldVal(t, value, "id")
		rows := []QueryResult{qr("id", id*10+1), qr("id", id*10+2)}
		return recordlayer.FromListWithContinuation(rows, c), nil
	}
	checkFn := func(value QueryResult) []byte {
		id := fieldVal(t, value, "id")
		return []byte{byte(id >> 8), byte(id)}
	}
	c, err := newRecursiveCursor(rootFn, childFn, checkFn, cont, preorder, nil)
	if err != nil {
		t.Fatalf("newRecursiveCursor(cont=%x): %v", cont, err)
	}
	return c
}

// drainRecursive drains the cursor, returning each emitted id and the
// continuation bytes captured AFTER that row (nil when the cursor ended).
func drainRecursive(t *testing.T, c *recursiveCursor) (ids []int64, tokens [][]byte) {
	t.Helper()
	ctx := context.Background()
	for {
		r, err := c.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !r.HasNext() {
			return ids, tokens
		}
		ids = append(ids, fieldVal(t, r.GetValue(), "id"))
		tokens = append(tokens, mustBytes(t, r.GetContinuation()))
	}
}

func TestRecursiveCursor_MidStreamResumeEveryPrefix(t *testing.T) {
	t.Parallel()
	for _, preorder := range []bool{true, false} {
		name := "postorder"
		if preorder {
			name = "preorder"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			full := recursiveTestCursor(t, preorder, nil)
			defer full.Close()
			wantIDs, tokens := drainRecursive(t, full)
			if len(wantIDs) != 14 {
				t.Fatalf("full %s walk = %v (%d rows), want 14 (2 roots × (1+2+4) nodes)", name, wantIDs, len(wantIDs))
			}

			for k, token := range tokens {
				if len(token) == 0 {
					// The final row's continuation is terminal (nil) — nothing to resume.
					if k != len(tokens)-1 {
						t.Fatalf("row %d (id %d) carries EMPTY continuation mid-stream", k, wantIDs[k])
					}
					continue
				}
				resumed := recursiveTestCursor(t, preorder, token)
				gotIDs, _ := drainRecursive(t, resumed)
				resumed.Close()
				want := wantIDs[k+1:]
				if len(gotIDs) != len(want) {
					t.Fatalf("resume after row %d (id %d): %d rows %v, want %d rows %v",
						k, wantIDs[k], len(gotIDs), gotIDs, len(want), want)
				}
				for i := range want {
					if gotIDs[i] != want[i] {
						t.Fatalf("resume after row %d (id %d): row %d = %d, want %d (full tail %v, got %v)",
							k, wantIDs[k], i, gotIDs[i], want[i], want, gotIDs)
					}
				}
			}
		})
	}
}
