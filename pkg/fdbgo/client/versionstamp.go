package client

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"

	"fdb.dev/pkg/fdbgo/wire"
)

// versionstampCompletion belongs to one admitted commit producer. All mutable
// fields are protected by tx.readErrMu; closing done publishes an immutable result.
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
	c := &versionstampCompletion{tx: tx, inc: inc, done: make(chan struct{})}
	if inc.versionstamps == nil {
		inc.versionstamps = make(map[*versionstampCompletion]struct{})
	}
	inc.versionstamps[c] = struct{}{}
	inc.versionstamp = c
	return c
}

func (c *versionstampCompletion) selectLocked(result commitOutcome, err error, retired bool) {
	if c.ready {
		return
	}
	c.result, c.err, c.retired, c.ready = result, err, retired, true
	delete(c.inc.versionstamps, c)
	close(c.done)
}

// finishNative runs at native commit completion, after uncertainty handling.
// C++ commitAndWatch reports transaction_invalid_version, not the Commit error,
// and excludes actor_cancelled. Go caller interruption is likewise not a shared
// promise failure. Incarnation retirement has its own first-cause authority.
func (c *versionstampCompletion) finishNative(result commitOutcome, err error) {
	if c == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	var fdbErr *wire.FDBError
	// flow/Error.h aliases actor_cancelled to operation_cancelled (1101).
	if errors.As(err, &fdbErr) && fdbErr != nil && fdbErr.Code == 1101 {
		return
	}
	if err != nil {
		err = &wire.FDBError{Code: 2020}
	}
	c.tx.readErrMu.Lock()
	c.selectLocked(result, err, false)
	c.tx.readErrMu.Unlock()
}

func (c *versionstampCompletion) finishNoWrite() {
	c.tx.readErrMu.Lock()
	c.selectLocked(commitOutcome{}, &wire.FDBError{Code: 2021}, false)
	c.tx.readErrMu.Unlock()
}

// value is called only after observing done, or under readErrMu. Give each handle
// its own bytes: callers must not be able to mutate another future's result.
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
	once       sync.Once
	value      []byte
	err        error
}

func (p *PendingVersionstamp) Resolve() ([]byte, error) {
	p.once.Do(func() {
		defer p.cleanup()
		// A selected native result is immutable, including when interruption
		// is also ready by the time the consumer runs.
		select {
		case <-p.completion.done:
		default:
			select {
			case <-p.completion.done:
			case <-p.ctx.Done():
				select {
				case <-p.completion.done:
				default:
					p.err = p.completion.tx.mapReadError(p.ctx, p.ctx.Err())
					return
				}
			}
		}
		if p.completion.retired {
			p.err = p.completion.tx.mapReadError(p.ctx, context.Canceled)
			return
		}
		p.value, p.err = p.completion.value()
	})
	return p.value, p.err
}

// GetVersionstampPending captures entry synchronously. A nil pending handle
// denotes an already-selected value/error; otherwise Resolve waits for this
// incarnation's native versionstamp outcome or its captured retirement signal.
func (tx *Transaction) GetVersionstampPending(parent context.Context) ([]byte, *PendingVersionstamp, error) {
	ctx, cleanup := tx.opContext(parent)
	op := tx.readOperation(ctx)
	if err := tx.readEntryError(ctx); err != nil {
		cleanup()
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
	tx.readErrMu.Unlock()
	op.lease.release()
	pending := &PendingVersionstamp{ctx: ctx, cleanup: cleanup, completion: completion}
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
			p.err = p.tx.commitAdmitted(p.ctx, p.completion)
		}
	})
	return p.err
}

// PrepareCommit binds the producer before the facade starts asynchronous work.
// The synchronous Commit path uses the same admission without a future goroutine.
func (tx *Transaction) PrepareCommit(parent context.Context) *PendingCommit {
	ctx, cleanup := tx.opContext(parent)
	completion, err := tx.admitCommit(ctx)
	tx.readOperation(ctx).lease.release()
	return &PendingCommit{tx: tx, ctx: ctx, cleanup: cleanup, completion: completion, err: err}
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
