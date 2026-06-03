package recordlayer

import (
	"fmt"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

func TestOneToNineFactor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		count  int
		factor int
	}{
		{0, 10},  // 10 - 0 = 10 (shouldn't happen in practice)
		{1, 9},   // 10 - 1 = 9, 90%
		{2, 8},   // 10 - 2 = 8, 80%
		{3, 7},   // 10 - 3 = 7, 70%
		{4, 5},   // > 3, 50%
		{5, 5},   // > 3, 50%
		{6, 5},   // > 3, 50%
		{7, 5},   // > 3, 50%
		{8, 1},   // > 7, panic mode 10%
		{9, 1},   // > 7, panic mode 10%
		{10, 1},  // > 7, panic mode 10%
		{100, 1}, // > 7, panic mode 10%
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("count=%d", tt.count), func(t *testing.T) {
			t.Parallel()
			got := oneToNineFactor(tt.count)
			if got != tt.factor {
				t.Errorf("oneToNineFactor(%d) = %d, want %d", tt.count, got, tt.factor)
			}
		})
	}
}

func TestDecreaseLimit(t *testing.T) {
	t.Parallel()

	t.Run("first failure with 100 records", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		th.decreaseLimit(100)
		// factor=9 (first failure), newLimit = 100*9/10 = 90
		if th.recordsLimit != 90 {
			t.Errorf("limit = %d, want 90", th.recordsLimit)
		}
		if th.consecutiveFailureCount != 1 {
			t.Errorf("consecutiveFailureCount = %d, want 1", th.consecutiveFailureCount)
		}
		if th.consecutiveSuccessCount != 0 {
			t.Errorf("consecutiveSuccessCount = %d, want 0", th.consecutiveSuccessCount)
		}
		if th.lastFailureRecordsScanned != 100 {
			t.Errorf("lastFailureRecordsScanned = %d, want 100", th.lastFailureRecordsScanned)
		}
	})

	t.Run("second consecutive failure", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		th.decreaseLimit(100) // first: factor=9, limit=90
		th.decreaseLimit(90)  // second: factor=8, limit=90*8/10=72
		if th.recordsLimit != 72 {
			t.Errorf("limit = %d, want 72", th.recordsLimit)
		}
		if th.consecutiveFailureCount != 2 {
			t.Errorf("consecutiveFailureCount = %d, want 2", th.consecutiveFailureCount)
		}
	})

	t.Run("third consecutive failure", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 10, 0)
		th.decreaseLimit(100) // 1st: factor=9, limit=90
		th.decreaseLimit(90)  // 2nd: factor=8, limit=72
		th.decreaseLimit(72)  // 3rd: factor=7, limit=72*7/10=50
		if th.recordsLimit != 50 {
			t.Errorf("limit = %d, want 50", th.recordsLimit)
		}
	})

	t.Run("panic mode after 8 failures", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 20, 0)
		// Simulate 8 consecutive failures at 100 records each
		for i := 0; i < 8; i++ {
			th.decreaseLimit(100)
		}
		// After 8th: factor=1, limit=100*1/10=10
		if th.recordsLimit != 10 {
			t.Errorf("limit = %d, want 10", th.recordsLimit)
		}
	})

	t.Run("lower bound is 1", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 20, 0)
		// With 1 record scanned and panic mode: 1*1/10=0, clamped to 1
		th.consecutiveFailureCount = 7 // next will be 8th → factor=1
		th.decreaseLimit(1)
		if th.recordsLimit != 1 {
			t.Errorf("limit = %d, want 1", th.recordsLimit)
		}
	})

	t.Run("limit never exceeds recordsScanned minus 1", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		// With 2 records scanned, factor=9: 2*9/10=1, and recordsScanned-1=1
		// So limit should be 1 (capped by recordsScanned-1)
		th.decreaseLimit(2)
		if th.recordsLimit != 1 {
			t.Errorf("limit = %d, want 1", th.recordsLimit)
		}
	})

	t.Run("resets consecutiveSuccessCount", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		th.consecutiveSuccessCount = 10
		th.decreaseLimit(100)
		if th.consecutiveSuccessCount != 0 {
			t.Errorf("consecutiveSuccessCount = %d, want 0", th.consecutiveSuccessCount)
		}
	})

	t.Run("recordsScanned 0 gives limit 1", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		th.decreaseLimit(0)
		// 0*9/10=0, max(0, 0-1)=-1 → clamped to 1
		if th.recordsLimit != 1 {
			t.Errorf("limit = %d, want 1", th.recordsLimit)
		}
	})
}

