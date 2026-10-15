package client

import (
	"context"
	"time"

	"fdb.dev/pkg/fdbgo/internal/hrtimer"
)

// batchWindow is the timer behind a GRV batch window (RFC-259). The batcher
// calls arm and stop under b.mu; an expiry calls b.expiredBatch, which checks
// the window's deadline, so a late or superseded expiry detaches nothing.
type batchWindow interface {
	// arm starts the window's timer; false means no timer is available
	// and the caller sends the batch at once.
	arm(d time.Duration) bool
	stop()
}

// newWindowLocked creates b's window timer: a timerfd read by one worker
// goroutine, so a sub-millisecond window is not rounded up to the runtime's
// 1 ms epoll timeout (flow/Net2.actor.cpp:2101-2124 arms a precise timer). Off
// Linux, or if no timerfd can be created, the runtime's timers. nil once
// Close has begun.
func (b *grvBatcher) newWindowLocked(db *database) batchWindow {
	if db.ctx.Err() != nil {
		return nil
	}
	t, err := hrtimer.New()
	if err != nil {
		return b.newFallbackWindowLocked(db)
	}
	// Registered under the close barrier, so no wg.Add races Close's Wait.
	if !db.registerBackgroundGoroutine() {
		_ = t.Close()
		return nil
	}
	w := &hrWindow{t: t}
	w.stopHook = context.AfterFunc(db.ctx, func() { _ = t.Close() })
	go b.runWindow(db, w)
	return w
}

// newFallbackWindowLocked uses runtime timers, coarse when the process is
// idle. Close detaches and sends whatever is pending, as with a timerfd.
func (b *grvBatcher) newFallbackWindowLocked(db *database) batchWindow {
	if db.ctx.Err() != nil {
		return nil
	}
	w := &afterFuncWindow{b: b, db: db}
	w.stopHook = context.AfterFunc(db.ctx, func() { b.retireWindow(db, w) })
	return w
}

// runWindow sends each batch whose window expires. When the timer is closed
// (database Close) or fails, it retires the window and exits.
func (b *grvBatcher) runWindow(db *database, w *hrWindow) {
	defer db.wg.Done()
	for {
		if err := w.t.Wait(); err != nil {
			w.stopHook()
			b.retireWindow(db, w)
			_ = w.t.Close()
			return
		}
		if batch := b.expiredBatch(); batch != nil {
			go b.flush(db, batch)
		}
	}
}

// retireWindow detaches and sends whatever is pending in w's window (the RPC
// fails on a closed database, so no waiter is stranded) and drops w, so the
// next window creates a new timer, or none once Close has begun.
func (b *grvBatcher) retireWindow(db *database, w batchWindow) {
	b.mu.Lock()
	var batch []grvRequest
	if b.window == w {
		if len(b.pending) > 0 {
			batch = b.detachLocked()
		}
		b.window = nil
	}
	b.mu.Unlock()
	if batch != nil {
		b.flush(db, batch)
	}
}

type hrWindow struct {
	t        *hrtimer.Timer
	stopHook func() bool
}

func (w *hrWindow) arm(d time.Duration) bool { return w.t.Reset(d) == nil }
func (w *hrWindow) stop()                    { _ = w.t.Stop() }

type afterFuncWindow struct {
	b        *grvBatcher
	db       *database
	timer    *time.Timer
	stopHook func() bool
}

func (w *afterFuncWindow) arm(d time.Duration) bool {
	w.timer = time.AfterFunc(d, func() {
		if batch := w.b.expiredBatch(); batch != nil {
			w.b.flush(w.db, batch)
		}
	})
	return true
}

func (w *afterFuncWindow) stop() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}
