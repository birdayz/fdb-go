//go:build !linux

package hrtimer

import "time"

// Timer is unavailable off Linux; New always fails and callers fall back to
// the runtime's timers.
type Timer struct{}

// New reports ErrUnsupported.
func New() (*Timer, error) { return nil, ErrUnsupported }

// Reset reports ErrUnsupported.
func (*Timer) Reset(time.Duration) error { return ErrUnsupported }

// Stop reports ErrUnsupported.
func (*Timer) Stop() error { return ErrUnsupported }

// Wait reports ErrUnsupported.
func (*Timer) Wait() error { return ErrUnsupported }

// Close reports ErrUnsupported.
func (*Timer) Close() error { return ErrUnsupported }