func TestMayRetryAfterHandlingException(t *testing.T) {
	t.Parallel()

	// Java IndexingThrottle.lessenWorkCodes (1:1): timed_out(1004),
	// transaction_too_old(1007), not_committed(1020), transaction_timed_out(1031),
	// commit_read_incomplete(2002), transaction_too_large(2101). RFC-067.
	retryableCodes := []int{1004, 1007, 1020, 1031, 2002, 2101}
	// Codes the OLD (buggy) list wrongly whitelisted — must NOT lessen work now:
	// 1028=new_coordinators_timed_out, 1039=cluster_version_changed, 2501≠any lessen code.
	notRetryableCodes := []int{1028, 1039, 2501, 9999}

	t.Run("returns false when attempt >= maxRetries", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)
		err := fdb.Error{Code: 1007}
		if th.mayRetryAfterHandlingException(err, 3, 50) {
			t.Error("should return false when attempt == maxRetries")
		}
		if th.mayRetryAfterHandlingException(err, 4, 50) {
			t.Error("should return false when attempt > maxRetries")
		}
	})

	t.Run("returns false for non-retryable errors", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)

		// Plain error (not fdb.Error)
		if th.mayRetryAfterHandlingException(fmt.Errorf("some error"), 0, 50) {
			t.Error("should return false for non-fdb error")
		}

		// fdb.Error with non-retryable code
		if th.mayRetryAfterHandlingException(fdb.Error{Code: 9999}, 0, 50) {
			t.Error("should return false for non-retryable fdb error code")
		}
	})

	t.Run("returns true for retryable FDB errors under max retries", func(t *testing.T) {
		t.Parallel()
		for _, code := range retryableCodes {
			t.Run(fmt.Sprintf("code=%d", code), func(t *testing.T) {
				t.Parallel()
				th := newIndexingThrottle(100, 3, 0)
				err := fdb.Error{Code: code}
				if !th.mayRetryAfterHandlingException(err, 0, 50) {
					t.Errorf("should return true for fdb error code %d", code)
				}
			})
		}
	})

	t.Run("does NOT retry codes outside the Java lessen-work set", func(t *testing.T) {
		t.Parallel()
		for _, code := range notRetryableCodes {
			code := code
			t.Run(fmt.Sprintf("code=%d", code), func(t *testing.T) {
				t.Parallel()
				th := newIndexingThrottle(100, 3, 0)
				if th.mayRetryAfterHandlingException(fdb.Error{Code: code}, 0, 50) {
					t.Errorf("code %d must NOT lessen work (not in Java lessenWorkCodes)", code)
				}
			})
		}
	})

	t.Run("decreases limit when returning true", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)
		err := fdb.Error{Code: 1007}
		before := th.getLimit()
		th.mayRetryAfterHandlingException(err, 0, 50)
		after := th.getLimit()
		if after >= before {
			t.Errorf("limit should decrease: before=%d, after=%d", before, after)
		}
		// factor=9 for first failure: 50*9/10=45
		if after != 45 {
			t.Errorf("limit = %d, want 45", after)
		}
	})

	t.Run("does not decrease limit when returning false", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)
		before := th.getLimit()
		th.mayRetryAfterHandlingException(fmt.Errorf("non-retryable"), 0, 50)
		if th.getLimit() != before {
			t.Errorf("limit should not change for non-retryable: before=%d, after=%d", before, th.getLimit())
		}
	})

	t.Run("returns false when maxRetries is 0", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 0, 0)
		err := fdb.Error{Code: 1007}
		if th.mayRetryAfterHandlingException(err, 0, 50) {
			t.Error("should return false when maxRetries is 0 (attempt 0 >= 0)")
		}
	})

	t.Run("wrapped fdb error is retryable", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)
		wrapped := fmt.Errorf("build failed: %w", fdb.Error{Code: 1020})
		if !th.mayRetryAfterHandlingException(wrapped, 0, 50) {
			t.Error("should return true for wrapped fdb error")
		}
	})
}

