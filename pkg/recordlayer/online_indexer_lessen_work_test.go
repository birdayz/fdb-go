package recordlayer

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

// Java's IndexingThrottle.buildCommitRetryAsync waits the runner's exponential delay, timed
// as RETRY_DELAY, before each retry with a lessened limit.
func TestLessenWorkRetryWaits(t *testing.T) {
	t.Parallel()
	tooLarge := fdb.Error{Code: 2101} // not retried by the runner, so only lessen-work sees it

	t.Run("each retry waits the backoff", func(t *testing.T) {
		t.Parallel()
		env := dst.NewSim(41)
		db := NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
		timer := NewStoreTimer()
		db.SetTimer(timer)
		oi := &OnlineIndexer{db: db, maxRetries: 5, throttle: newIndexingThrottle(100, 5, 0, 0)}
		attempts := 0
		_, _, err := oi.buildRangeWithRetries(context.Background(), func(context.Context) (int64, bool, error) {
			if attempts++; attempts <= 2 {
				return 10, true, tooLarge
			}
			return 10, true, nil
		})
		if err != nil || attempts != 3 {
			t.Fatalf("buildRangeWithRetries = %v after %d attempts, want success after 3", err, attempts)
		}
		if got := timer.GetCount(EventRetryDelay); got != 2 {
			t.Fatalf("%d retry delays, want 2", got)
		}
	})

	t.Run("a cancelled context ends the wait", func(t *testing.T) {
		t.Parallel()
		env := dst.NewSim(43)
		db := NewFDBDatabaseWithBackend(simfdb.New(env)) // no env: the delay is waited
		oi := &OnlineIndexer{db: db, maxRetries: 5, throttle: newIndexingThrottle(100, 5, 0, 0)}
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		_, _, err := oi.buildRangeWithRetries(ctx, func(context.Context) (int64, bool, error) {
			attempts++
			cancel()
			return 10, true, tooLarge
		})
		var fdbErr fdb.Error
		if attempts != 1 || !errors.Is(err, context.Canceled) || !errors.As(err, &fdbErr) || fdbErr.Code != 2101 {
			t.Fatalf("buildRangeWithRetries = %v after %d attempts, want the cancellation and the last error after 1", err, attempts)
		}
	})
}
