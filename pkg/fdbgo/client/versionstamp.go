package client

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"

	"fdb.dev/pkg/fdbgo/wire"
)

// versionstampCompletion belongs to one admitted commit producer. Its fields use
// tx.readErrMu; results publish through that lock or an observer's done channel.
// Commit's return value is separate: a retired versionstamp cannot cancel a
// dispatched commit, and a pre-native Commit failure does not complete the stamp.
type versionstampCompletion struct {
	tx      *Transaction
	inc     *readIncarnation
	done    chan struct{}
	claimed bool
	ready   bool
	retired bool
	result  commitOutcome
	err     error
}

func (tx *Transaction) newVersionstampLocked(inc *readIncarnation) *versionstampCompletion {
	c := &versionstampCompletion{tx: tx, inc: inc}
	if previous := inc.versionstamp; previous != nil && !previous.ready {
		if inc.versionstamps == nil {
			inc.versionstamps = make(map[*versionstampCompletion]struct{})
		}
		inc.versionstamps[previous] = struct{}{}
	}
	inc.versionstamp = c
	return c
}

func (c *versionstampCompletion) selectLocked(result commitOutcome, err error, retired bool) {
	if c.ready {
		return
	}
	c.result, c.err, c.retired, c.ready = result, err, retired, true
	delete(c.inc.versionstamps, c)
	if c.done != nil {
		close(c.done)
	}
}

// finishNative runs at native commit completion, after uncertainty handling.
// C++ commitAndWatch reports transaction_invalid_version, not the Commit error,
// and excludes actor_cancelled. Go caller interruption is likewise not a shared
// promise failure. Incarnation retirement has its own first-cause authority.
func (c *versionstampCompletion) finishNative(result commitOutcome, err error) {
	if c == nil {
		return
	}
	c.tx.readErrMu.Lock()
	c.finishNativeLocked(result, err)
	c.tx.readErrMu.Unlock()
}

func (c *versionstampCompletion) finishNativeLocked(result commitOutcome, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	// flow/Error.h aliases actor_cancelled to operation_cancelled (1101).
	if code, ok := fdbErrorCode(err); ok && code == 1101 {
		return
	}
	if err != nil {
		err = &wire.FDBError{Code: 2020}
	}
	c.selectLocked(result, err, false)
}

func (c *versionstampCompletion) finishNoWrite() {
	c.tx.readErrMu.Lock()
	c.selectLocked(commitOutcome{}, &wire.FDBError{Code: 2021}, false)
	c.tx.readErrMu.Unlock()
}

// value requires readErrMu, a ready getter capture, or an observed done signal.
// Each handle owns its bytes; callers cannot mutate another future's result.
func (c *versionstampCompletion) value() ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	value := make([]byte, 10)
	binary.BigEndian.PutUint64(value[:8], uint64(c.result.version))
	binary.BigEndian.PutUint16(value[8:], c.result.batchID)
	return value, nil
}

// PendingVersionstamp retains its admission and completion owner across Reset.
// It holds no execution lease while waiting, so an unconsumed future cannot block
// turnover. Caller interruption is memoized per handle, not in the shared promise.
type PendingVersionstamp struct {
	ctx        context.Context
	cleanup    context.CancelFunc
	completion *versionstampCompletion
	done       <-chan struct{}
	once       sync.Once
	value      []byte
	err        error
}

func (p *PendingVersionstamp) Resolve() ([]byte, error) {
	p.once.Do(func() {
		defer p.cleanup()
		done := p.done
		// A selected native result is immutable, including when interruption
		// is also ready by the time the consumer runs.
		select {
		case <-done:
		default:
			select {
			case <-done:
			case <-p.ctx.Done():
				select {
				case <-done:
				default:
					p.err = p.completion.tx.mapReadError(p.ctx, p.ctx.Err())
					return
				}
			}
		}
		if p.completion.retired {
			// A reused handle can retain a retired completion from an older
			// incarnation. Preserve caller-first interruption, but take the
			// terminal cause from that sealed record, not this caller's owner.
			if err := p.completion.tx.readOperation(p.ctx).parent.Err(); err != nil {
				p.err = err
				return
			}
		}
		p.value, p.err = p.completion.value()
	})
	return p.value, p.err
}

// GetVersionstampPending captures entry synchronously. A nil pending handle
// denotes an already-selected value/error; otherwise Resolve waits for this
// incarnation's native versionstamp outcome or its captured retirement signal.
func (tx *Transaction) GetVersionstampPending(parent context.Context) ([]byte, *PendingVersionstamp, error) {
	ctx, cleanup, owned := tx.opContextWithLease(parent, false)
	op := tx.readOperation(ctx)
	if err := tx.readEntryError(ctx); err != nil {
		cleanup.release()
		return nil, nil, err
	}
	tx.readErrMu.Lock()
	completion := op.inc.versionstamp
	if completion == nil && tx.hasCommitted {
		completion = tx.lastVersionstamp
	}
	if completion == nil {
		completion = tx.newVersionstampLocked(op.inc)
	}
	if op.inc.cause != nil {
		completion.selectLocked(commitOutcome{}, op.inc.cause, true)
	}
	ready := completion.ready
	done := completion.done
	if done == nil {
		if ready {
			// The mutex published these fields, not the shared signal's close.
			done = closedDone
		} else {
			completion.done = make(chan struct{})
			done = completion.done
		}
	}
	tx.readErrMu.Unlock()
	owned.release()
	pending := &PendingVersionstamp{ctx: ctx, cleanup: cleanup.afterLease(), completion: completion, done: done}
	if ready {
		value, err := pending.Resolve()
		return value, nil, err
	}
	return nil, pending, nil
}

// PendingCommit is a synchronously admitted producer. Resolve executes exactly
// once; the facade starts it eagerly in its normal panic-protected future. No
// execution lease escapes preparation, even if this handle is never consumed.
type PendingCommit struct {
	tx         *Transaction
	ctx        context.Context
	cleanup    context.CancelFunc
	completion *versionstampCompletion
	once       sync.Once
	err        error
}

func (p *PendingCommit) Resolve() error {
	p.once.Do(func() {
		defer p.cleanup()
		if p.err == nil {
			ctx, release, owned := p.tx.opContextWithLease(p.ctx, false)
			defer release.release()
			p.err = p.tx.commitAdmitted(ctx, p.completion, owned)
		}
	})
	return p.err
}

// PrepareCommit binds the producer before the facade starts asynchronous work.
// The synchronous Commit path uses the same admission without a future goroutine.
func (tx *Transaction) PrepareCommit(parent context.Context) *PendingCommit {
	ctx, cleanup, owned := tx.opContextWithLease(parent, false)
	completion, err := tx.admitCommit(ctx)
	owned.release()
	return &PendingCommit{tx: tx, ctx: ctx, cleanup: cleanup.afterLease(), completion: completion, err: err}
}

func (tx *Transaction) admitCommit(ctx context.Context) (*versionstampCompletion, error) {
	if err := tx.commitEntryError(ctx); err != nil {
		return nil, err
	}
	op := tx.readOperation(ctx)
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if op.inc.cause != nil {
		return nil, op.inc.cause
	}
	completion := op.inc.versionstamp
	if completion == nil || completion.claimed || completion.ready {
		completion = tx.newVersionstampLocked(op.inc)
	}
	completion.claimed = true
	return completion, nil
}