func TestHandleSuccess(t *testing.T) {
	t.Parallel()

	t.Run("resets failure count and increments success count", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)
		th.consecutiveFailureCount = 5
		th.consecutiveSuccessCount = 0
		th.handleSuccess(50)
		if th.consecutiveFailureCount != 0 {
			t.Errorf("consecutiveFailureCount = %d, want 0", th.consecutiveFailureCount)
		}
		if th.consecutiveSuccessCount != 1 {
			t.Errorf("consecutiveSuccessCount = %d, want 1", th.consecutiveSuccessCount)
		}
	})

	t.Run("increments consecutiveSuccessCount on repeated calls", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)
		th.handleSuccess(10)
		th.handleSuccess(20)
		th.handleSuccess(30)
		if th.consecutiveSuccessCount != 3 {
			t.Errorf("consecutiveSuccessCount = %d, want 3", th.consecutiveSuccessCount)
		}
	})

	t.Run("accumulates records scanned since forced delay", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)
		th.handleSuccess(10)
		th.handleSuccess(20)
		if th.recordsScannedSinceForcedDelay != 30 {
			t.Errorf("recordsScannedSinceForcedDelay = %d, want 30", th.recordsScannedSinceForcedDelay)
		}
	})
}

func TestGetLimit(t *testing.T) {
	t.Parallel()

	t.Run("returns initial limit", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		if th.getLimit() != 200 {
			t.Errorf("getLimit() = %d, want 200", th.getLimit())
		}
	})

	t.Run("reflects changes after decreaseLimit", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		th.decreaseLimit(100) // factor=9: 100*9/10=90
		if th.getLimit() != 90 {
			t.Errorf("getLimit() = %d, want 90", th.getLimit())
		}
	})
}

func TestIncreaseLimit(t *testing.T) {
	t.Parallel()

	t.Run("increases after 10 consecutive successes", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		// Decrease to force a low limit
		th.decreaseLimit(100) // limit = 90
		th.decreaseLimit(90)  // limit = 72
		if th.getLimit() != 72 {
			t.Fatalf("expected limit 72, got %d", th.getLimit())
		}

		// 9 successes — should NOT increase yet
		for i := 0; i < 9; i++ {
			th.handleSuccess(72)
		}
		if th.getLimit() != 72 {
			t.Errorf("limit should not change after 9 successes, got %d", th.getLimit())
		}

		// 10th success — should increase (72 < 100, so doubles to 144)
		th.handleSuccess(72)
		if th.getLimit() != 144 {
			t.Errorf("expected limit 144 after 10 successes, got %d", th.getLimit())
		}
	})

	t.Run("caps at initialLimit", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 5, 0)
		th.decreaseLimit(100) // limit = 90

		// 10 successes — 90 < 100, doubles to 180, but capped at 100
		for i := 0; i < 10; i++ {
			th.handleSuccess(90)
		}
		if th.getLimit() != 100 {
			t.Errorf("expected limit capped at 100, got %d", th.getLimit())
		}
	})

	t.Run("small limit adds 5", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(200, 5, 0)
		th.recordsLimit = 3 // force tiny limit

		for i := 0; i < 10; i++ {
			th.handleSuccess(3)
		}
		// 3 < 5, so adds 5 → 8
		if th.getLimit() != 8 {
			t.Errorf("expected limit 8, got %d", th.getLimit())
		}
	})

	t.Run("large limit uses 4/3 multiplier", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(10000, 5, 0)
		th.recordsLimit = 150

		for i := 0; i < 10; i++ {
			th.handleSuccess(150)
		}
		// 150 >= 100, so 4*150/3 = 200
		if th.getLimit() != 200 {
			t.Errorf("expected limit 200, got %d", th.getLimit())
		}
	})

	t.Run("no increase when already at initialLimit", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 5, 0)
		// Already at initialLimit — should not change
		for i := 0; i < 20; i++ {
			th.handleSuccess(100)
		}
		if th.getLimit() != 100 {
			t.Errorf("expected limit unchanged at 100, got %d", th.getLimit())
		}
	})
}

