// Portions derived from FoundationDB Record Layer (OnlineIndexer.java,
// IndexingBase.java, OnlineIndexOperationConfig.java, IndexingByIndex.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

// shouldLessenWork returns true if the error is a transient FDB error indicating
// the transaction did too much work (too large, timed out, conflicted, etc.).
// Only these errors warrant retrying with a smaller limit.
//
// Ports Java's IndexingThrottle.lessenWorkCodes (IndexingThrottle.java:64-70)
// 1:1, with codes from the FDB error table (flow/error_definitions.h):
// TIMED_OUT(1004), TRANSACTION_TOO_OLD(1007), NOT_COMMITTED(1020),
// TRANSACTION_TIMED_OUT(1031), COMMIT_READ_INCOMPLETE(2002),
// TRANSACTION_TOO_LARGE(2101). The previous list had the right error NAMES but
// wrong NUMBERS — 1028 is new_coordinators_timed_out, 1039 is
// cluster_version_changed, 2501 is not transaction_timed_out — and was missing
// 1004/2002/2101, so a transaction_too_large(2101) batch was not retried with a
// smaller limit (the bug surfaced once the client started enforcing the txn-size
// limit; see RFC-067).
func shouldLessenWork(err error) bool {
	var fdbErr fdb.Error
	if !errors.As(err, &fdbErr) {
		return false
	}
	switch fdbErr.Code {
	case 1004, // timed_out
		1007, // transaction_too_old
		1020, // not_committed (conflict)
		1031, // transaction_timed_out
		2002, // commit_read_incomplete
		2101: // transaction_too_large
		return true
	}
	return false
}

// buildRangeWithRetries wraps a buildRange call with retry logic.
// On transient FDB failures indicating the transaction was too large/slow,
// halves the limit and retries up to maxRetries times. Permanent errors
// are returned immediately without retry.
// Matches Java's IndexingThrottle.mayRetryAfterHandlingException().
func (oi *OnlineIndexer) buildRangeWithRetries(ctx context.Context, buildFn func(context.Context) (int64, bool, error)) (int64, bool, error) {
	if oi.throttle == nil {
		return buildFn(ctx)
	}

	var delay *exponentialDelay // made at the first retry: it draws from the env
	for attempt := 0; ; attempt++ {
		// Apply adaptive limit from throttle
		oi.limit = oi.throttle.getLimit()

		// No pacing wait here. Java takes the whole inter-range wait once per RANGE in
		// doneOrThrottleDelayAndMaybeLogProgress (IndexingBase.java:512) — i.e. in
		// throttleBetweenRanges — not once per retry ATTEMPT, so a range that retries
		// does not pay the rate-limit wait repeatedly.
		n, hasMore, err := buildFn(ctx)
		if err == nil {
			oi.throttle.handleSuccess(int(n))
			// The enforced post-transaction delay is applied by the build loop via
			// throttleBetweenRanges AFTER the time-limit check (Java
			// doneOrThrottleDelayAndMaybeLogProgress), not here — so an over-budget build
			// returns a TimeLimitExceededError without paying the delay, and the final range
			// pays nothing.
			return n, hasMore, nil
		}
		if oi.maxRetries <= 0 || !oi.throttle.mayRetryAfterHandlingException(err, attempt, int(n)) {
			return n, hasMore, err
		}
		// The limit is decreased; Java's buildCommitRetryAsync also waits before the retry.
		if delay == nil {
			delay = newExponentialDelay(oi.db.InitialDelay(), oi.db.MaxDelay(), oi.db.Env())
		}
		if werr := waitRetryDelay(ctx, oi.db.Env(), oi.db.Timer(), delay.delay(), err); werr != nil {
			return n, hasMore, werr
		}
	}
}

