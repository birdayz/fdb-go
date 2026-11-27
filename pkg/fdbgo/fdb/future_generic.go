package fdb

import (
	"errors"
	"sync"
	"sync/atomic"
)

var errFutureCallbackPanicked = errors.New("fdb: future callback panicked")

// Future represents an asynchronous FoundationDB result of type T.
// Implementations produced by [FutureOf], [ReadyFuture], and [MapFuture]
// spawn no wrapper goroutines and evaluate decoding/mapping lazily on Get
// (underlying [RawFuture] implementations may still manage their own background
// work).
type Future[T any] interface {
	// Get blocks until the future is ready and returns its decoded value or error.
	Get() (T, error)
	// MustGet calls Get and panics if it returns a non-nil error.
	MustGet() T
	// IsReady reports whether the future has completed without blocking.
	IsReady() bool
	// Cancel attempts to cancel the future with operation_cancelled (1101) if
	// it has not yet completed. If Get's decode or mapping callback is
	// concurrently running, Cancel waits for it to finish.
	Cancel()
}

// ReadyFuture returns an immediately resolved [Future] holding (val, err)
// without spawning any wrapper goroutines.
func ReadyFuture[T any](val T, err error) Future[T] {
	return readyFuture[T]{val: val, err: err}
}

type readyFuture[T any] struct {
	val T
	err error
}

func (f readyFuture[T]) Get() (T, error) {
	return f.val, f.err
}

func (f readyFuture[T]) MustGet() T {
	if f.err != nil {
		panic(f.err)
	}
	return f.val
}

func (f readyFuture[T]) IsReady() bool {
	return true
}

func (f readyFuture[T]) Cancel() {}

// FutureOf wraps a [RawFuture] with a lazy decode function that runs on the
// caller's goroutine at most once when Get is first called. It spawns no
// wrapper goroutines. Cancel waits if Get's decode callback is concurrently
// running.
func FutureOf[T any](raw RawFuture, decode func() (T, error)) Future[T] {
	if raw == nil || decode == nil {
		var zero T
		return ReadyFuture(zero, Error{Code: 2000})
	}
	return &wrappedFuture[T]{raw: raw, decode: decode}
}

type wrappedFuture[T any] struct {
	raw       RawFuture
	decode    func() (T, error)
	once      sync.Once
	ready     atomic.Bool
	cancelled atomic.Bool
	val       T
	err       error
}

func (f *wrappedFuture[T]) Get() (T, error) {
	if !f.ready.Load() {
		f.raw.BlockUntilReady()
		f.once.Do(func() {
			defer f.ready.Store(true)
			if f.cancelled.Load() {
				f.err = Error{Code: 1101}
				return
			}
			f.err = errFutureCallbackPanicked
			f.val, f.err = f.decode()
		})
	}
	return f.val, f.err
}

func (f *wrappedFuture[T]) MustGet() T {
	val, err := f.Get()
	if err != nil {
		panic(err)
	}
	return val
}

func (f *wrappedFuture[T]) IsReady() bool {
	return f.ready.Load() || f.raw.IsReady()
}

func (f *wrappedFuture[T]) Cancel() {
	if f.IsReady() {
		return
	}
	f.cancelled.Store(true)
	f.raw.Cancel()
	f.once.Do(func() {
		f.err = Error{Code: 1101}
		f.ready.Store(true)
	})
}

// MapFuture transforms a [Future] of type In into a [Future] of type Out by
// running mapFn lazily on the caller's goroutine when Get is called. If the
// input future fails, mapFn is skipped and the error is propagated. It spawns
// no wrapper goroutines and caches the mapped result. Cancel waits if Get's
// mapFn callback is concurrently running.
func MapFuture[In, Out any](f Future[In], mapFn func(In) (Out, error)) Future[Out] {
	if f == nil || mapFn == nil {
		var zero Out
		return ReadyFuture(zero, Error{Code: 2000})
	}
	return &mappedFuture[In, Out]{inner: f, mapFn: mapFn}
}

type mappedFuture[In, Out any] struct {
	inner     Future[In]
	mapFn     func(In) (Out, error)
	once      sync.Once
	ready     atomic.Bool
	cancelled atomic.Bool
	val       Out
	err       error
}

func (m *mappedFuture[In, Out]) Get() (Out, error) {
	if !m.ready.Load() {
		in, err := m.inner.Get()
		m.once.Do(func() {
			defer m.ready.Store(true)
			switch {
			case m.cancelled.Load():
				m.err = Error{Code: 1101}
			case err != nil:
				m.err = err
			default:
				m.err = errFutureCallbackPanicked
				m.val, m.err = m.mapFn(in)
			}
		})
	}
	return m.val, m.err
}

func (m *mappedFuture[In, Out]) MustGet() Out {
	val, err := m.Get()
	if err != nil {
		panic(err)
	}
	return val
}

func (m *mappedFuture[In, Out]) IsReady() bool {
	return m.ready.Load() || m.inner.IsReady()
}

func (m *mappedFuture[In, Out]) Cancel() {
	if m.IsReady() {
		return
	}
	m.cancelled.Store(true)
	m.inner.Cancel()
	m.once.Do(func() {
		m.err = Error{Code: 1101}
		m.ready.Store(true)
	})
}
