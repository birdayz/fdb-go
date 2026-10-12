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
	cancel     context.CancelFunc
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

func (n *nativeCommit) stop() { n.cancel() }

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
	ctx, cancel := context.WithCancel(input.db.ctx)
	n := &nativeCommit{
		ctx: ctx, cancel: cancel, completion: weak.Make(completion),
		finished: make(chan struct{}), done: make(chan struct{}),
	}
	if inc.nativeCommits == nil {
		inc.nativeCommits = make(map[*nativeCommit]struct{})
	}
	inc.nativeCommits[n] = struct{}{}
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
		ctx, cancel := context.WithCancel(caller)
		stop := context.AfterFunc(n.ctx, cancel)
		if n.ctx.Err() != nil {
			cancel()
		}
		input.readVersion, err = input.readState.readVersionForOperation(ctx, grvFlagCausalReadRisky)
		if err == nil {
			err = ctx.Err()
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
	defer n.cancel()
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
		delete(completion.inc.nativeCommits, n)
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
