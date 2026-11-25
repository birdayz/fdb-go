// Package hrtimer is a one-shot timer that fires with sub-millisecond
// precision. Go's runtime rounds a timer shorter than 1 ms up to 1 ms when
// every P is idle (it waits in epoll_wait with a millisecond timeout); Flow
// arms a timerfd-backed asio timer instead (flow/Net2.actor.cpp:2101-2124).
// The GRV batch window (RFC-259) needs Flow's precision.
//
// A Timer has one owner goroutine that calls Wait. Reset and Stop may be
// called from any goroutine, including while Wait is parked. Wait can return
// for an expiry that a later Reset or Stop superseded, so the owner must
// recheck its own state after every Wait.
package hrtimer

import "errors"

// ErrUnsupported is returned by New on platforms without a precise timer;
// callers fall back to the runtime's timers.
var ErrUnsupported = errors.New("hrtimer: unsupported platform")

// ErrClosed is returned by Wait, Reset and Stop after Close.
var ErrClosed = errors.New("hrtimer: timer closed")
