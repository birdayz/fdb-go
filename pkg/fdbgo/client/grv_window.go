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
// Linux, or if no timerfd can be created, the runtime's timers. nil when Close
// has begun.
func (b *grvBatcher) newWindowLocked(db *database) batchWindow {
	t, err := hrtimer.New()
	if err != nil {
		return &afterFuncWindow{b: b, db: db}
	}
	// Registered under the close barrier, so no wg.Add races Close's Wait.
	if !db.registerBackgroundGoroutine() {
		_ = t.Close()
		return nil
	}
	context.AfterFunc(db.ctx, func() { _ = t.Close() })
	go b.runWindow(db, t)
	return hrWindow{t}
}

// runWindow sends each batch whose window expires. When the timer is closed
// (database Close) or fails, it detaches whatever is pending, sends it (the
// RPC fails on a closed database, so no waiter is stranded) and exits; the
// next window then creates a new timer, or none once Close has begun.
func (b *grvBatcher) runWindow(db *database, t *hrtimer.Timer) {
	defer db.wg.Done()
	for {
		if err := t.Wait(); err != nil {
			b.mu.Lock()
			var batch []grvRequest
			if len(b.pending) > 0 {
				batch = b.detachLocked(false)
			}
			if w, ok := b.window.(hrWindow); ok && w.t == t {
				b.window = nil
			}
			b.mu.Unlock()
			_ = t.Close()
			if batch != nil {
				b.flush(db, batch)
			}
			return
		}
		if batch := b.expiredBatch(); batch != nil {
			go b.flush(db, batch)
		}
	}
}

type hrWindow struct{ t *hrtimer.Timer }

func (w hrWindow) arm(d time.Duration) bool { return w.t.Reset(d) == nil }
func (w hrWindow) stop()                    { _ = w.t.Stop() }

// afterFuncWindow is the fallback: today's runtime timer, coarse when the
// process is idle.
type afterFuncWindow struct {
	b     *grvBatcher
	db    *database
	timer *time.Timer
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
