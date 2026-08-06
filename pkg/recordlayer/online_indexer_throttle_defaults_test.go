package recordlayer

import (
	"context"
	"testing"
	"time"
)

// TestOnlineIndexerBuilderRetryDefaults pins the two halves of the throttling
// contract that a caller who never touches the builder gets.
//
// Both halves were wrong together, which is why neither showed up on its own: the
// builder defaulted maxRetries to 0 while the build loop applied the
// records-per-second pacing only when maxRetries > 0, so SetRecordsPerSecond was a
// no-op in the default configuration and the adaptive limit-halving never engaged
// either. Java has no such coupling — OnlineIndexOperationConfig.Builder defaults
// maxRetries to DEFAULT_MAX_RETRIES = 100 (OnlineIndexOperationConfig.java:52,251),
// and IndexingBase.java:512 takes the inter-range wait on every non-final range
// regardless of the retry budget.
func TestOnlineIndexerBuilderRetryDefaults(t *testing.T) {
	t.Parallel()

	t.Run("maxRetries defaults to Java's 100", func(t *testing.T) {
		t.Parallel()
		b := NewOnlineIndexerBuilder()
		if got := b.indexer.maxRetries; got != 100 {
			t.Errorf("default maxRetries = %d, want 100 (Java DEFAULT_MAX_RETRIES); a 0 "+
				"default disables the adaptive limit-halving for every caller that "+
				"never calls SetMaxRetries", got)
		}
	})

	t.Run("SetRecordsPerSecond paces without SetMaxRetries", func(t *testing.T) {
		t.Parallel()
		// Exactly the shape that used to be silently ignored: rps set, retries never
		// mentioned. The pacing must follow from the rps setting alone.
		b := NewOnlineIndexerBuilder().SetLimit(100).SetRecordsPerSecond(10)
		th := newIndexingThrottle(b.indexer.limit, b.indexer.maxRetries,
			b.indexer.recordsPerSecond, b.indexer.enforcedPostTransactionDelay)
		th.handleSuccess(10) // one committed range of 10 records, at 10 records/sec
		if got := th.waitTimeMillis(); got < 900 {
			t.Errorf("waitTimeMillis = %d, want ~999ms; SetRecordsPerSecond must throttle "+
				"without also requiring SetMaxRetries", got)
		}
	})

	t.Run("the build loop paces between ranges with retries disabled", func(t *testing.T) {
		t.Parallel()
		// The direction the throttle unit test cannot see: where the wait is TAKEN.
		// The pacing wait used to sit at the top of the per-attempt retry loop behind
		// `if oi.maxRetries > 0`, so an explicit SetMaxRetries(0) — or, before the
		// default moved, any build at all — scanned at full speed with rps configured.
		// Java takes it once per range in doneOrThrottleDelayAndMaybeLogProgress
		// (IndexingBase.java:512), which is what throttleBetweenRanges is.
		oi := &OnlineIndexer{
			maxRetries:                0, // retries explicitly OFF; pacing must still apply
			recordsPerSecond:          10,
			progressLogIntervalMillis: -1, // logging off; this test is about the wait
			throttle:                  newIndexingThrottle(100, 0, 10, 0),
		}
		oi.throttle.handleSuccess(10) // one committed range of 10 records at 10/sec

		start := time.Now()
		if err := oi.throttleBetweenRanges(context.Background(), time.Now(), 10, 10); err != nil {
			t.Fatalf("throttleBetweenRanges: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
			t.Errorf("inter-range wait was %v, want ~999ms; the records-per-second "+
				"pacing must not be gated behind a non-zero retry budget", elapsed)
		}
	})

	t.Run("SetMaxRetries(0) still disables retries", func(t *testing.T) {
		t.Parallel()
		// The default moved; the setter did not. An explicit 0 must remain "no retries",
		// otherwise a caller can no longer opt out of the retry loop at all.
		b := NewOnlineIndexerBuilder().SetMaxRetries(0)
		if got := b.indexer.maxRetries; got != 0 {
			t.Errorf("SetMaxRetries(0) left maxRetries = %d, want 0", got)
		}
	})
}
