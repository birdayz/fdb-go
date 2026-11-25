package executor

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

type textTestPage struct {
	rows         []QueryResult
	continuation []byte
	reason       recordlayer.NoNextReason
}

// Publish a page only after the transaction runner accepts its whole attempt.
func readTextTestPage(ctx context.Context, continuation []byte,
	run func(context.Context, func(*recordlayer.FDBRecordContext) (any, error)) (any, error),
	open func(*recordlayer.FDBRecordContext, []byte) (recordlayer.RecordCursor[QueryResult], error),
) (textTestPage, error) {
	input := slices.Clone(continuation)
	result, err := run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		cursor, err := open(rtx, slices.Clone(input))
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		var page textTestPage
		for {
			row, err := cursor.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !row.HasNext() {
				page.reason = row.GetNoNextReason()
				page.continuation, err = row.GetContinuation().ToBytes()
				return page, err
			}
			page.rows = append(page.rows, row.GetValue())
		}
	})
	if err != nil {
		return textTestPage{}, err
	}
	return result.(textTestPage), nil
}

func TestTextPageDiscardsRetriedAttempts(t *testing.T) {
	t.Parallel()
	for _, partial := range []bool{true, false} {
		t.Run(map[bool]string{true: "mid-page error", false: "retry after completed attempt"}[partial], func(t *testing.T) {
			t.Parallel()
			input := []byte{1, 2, 3}
			attempts := 0
			run := func(ctx context.Context, body func(*recordlayer.FDBRecordContext) (any, error)) (any, error) {
				first, err := body(nil)
				if partial {
					if !errors.Is(err, errInjectedLegFailure) || first != nil {
						t.Fatalf("first partial attempt: result=%v err=%v", first, err)
					}
				} else if err != nil || len(first.(textTestPage).rows) != 1 {
					t.Fatalf("completed attempt before simulated commit retry: result=%v err=%v", first, err)
				}
				return body(nil)
			}
			page, err := readTextTestPage(context.Background(), input, run, func(_ *recordlayer.FDBRecordContext, continuation []byte) (recordlayer.RecordCursor[QueryResult], error) {
				attempts++
				if !bytes.Equal(continuation, []byte{1, 2, 3}) {
					t.Fatalf("attempt %d did not restart at input continuation: %v", attempts, continuation)
				}
				continuation[0] = 9
				if attempts == 1 {
					if partial {
						return mkErrThenOKCursor(2, 99), nil
					}
					return plainCursor(99), nil
				}
				return plainCursor(1), nil
			})
			if err != nil || attempts != 2 || len(page.rows) != 1 || fieldVal(t, page.rows[0], "id") != 1 || !page.reason.IsSourceExhausted() || len(page.continuation) != 0 {
				t.Fatalf("published page=%v attempts=%d err=%v", page, attempts, err)
			}
			if !bytes.Equal(input, []byte{1, 2, 3}) {
				t.Fatalf("input continuation changed: %v", input)
			}
		})
	}
}
