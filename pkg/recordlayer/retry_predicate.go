// Portions derived from FoundationDB Record Layer (
// RecordCoreRetriableTransactionException.java, ExponentialDelay.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
)

// RecordCoreRetriableTransactionError is Java's
// RecordCoreRetriableTransactionException: a failure the transaction runner
// retries like a retryable FDB error.
type RecordCoreRetriableTransactionError struct {
	Message string
	Cause   error
}

func (e *RecordCoreRetriableTransactionError) Error() string { return e.Message }

func (e *RecordCoreRetriableTransactionError) Unwrap() error { return e.Cause }

func (*RecordCoreRetriableTransactionError) JavaRecordCoreException() {}

// isRetriableAnyCause is the transaction runner's retry rule,
// FDBDatabaseRunnerImpl.RunRetriable.handle (:195-207): retry when ANY cause
// in the chain is a retryable FDB error or a
// RecordCoreRetriableTransactionException. Go's chain is a tree (errors.Join,
// several %w), and a node is retriable when any branch is.
func isRetriableAnyCause(err error) bool {
	if err == nil {
		return false
	}
	switch e := err.(type) {
	case fdb.Error:
		return fdb.IsRetryable(e.Code)
	case *fdb.Error:
		return e != nil && fdb.IsRetryable(e.Code)
	case *RecordCoreRetriableTransactionError:
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, branch := range u.Unwrap() {
			if isRetriableAnyCause(branch) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return isRetriableAnyCause(u.Unwrap())
	}
	return false
}

// isRetriableFirstCause is FDBExceptions.isRetriable (:236-247), the rule only
// AutoContinuingCursor uses: a RecordCoreRetriableTransactionException, or
// else the FIRST FDB error in the chain and whether it is retryable.
func isRetriableFirstCause(err error) bool {
	var retriable *RecordCoreRetriableTransactionError
	if errors.As(err, &retriable) {
		return true
	}
	var fdbErr fdb.Error
	return errors.As(err, &fdbErr) && fdb.IsRetryable(fdbErr.Code)
}

// exponentialDelay is Java's ExponentialDelay: each delay is drawn uniformly
// from [0, current) milliseconds, and current then doubles, capped at the
// maximum and floored at 2 ms.
type exponentialDelay struct {
	current, max time.Duration
	next         time.Duration
	env          *dst.Env
}

const exponentialDelayMinimum = 2 * time.Millisecond

func newExponentialDelay(initial, max time.Duration, env *dst.Env) *exponentialDelay {
	d := &exponentialDelay{current: initial, max: max, env: env}
	d.next = d.draw()
	return d
}

// delay returns the delay to wait now and advances, as ExponentialDelay.delay.
func (d *exponentialDelay) delay() time.Duration {
	wait := d.next
	d.current = max(min(d.current*2, d.max), exponentialDelayMinimum)
	d.next = d.draw()
	return wait
}

// draw is calculateNextDelayMillis: (long) (nextDouble() * current) ms.
func (d *exponentialDelay) draw() time.Duration {
	var u float64
	var b [8]byte
	if d.env != nil {
		if _, err := d.env.Read(b[:]); err == nil {
			u = float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
		}
	} else {
		u = rand.Float64()
	}
	return time.Duration(int64(u*float64(d.current.Milliseconds()))) * time.Millisecond
}