func TestWaitForRateLimit(t *testing.T) {
	t.Parallel()

	t.Run("returns immediately when recordsPerSecond is 0", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 0)
		th.recordsScannedSinceForcedDelay = 1000 // should be ignored
		start := time.Now()
		th.waitForRateLimit()
		elapsed := time.Since(start)
		if elapsed > 50*time.Millisecond {
			t.Errorf("should return immediately, took %v", elapsed)
		}
	})

	t.Run("returns immediately when no records scanned since last delay", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 100) // 100 records/sec
		th.recordsScannedSinceForcedDelay = 0
		start := time.Now()
		th.waitForRateLimit()
		elapsed := time.Since(start)
		if elapsed > 50*time.Millisecond {
			t.Errorf("should return immediately, took %v", elapsed)
		}
	})

	t.Run("returns immediately when negative recordsPerSecond", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, -1)
		th.recordsScannedSinceForcedDelay = 1000
		start := time.Now()
		th.waitForRateLimit()
		elapsed := time.Since(start)
		if elapsed > 50*time.Millisecond {
			t.Errorf("should return immediately with negative rate, took %v", elapsed)
		}
	})

	t.Run("resets state when enough time already elapsed", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 100) // 100 records/sec
		th.recordsScannedSinceForcedDelay = 10
		// Set forced delay far in the past so elapsed > expected
		th.forcedDelayTimestamp = time.Now().Add(-5 * time.Second)
		start := time.Now()
		th.waitForRateLimit()
		elapsed := time.Since(start)
		if elapsed > 50*time.Millisecond {
			t.Errorf("should not sleep when enough time elapsed, took %v", elapsed)
		}
		// State should be reset
		if th.recordsScannedSinceForcedDelay != 0 {
			t.Errorf("recordsScannedSinceForcedDelay = %d, want 0", th.recordsScannedSinceForcedDelay)
		}
	})

	t.Run("sleeps when rate limit exceeded", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 10) // 10 records/sec
		th.recordsScannedSinceForcedDelay = 10
		th.forcedDelayTimestamp = time.Now() // just now, 0ms elapsed
		// 10 records at 10/sec = 1000ms expected, but capped at 999ms
		start := time.Now()
		th.waitForRateLimit()
		elapsed := time.Since(start)
		// Should sleep close to 999ms (capped)
		if elapsed < 900*time.Millisecond {
			t.Errorf("should sleep ~999ms, only slept %v", elapsed)
		}
		if elapsed > 1200*time.Millisecond {
			t.Errorf("should sleep ~999ms, slept %v", elapsed)
		}
		// State should be reset after sleep
		if th.recordsScannedSinceForcedDelay != 0 {
			t.Errorf("recordsScannedSinceForcedDelay = %d, want 0", th.recordsScannedSinceForcedDelay)
		}
	})

	t.Run("wait is capped at 999ms", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 1) // 1 record/sec
		th.recordsScannedSinceForcedDelay = 100
		th.forcedDelayTimestamp = time.Now()
		// 100 records at 1/sec = 100,000ms expected, but capped at 999ms
		start := time.Now()
		th.waitForRateLimit()
		elapsed := time.Since(start)
		if elapsed > 1200*time.Millisecond {
			t.Errorf("should be capped at 999ms, slept %v", elapsed)
		}
	})

	t.Run("first call with no prior timestamp does not sleep when expected zero", func(t *testing.T) {
		t.Parallel()
		th := newIndexingThrottle(100, 3, 1000000) // very high rate
		th.recordsScannedSinceForcedDelay = 1      // 1 record at 1M/sec = ~0ms
		start := time.Now()
		th.waitForRateLimit()
		elapsed := time.Since(start)
		if elapsed > 50*time.Millisecond {
			t.Errorf("should return immediately for trivial rate, took %v", elapsed)
		}
	})
}

func TestNewIndexingThrottle(t *testing.T) {
	t.Parallel()

	th := newIndexingThrottle(150, 7, 500)
	if th.initialLimit != 150 {
		t.Errorf("initialLimit = %d, want 150", th.initialLimit)
	}
	if th.maxRetries != 7 {
		t.Errorf("maxRetries = %d, want 7", th.maxRetries)
	}
	if th.recordsPerSecond != 500 {
		t.Errorf("recordsPerSecond = %d, want 500", th.recordsPerSecond)
	}
	if th.recordsLimit != 150 {
		t.Errorf("recordsLimit = %d, want 150 (should equal initialLimit)", th.recordsLimit)
	}
	if th.consecutiveFailureCount != 0 {
		t.Errorf("consecutiveFailureCount = %d, want 0", th.consecutiveFailureCount)
	}
	if th.consecutiveSuccessCount != 0 {
		t.Errorf("consecutiveSuccessCount = %d, want 0", th.consecutiveSuccessCount)
	}
}
