package recordlayer

import (
	"context"
	"time"
)

// indexingThrottle implements adaptive throttling for OnlineIndexer, matching
// Java's IndexingThrottle. Reduces the per-transaction record limit on transient
// FDB errors and enforces a records-per-second rate limit between transactions.
type indexingThrottle struct {
	// Configuration
	initialLimit     int // starting limit (from builder)
	maxRetries       int // max retries per range (0 = no retries)
	recordsPerSecond int // inter-transaction rate limit (0 = unlimited)
	// enforcedPostTransactionDelay, if > 0, is a fixed per-transaction delay (ms) applied
	// INSTEAD of the records-per-second throttle. Matches Java
	// OnlineIndexOperationConfig.enforcedPostTransactionDelay (0 = disabled).
	enforcedPostTransactionDelay int

	// Adaptive limit state (matches Java's IndexingThrottle.Booker)
	recordsLimit              int // current per-transaction limit
	lastFailureRecordsScanned int // records scanned when last failure occurred
	consecutiveFailureCount   int // for oneToNineFactor
	consecutiveSuccessCount   int // for optional limit increase (unused by default)

	// Rate limiter state (matches Java's Booker.waitTimeMilliseconds)
	forcedDelayTimestamp           time.Time // next allowed transaction start
	recordsScannedSinceForcedDelay int       // records since last delay reset
}

// newIndexingThrottle creates a throttle with the given initial parameters.
func newIndexingThrottle(initialLimit, maxRetries, recordsPerSecond, enforcedPostTransactionDelay int) *indexingThrottle {
	return &indexingThrottle{
		initialLimit:                 initialLimit,
		maxRetries:                   maxRetries,
		recordsPerSecond:             recordsPerSecond,
		enforcedPostTransactionDelay: enforcedPostTransactionDelay,
		recordsLimit:                 initialLimit,
	}
}

// getLimit returns the current per-transaction record limit.
func (t *indexingThrottle) getLimit() int {
	return t.recordsLimit
}