// throttleBetweenRanges paces the build between committed ranges: it validates the time
// limit FIRST (accounting for the upcoming enforced delay) and only then applies the
// enforced post-transaction delay. It returns a TimeLimitExceededError — without sleeping —
// when the build has already run, or after the delay would have run, past its time limit.
// This matches Java IndexingBase.doneOrThrottleDelayAndMaybeLogProgress + validateTimeLimit,
// where the time check precedes the wait and includes the wait duration. It is called only
// when more ranges remain, so the final range pays no delay (Java returns READY_FALSE when
// done). With no enforced delay configured it reduces to the prior plain time-limit check.
func (oi *OnlineIndexer) throttleBetweenRanges(ctx context.Context, startTime time.Time, totalRecords, rangeRecords int64) error {
	// One wait per range, computed once and then both logged and slept — exactly Java's
	// `long toWait = throttle.waitTimeMilliseconds()`. It is the enforced
	// post-transaction delay when set, otherwise the records-per-second pacing; a
	// negative enforced delay is not a wait, so it falls through to the rps path
	// rather than being subtracted from elapsed and loosening the time-limit check.
	delayMs := 0
	if oi.throttle != nil {
		delayMs = oi.throttle.waitTimeMillis()
	}
	// Java emits the progress log (with DELAY=toWait) BEFORE validateTimeLimit + the
	// delay, so the final range that trips the time limit still reports progress.
	oi.maybeLogBuildProgress(ctx, totalRecords, rangeRecords, delayMs)
	if oi.timeLimit > 0 {
		// Same clock the anchor was minted on. Measuring a sim-clock anchor against the wall
		// clock does not merely lose determinism — it compares two unrelated epochs, so the
		// limit trips on the first range or never.
		if elapsed := oi.db.Env().Since(startTime); elapsed+time.Duration(delayMs)*time.Millisecond >= oi.timeLimit {
			return &TimeLimitExceededError{TimeLimit: oi.timeLimit, Elapsed: elapsed}
		}
	}
	if oi.throttle != nil {
		oi.throttle.sleepMillis(ctx, delayMs)
	}
	return nil
}

// shouldLogBuildProgress is the progress-log throttle, ported 1:1 from Java
// IndexingBase.shouldLogBuildProgress(): interval<0 never logs; interval==0 logs
// every range; interval>0 logs at most once per interval ms. It advances the
// throttle clock only when it returns true. Single-threaded in the build loop.
func (oi *OnlineIndexer) shouldLogBuildProgress() bool {
	interval := oi.progressLogIntervalMillis
	now := time.Now().UnixMilli()
	if interval < 0 || (interval != 0 && interval > now-oi.timeOfLastProgressLogMillis) {
		return false
	}
	oi.timeOfLastProgressLogMillis = now
	return true
}

// maybeLogBuildProgress emits the per-range "Indexer: Built Range" INFO event,
// throttled by shouldLogBuildProgress. Ports Java
// IndexingBase.doneOrThrottleDelayAndMaybeLogProgress's logging arm. The
// Enabled(INFO) guard runs FIRST so a disabled INFO level skips both the log AND
// the throttle-clock advance — matching Java's `LOGGER.isInfoEnabled() &&
// shouldLogBuildProgress()` short-circuit (otherwise a disabled logger would
// still consume the throttle interval).
func (oi *OnlineIndexer) maybeLogBuildProgress(ctx context.Context, totalRecords, rangeRecords int64, delayMs int) {
	logger := oi.logger
	if logger == nil {
		logger = slog.Default()
	}
	if !logger.Enabled(ctx, slog.LevelInfo) {
		return
	}
	if !oi.shouldLogBuildProgress() {
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "Indexer: Built Range",
		slog.String("index", oi.targetIndexNamesForLog()),
		slog.Int64("records_scanned", totalRecords),
		slog.Int64("range_records", rangeRecords),
		slog.Int("limit", oi.currentLimitForLog()),
		slog.Int("delay_ms", delayMs),
	)
}

// targetIndexNamesForLog joins the target index names for the progress log
// (Java logs LogMessageKeys.INDEX_NAME = common.getTargetIndexesNames()).
func (oi *OnlineIndexer) targetIndexNamesForLog() string {
	names := make([]string, len(oi.targetIndexes))
	for i, idx := range oi.targetIndexes {
		names[i] = idx.Name
	}
	return strings.Join(names, ",")
}

// currentLimitForLog reports the throttle's live per-transaction record limit
// (Java throttle.logMessageKeyValues() includes LIMIT), falling back to the
// configured limit before the throttle is created.
func (oi *OnlineIndexer) currentLimitForLog() int {
	if oi.throttle != nil {
		return oi.throttle.getLimit()
	}
	return oi.limit
}
