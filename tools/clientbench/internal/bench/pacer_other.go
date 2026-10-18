//go:build !linux

package bench

import "time"

// pacer falls back to the runtime's timers, which may wake up to a millisecond late.
type pacer struct{ closed chan struct{} }

func newPacer() (*pacer, error) { return &pacer{closed: make(chan struct{})}, nil }

func (p *pacer) wait(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-p.closed:
		return false
	}
}

func (p *pacer) close() {
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
}
