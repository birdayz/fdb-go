package recordlayer

import (
	"context"
	"errors"
	"testing"
	"time"

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
		newDB := func() *FDBDatabase {
			env := dst.NewSim(41)
			return NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
		}
		db := newDB()
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
		// The same seed draws the same two delays from a fresh exponential delay.
		ref := newDB()
		delay := newExponentialDelay(ref.InitialDelay(), ref.MaxDelay(), ref.Env())
		want := delay.delay() + delay.delay()
		if count, total := timer.GetCount(EventRetryDelay), timer.GetTimeNanos(EventRetryDelay); count != 2 || total != int64(want) || want == 0 {
			t.Fatalf("%d retry delays totalling %v, want 2 totalling the exponential delay's %v", count, time.Duration(total), want)
		}
	})

	t.Run("cancelling the context ends the wait", func(t *testing.T) {
		t.Parallel()
		db := NewFDBDatabaseWithBackend(simfdb.New(dst.NewSim(43))) // no env: the delay is waited
		if err := db.SetRetryDelays(time.Minute, time.Minute); err != nil {
			t.Fatal(err)
		}
		oi := &OnlineIndexer{db: db, maxRetries: 5, throttle: newIndexingThrottle(100, 5, 0, 0)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		_, _, err := oi.buildRangeWithRetries(ctx, func(context.Context) (int64, bool, error) {
			time.AfterFunc(50*time.Millisecond, cancel)
			return 10, true, tooLarge
		})
		var fdbErr fdb.Error
		if !errors.Is(err, context.Canceled) || !errors.As(err, &fdbErr) || fdbErr.Code != 2101 || time.Since(start) > 2*time.Second {
			t.Fatalf("buildRangeWithRetries = %v after %v, want the cancellation and the last error well within the minute's delay", err, time.Since(start))
		}
	})
}
