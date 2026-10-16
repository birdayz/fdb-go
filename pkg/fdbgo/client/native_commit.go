package client

import (
	"context"
	"runtime"
	"sync"
	"time"
	"weak"
)

// nativeCommit outlives the RYW wait on Cancel/timeout, but not Reset or owner
// destruction (NativeAPI.actor.cpp:6172, 6942–6945).
type nativeCommit struct {
	ctx        context.Context
	cancel     context.CancelCauseFunc
	completion weak.Pointer[versionstampCompletion]
	cleanup    runtime.Cleanup
	finished   chan struct{}
	done       chan struct{}

	mu       sync.Mutex
	selected bool
	native   bool
	err      error
	panicked any
}

// selectResult is also called under readErrMu when resetPromise is failed.
func (n *nativeCommit) selectResult(err error, native bool, panicked any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.selected {
		return
	}
	n.selected, n.native, n.err, n.panicked = true, native, err, panicked
	if !native {
		close(n.done)
	}
}

func (n *nativeCommit) deliver() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.native {
		close(n.done)
	}
}

func (n *nativeCommit) stop() { n.cancel(nil) }

// Start under the execution lease, before Reset can retire this admission.
func (tx *Transaction) startNativeCommit(caller context.Context, inc *readIncarnation, completion *versionstampCompletion, input *commitInput, tok cacheToken) (*nativeCommit, error) {
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if inc.cause != nil {
		return nil, inc.cause
	}
	if !input.db.registerBackgroundGoroutine() {
		return nil, context.Canceled
	}
	// An opContext contains a strong tx reference even through WithoutCancel.
	ctx, cancel := context.WithCancelCause(input.db.ctx)
	n := &nativeCommit{
		ctx: ctx, cancel: cancel, completion: weak.Make(completion),
		finished: make(chan struct{}), done: make(chan struct{}),
	}
	inc.addNativeLocked(n)
	n.cleanup = runtime.AddCleanup(tx, (*nativeCommit).stop, n)
	input.versionstamp = nil // the worker must not root completion.tx
	go n.run(caller, input, tok)
	return n, nil
}

func (n *nativeCommit) call(caller context.Context, input *commitInput) (result commitOutcome, err error, panicked any) {
	defer func() {
		panicked = recover()
		pb := panicBackstop{name: "nativeCommit", db: input.db}
		pb.recovered(panicked)
	}()
	if input.readState != nil {
		// Caller cancellation applies through GRV only; native owner drop applies throughout.
		var ctx context.Context
		cancel, stop := func() {}, func() bool { return false }
		if caller.Done() == nil {
			// Only owner drop can end this GRV, so it runs in an incarnation
			// that is the native context itself: the read state is private
			// to this goroutine, and nothing else fails or resets it.
			input.readState.readLife = &readIncarnation{ctx: n.ctx, cancel: n.cancel}
			ctx = context.Background()
		} else {
			var c context.CancelFunc
			ctx, c = context.WithCancel(caller)
			cancel, stop = c, context.AfterFunc(n.ctx, c)
			if n.ctx.Err() != nil {
				cancel()
			}
		}
		input.readVersion, err = input.readState.readVersionForOperation(ctx, grvFlagCausalReadRisky)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = n.ctx.Err()
		}
		stop()
		cancel()
		if err != nil {
			input.readState = nil
			return result, err, nil
		}
		input.readState.readVersionMu.Lock()
		input.metricStart = input.readState.metricStart
		input.readState.readVersionMu.Unlock()
		input.readState = nil
	}
	input.commitStarted = time.Now()
	result, err = input.commitNative(n.ctx)
	return result, err, nil
}

func (n *nativeCommit) run(caller context.Context, input *commitInput, tok cacheToken) {
	defer input.db.wg.Done()
	defer close(n.finished)
	defer n.cleanup.Stop()
	defer n.stop()
	defer input.release()
	result, err, panicked := n.call(caller, input)
	input.release()
	// Upgrade only after I/O: retaining completion/its incarnation would keep
	// the transaction alive indefinitely when the public wait has returned.
	completion := n.completion.Value()
	if completion != nil {
		tx := completion.tx
		tx.readErrMu.Lock()
		if panicked == nil {
			if err == nil && tx.readLife == completion.inc {
				tx.committedVersion = result.version
			}
			completion.finishNativeLocked(result, err)
		}
		n.selectResult(err, true, panicked)
		completion.inc.removeNativeLocked(n)
		tx.readErrMu.Unlock()
	} else {
		n.selectResult(err, true, panicked)
	}
	if err == nil && panicked == nil {
		input.publishSuccess(input.commitStarted, tok, result)
		if completion != nil {
			completion.tx.publishCommit(completion.inc, nil, result)
		}
	}
	n.deliver()
}
