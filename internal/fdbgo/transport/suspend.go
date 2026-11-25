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
// path fires it before blocking: the dial wait, high-water backpressure (or on
// the legacy writer a full write queue, the flush acknowledgment, and the writer
// turn's coalescing delay), the reply wait, and delays.
type SuspendSignal struct {
	once       sync.Once
	ch         chan struct{}
	resume     <-chan struct{}
	beforeWait func()
}

// suspendGate counts unfired signals, so with none live NotifySuspend is one
// atomic load. Process-scoped because WithSuspendSignal attaches to an
// arbitrary context.Context before any Conn or Database is selected.
type suspendGate struct {
	live atomic.Int32
}

func (g *suspendGate) enter()       { g.live.Add(1) }
func (g *suspendGate) leave()       { g.live.Add(-1) }
func (g *suspendGate) active() bool { return g.live.Load() != 0 }

var defaultSuspendGate suspendGate

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
	defaultSuspendGate.enter()
	return context.WithValue(ctx, suspendKey{}, s), s
}

// Fire closes Done once.
func (s *SuspendSignal) Fire() {
	s.once.Do(func() {
		defaultSuspendGate.leave()
		close(s.ch)
	})
}

// Done is closed at the first wait, or when the owner fires it.
func (s *SuspendSignal) Done() <-chan struct{} { return s.ch }

// NotifySuspend fires ctx's signal, if it carries one: the caller is about to wait.
func NotifySuspend(ctx context.Context) {
	if !defaultSuspendGate.active() {
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
