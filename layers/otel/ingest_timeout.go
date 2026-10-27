package otel

import (
	"context"
	"errors"
	"strconv"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

const maxSourceRuns = 8

func sourceRunBackoff(completedRuns int) time.Duration {
	delay := 25 * time.Millisecond
	for i := 1; i < completedRuns && delay < 250*time.Millisecond; i++ {
		delay *= 2
	}
	return min(delay, 250*time.Millisecond)
}

func waitSourceBackoff(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func isSourceTimeout(err error) bool {
	var domain *OtelError
	if errors.As(err, &domain) {
		return domain.Kind == "operation" && isSourceTimeout(domain.Cause)
	}
	var value fdb.Error
	if errors.As(err, &value) {
		return value.Code == 1031
	}
	var pointer *fdb.Error
	return errors.As(err, &pointer) && pointer != nil && pointer.Code == 1031
}

func (d *ingestRecorder) timeout() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.out.SourceTimeouts++
	d.mu.Unlock()
}

func (d *ingestRecorder) freshRetry() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.out.SourceFreshRetries++
	d.mu.Unlock()
}

// A 1031 can be maybe-committed and is terminal to FDB OnError. Fresh source
// Runs must reread old images; replaying captured mutations would be unsafe.
func (b *FDBBackend) saveIngestBatch(ctx context.Context, batch preparedBatch, d *ingestRecorder) error {
	var last error
	for run := 0; run < maxSourceRuns; run++ {
		if last = ctx.Err(); last != nil {
			break
		}
		if run > 0 {
			d.freshRetry()
		}
		last = b.saveIngestBatchRun(ctx, batch, d)
		if last == nil {
			return nil
		}
		if !isSourceTimeout(last) {
			break
		}
		d.timeout()
		if err := ctx.Err(); err != nil {
			last = err
			break
		}
		if run == maxSourceRuns-1 {
			break
		}
		started := time.Now()
		wait := b.ingestWait
		if wait == nil {
			wait = waitSourceBackoff
		}
		err := wait(ctx, sourceRunBackoff(run+1))
		d.phase("timeout_backoff", started)
		if err != nil {
			last = err
			break
		}
	}
	return wrap("operation", "ingest batch starting "+strconv.Itoa(batch.first), last)
}
