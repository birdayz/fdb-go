package rowdiff

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// blockingQuerier answers no statement until its context ends.
type blockingQuerier struct{}

func (blockingQuerier) ExecContext(ctx context.Context, _ string, _ ...any) (sql.Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingQuerier) QueryContext(ctx context.Context, _ string, _ ...any) (*sql.Rows, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A context cancelled while statements are still being handed out must still
// resolve every statement's result: the caller awaits them in statement
// order, so one left open hangs the seed (and its sweep) forever.
func TestExecuteConcurrently_CancelResolvesEveryStatement(t *testing.T) {
	t.Parallel()
	c := Generate(1)
	ctx, cancel := context.WithCancel(context.Background())
	out, wait := executeConcurrently(ctx, c, []execQuerier{blockingQuerier{}})
	if len(out) < 2 {
		t.Fatalf("seed 1 has %d statements, want several", len(out))
	}
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i, r := range out {
			if _, _, err := r.await(); !errors.Is(err, context.Canceled) {
				t.Errorf("statement %d: err = %v, want context.Canceled", i, err)
			}
		}
		wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("awaiting the statements hung after cancellation")
	}
}
