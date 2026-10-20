package transport

import (
	"context"
	"sync"
	"sync/atomic"
)

// SuspendSignal marks the first wait of a goroutine that carries it in its
// context: the point where a flow actor would suspend at its first wait that is
// not ready. The client's NativeAPI getRange fallback starts end's resolveKey
// there, after begin's (NativeAPI.actor.cpp:4465-4469). Every wait on a request's
// path fires it before blocking: the dial wait, a full write queue, the flush
// acknowledgment, the writer turn's coalescing delay, the reply wait, and delays.
type SuspendSignal struct {
	once       sync.Once
	ch         chan struct{}
	resume     <-chan struct{}
	beforeWait func()
}

// suspendWatchers counts unfired signals, so with none live NotifySuspend is one
// atomic load.
var suspendWatchers atomic.Int32

type suspendKey struct{}

// WithSuspendSignal returns ctx carrying a new signal. The owner must Fire it
// when the goroutine ends, whether or not it waited.
func WithSuspendSignal(ctx context.Context) (context.Context, *SuspendSignal) {
	return WithSuspendHandoff(ctx, nil, nil)
}

// WithSuspendHandoff also holds the notifying worker until resume closes. Fire
// remains nonblocking so completion and panic cleanup cannot wait on a successor.
// beforeWait optionally observes an unready gate, outside the signal's Once.
func WithSuspendHandoff(ctx context.Context, resume <-chan struct{}, beforeWait func()) (context.Context, *SuspendSignal) {
	s := &SuspendSignal{ch: make(chan struct{}), resume: resume, beforeWait: beforeWait}
	suspendWatchers.Add(1)
	return context.WithValue(ctx, suspendKey{}, s), s
}

// Fire closes Done once.
func (s *SuspendSignal) Fire() {
	s.once.Do(func() {
		suspendWatchers.Add(-1)
		close(s.ch)
	})
}

// Done is closed at the first wait, or when the owner fires it.
func (s *SuspendSignal) Done() <-chan struct{} { return s.ch }

// NotifySuspend fires ctx's signal, if it carries one: the caller is about to wait.
func NotifySuspend(ctx context.Context) {
	if suspendWatchers.Load() == 0 {
		return
	}
	if s, ok := ctx.Value(suspendKey{}).(*SuspendSignal); ok {
		s.Fire()
		if s.resume != nil {
			select {
			case <-s.resume:
				return
			default:
			}
			if s.beforeWait != nil {
				s.beforeWait()
			}
			select {
			case <-s.resume:
			case <-ctx.Done():
			}
		}
	}
}