// sleepMillis blocks for ms milliseconds, or until ctx is done. Non-positive ms
// returns immediately. Java expresses the same thing as
// MoreAsyncUtil.delayedFuture(toWait) chained onto the build loop, which the
// runner cancels along with the rest of the build — so a cancelled or
// deadline-hit build must never sit out the full delay here either.
func (t *indexingThrottle) sleepMillis(ctx context.Context, ms int) {
	if ms <= 0 {
		return
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// mayRetryAfterHandlingException checks if the build should retry after an error.
// Returns true if the error is retryable and we haven't exceeded maxRetries.
// If retryable, decreases the limit based on how many records were scanned.
// Matches Java's IndexingThrottle.mayRetryAfterHandlingException().
func (t *indexingThrottle) mayRetryAfterHandlingException(err error, attempt int, recordsScanned int) bool {
	if attempt >= t.maxRetries || !shouldLessenWork(err) {
		return false
	}
	t.decreaseLimit(recordsScanned)
	return true
}

// decreaseLimit reduces the per-transaction limit based on the graduated
// oneToNineFactor schedule. Matches Java's IndexingThrottle.decreaseLimit().
//
// The limit is based on actual records scanned at failure time (not the
// requested limit), so it adapts to the real workload.
func (t *indexingThrottle) decreaseLimit(recordsScanned int) {
	t.consecutiveFailureCount++
	t.consecutiveSuccessCount = 0
	t.lastFailureRecordsScanned = recordsScanned

	factor := oneToNineFactor(t.consecutiveFailureCount)
	newLimit := (recordsScanned * factor) / 10
	if newLimit > recordsScanned-1 {
		newLimit = recordsScanned - 1
	}
	if newLimit < 1 {
		newLimit = 1
	}
	t.recordsLimit = newLimit
}

// handleSuccess updates throttle state after a successful transaction.
// Resets failure counters, potentially increases limit after sustained success,
// and applies rate limiting delay.
// Matches Java's IndexingThrottle.handleSuccess() + increaseLimit().
func (t *indexingThrottle) handleSuccess(recordsScanned int) {
	t.consecutiveFailureCount = 0
	t.consecutiveSuccessCount++
	t.recordsScannedSinceForcedDelay += recordsScanned

	// After 10 consecutive successes, gradually increase the limit back toward
	// initialLimit. Matches Java's config.getIncreaseLimitAfter() = 10.
	const increaseLimitAfter = 10
	if t.consecutiveSuccessCount >= increaseLimitAfter && t.recordsLimit < t.initialLimit {
		t.increaseLimit()
		t.consecutiveSuccessCount = 0
	}
}

// increaseLimit gradually increases the per-transaction limit.
// Matches Java's IndexingThrottle.increaseLimit() + getIncreasedLimit().
//
// Schedule:
//
//	limit < 5:   add 5
//	limit < 100: double
//	limit >= 100: multiply by 4/3
//
// Never exceeds initialLimit.
func (t *indexingThrottle) increaseLimit() {
	oldLimit := t.recordsLimit
	var newLimit int
	if oldLimit < 5 {
		newLimit = oldLimit + 5
	} else if oldLimit < 100 {
		newLimit = oldLimit * 2
	} else {
		newLimit = (4 * oldLimit) / 3
	}
	// Ensure at least +1 progress
	if newLimit <= oldLimit {
		newLimit = oldLimit + 1
	}
	// Cap at initialLimit
	if newLimit > t.initialLimit {
		newLimit = t.initialLimit
	}
	t.recordsLimit = newLimit
}

// waitTimeMillis computes the inter-range throttle wait and advances the rate
// limiter's clock. Port of Java IndexingThrottle.Booker.waitTimeMilliseconds()
// (IndexingThrottle.java:104-131), including its precedence: an enforced
// post-transaction delay REPLACES the records-per-second pacing rather than
// stacking with it, and an unlimited rate resets the bookkeeping so a config
// that flips back to a finite rate does not credit the unlimited stretch.
//
// It is deliberately independent of maxRetries. Java calls it from
// doneOrThrottleDelayAndMaybeLogProgress (IndexingBase.java:512) on every
// non-final range regardless of the retry budget; gating pacing behind retries
// would silently disable the operator's only rate control.
//
// This mutates the limiter clock, so call it exactly once per range and reuse
// the returned value for both the progress log and the sleep — which is also
// what makes the logged DELAY the wait actually taken for THIS range.
func (t *indexingThrottle) waitTimeMillis() int {
	if t.enforcedPostTransactionDelay > 0 {
		return t.enforcedPostTransactionDelay
	}
	if t.recordsPerSecond <= 0 {
		t.recordsScannedSinceForcedDelay = 0
		t.forcedDelayTimestamp = time.Time{}
		return 0
	}
	// No early return for a zero record count: Java has none either, and the
	// difference is not the returned wait (both give 0) but the limiter clock.
	// Java falls through and re-anchors forcedDelayTimestamp at now, so a range
	// that scanned nothing does not leave a stale anchor behind for the NEXT
	// range to measure a too-large delta against — which would cancel that
	// range's pacing wait and under-throttle relative to Java.
	now := time.Now()

	// Calculate how long we should have spent on the records we've processed
	// since the last delay reset: records / recordsPerSecond (in milliseconds).
	expectedMs := (1000 * t.recordsScannedSinceForcedDelay) / t.recordsPerSecond

	// Subtract elapsed time since last forced delay
	var deltaMs int64
	if !t.forcedDelayTimestamp.IsZero() {
		deltaMs = now.Sub(t.forcedDelayTimestamp).Milliseconds()
		if deltaMs < 0 {
			deltaMs = 0
		}
	}

	waitMs := int64(expectedMs) - deltaMs
	if waitMs < 0 {
		waitMs = 0
	}
	// Cap wait at 999ms (matching Java's min(999, ...))
	if waitMs > 999 {
		waitMs = 999
	}

	// Java assumes the next chunk starts at now+toWait rather than re-reading the
	// clock after the sleep, so the pacing target does not drift with scheduler jitter.
	t.forcedDelayTimestamp = now.Add(time.Duration(waitMs) * time.Millisecond)
	t.recordsScannedSinceForcedDelay = 0
	return int(waitMs)
}

// oneToNineFactor returns a limit multiplier (1-9) based on consecutive failures.
// The limit is reduced to (factor/10) of the records scanned at failure time.
// Matches Java's IndexingThrottle.oneToNineFactor().
//
// Schedule:
//
//	count=1: 9 (90%), count=2: 8 (80%), count=3: 7 (70%),
//	count=4-7: 5 (50%), count>7: 1 (10% — panic mode)
func oneToNineFactor(consecutiveFailures int) int {
	if consecutiveFailures > 7 {
		return 1 // panic mode: 10%
	}
	if consecutiveFailures > 3 {
		return 5 // 50%
	}
	f := 10 - consecutiveFailures
	if f < 1 {
		f = 1
	}
	return f
}
