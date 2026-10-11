package fdb

import (
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/client"
)

// Future represents a value (or error) available at some later time.
type Future interface {
	BlockUntilReady()
	IsReady() bool
	Cancel()
}

// futureBase is the completion shared by every future: the operation or a
// Cancel publishes exactly once, and readers wait on done.
type futureBase struct {
	done chan struct{}
	once sync.Once
	err  error
}

func (f *futureBase) init() {
	f.done = make(chan struct{})
}

func (f *futureBase) finish(set func()) {
	f.once.Do(func() {
		set()
		close(f.done)
	})
}

func (f *futureBase) BlockUntilReady() {
	<-f.done
}

func (f *futureBase) IsReady() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Cancel completes a future that is not yet ready with operation_cancelled
// (1101), as fdb_future_cancel does; the operation itself is not stopped.
func (f *futureBase) Cancel() {
	f.finish(func() { f.err = Error{Code: 1101} })
}

// runFuture resolves f from fn on its own goroutine; a Cancel may win.
func runFuture[T any](f *futureBase, val *T, fn func() (T, error)) {
	go func() {
		var v T
		var err error
		defer func() { f.finish(func() { *val, f.err = v, err }) }()
		defer recoverFuturePanic(func(e error) { err = e })
		v, err = fn()
	}()
}

// FutureByteSlice represents the asynchronous result of a function that
// returns a byte slice.
type FutureByteSlice interface {
	Get() ([]byte, error)
	MustGet() []byte
	Future
}

type futureByteSlice struct {
	futureBase
	val []byte
}

func (f *futureByteSlice) Get() ([]byte, error) {
	f.BlockUntilReady()
	return f.val, f.err
}

func (f *futureByteSlice) MustGet() []byte {
	val, err := f.Get()
	if err != nil {
		panic(err)
	}
	return val
}

func newReadyFutureByteSlice(val []byte, err error) FutureByteSlice {
	f := &futureByteSlice{}
	f.init()
	f.finish(func() { f.val, f.err = val, err })
	return f
}

func newFutureByteSlice(fn func() ([]byte, error)) FutureByteSlice {
	f := &futureByteSlice{}
	f.init()
	runFuture(&f.futureBase, &f.val, fn)
	return f
}

// newPendingFutureByteSlice wraps a pipelined read. Get resolves it on the
// caller's goroutine, so N Gets flush their N queued frames together; polling
// IsReady starts a background resolve instead, so it does become ready.
// No channel: this is the per-Get hot path.
func newPendingFutureByteSlice(pending *client.PendingGet) FutureByteSlice {
	return &pendingFutureByteSlice{pending: pending}
}

type pendingFutureByteSlice struct {
	pending *client.PendingGet
	once    sync.Once
	ready   atomic.Bool
	polled  atomic.Bool
	val     []byte
	err     error
}

func (f *pendingFutureByteSlice) finish(val []byte, err error) {
	f.once.Do(func() {
		f.val, f.err = val, err
		f.ready.Store(true)
	})
}

// resolve may run on several goroutines: PendingGet.Resolve is memoized.
func (f *pendingFutureByteSlice) resolve() {
	if f.ready.Load() {
		return
	}
	val, err := f.pending.Resolve()
	f.finish(val, convertError(err))
}

func (f *pendingFutureByteSlice) Get() ([]byte, error) {
	f.resolve()
	return f.val, f.err
}

func (f *pendingFutureByteSlice) MustGet() []byte {
	val, err := f.Get()
	if err != nil {
		panic(err)
	}
	return val
}

func (f *pendingFutureByteSlice) BlockUntilReady() { f.resolve() }

func (f *pendingFutureByteSlice) IsReady() bool {
	if f.ready.Load() {
		return true
	}
	if f.polled.CompareAndSwap(false, true) {
		go f.resolve()
	}
	return false
}

func (f *pendingFutureByteSlice) Cancel() { f.finish(nil, Error{Code: 1101}) }

// FutureNil represents the asynchronous result of a function that has no
// return value.
type FutureNil interface {
	Get() error
	MustGet()
	Future
}

type futureNil struct {
	futureBase
}

func (f *futureNil) Get() error {
	f.BlockUntilReady()
	return f.err
}

func (f *futureNil) MustGet() {
	if err := f.Get(); err != nil {
		panic(err)
	}
}

func newFutureNil(fn func() error) FutureNil {
	f := &futureNil{}
	f.init()
	runFuture(&f.futureBase, new(struct{}), func() (struct{}, error) { return struct{}{}, fn() })
	return f
}

// cancellableFutureNil is a FutureNil whose Cancel() runs a hook before completing with 1101. Watch()
// uses it so that an app freeing an unneeded watch by Cancel()ing the returned future actually
// cancels the underlying watch (its context → the long-poll drains and releases its
// outstanding-watch slot). Without this the slot stays charged until the key changes.
type cancellableFutureNil struct {
	futureNil
	cancel func()
}

func (f *cancellableFutureNil) Cancel() {
	if f.cancel != nil {
		f.cancel()
	}
	f.futureBase.Cancel()
}

func newFutureNilCancel(fn func() error, cancel func()) FutureNil {
	f := &cancellableFutureNil{cancel: cancel}
	f.init()
	runFuture(&f.futureBase, new(struct{}), func() (struct{}, error) { return struct{}{}, fn() })
	return f
}

// FutureKey represents the asynchronous result of a function that returns
// a Key.
type FutureKey interface {
	Get() (Key, error)
	MustGet() Key
	Future
}

type futureKey struct {
	futureBase
	val Key
}

func (f *futureKey) Get() (Key, error) {
	f.BlockUntilReady()
	return f.val, f.err
}

func (f *futureKey) MustGet() Key {
	val, err := f.Get()
	if err != nil {
		panic(err)
	}
	return val
}

func newReadyFutureKey(val Key, err error) FutureKey {
	f := &futureKey{}
	f.init()
	f.finish(func() { f.val, f.err = val, err })
	return f
}

func newFutureKey(fn func() (Key, error)) FutureKey {
	f := &futureKey{}
	f.init()
	runFuture(&f.futureBase, &f.val, fn)
	return f
}

// FutureInt64 represents the asynchronous result of a function that returns
// an int64.
type FutureInt64 interface {
	Get() (int64, error)
	MustGet() int64
	Future
}

type futureInt64 struct {
	futureBase
	val int64
}

func (f *futureInt64) Get() (int64, error) {
	f.BlockUntilReady()
	return f.val, f.err
}

func (f *futureInt64) MustGet() int64 {
	val, err := f.Get()
	if err != nil {
		panic(err)
	}
	return val
}

func newReadyFutureInt64(val int64, err error) FutureInt64 {
	f := &futureInt64{}
	f.init()
	f.finish(func() { f.val, f.err = val, err })
	return f
}

func newFutureInt64(fn func() (int64, error)) FutureInt64 {
	f := &futureInt64{}
	f.init()
	runFuture(&f.futureBase, &f.val, fn)
	return f
}

// FutureKeyArray represents the asynchronous result of a function that
// returns an array of keys.
type FutureKeyArray interface {
	Get() ([]Key, error)
	MustGet() []Key
	Future
}

type futureKeyArray struct {
	futureBase
	val []Key
}

func (f *futureKeyArray) Get() ([]Key, error) {
	f.BlockUntilReady()
	return f.val, f.err
}

func (f *futureKeyArray) MustGet() []Key {
	val, err := f.Get()
	if err != nil {
		panic(err)
	}
	return val
}

func newFutureKeyArray(fn func() ([]Key, error)) FutureKeyArray {
	f := &futureKeyArray{}
	f.init()
	runFuture(&f.futureBase, &f.val, fn)
	return f
}

// FutureStringSlice represents the asynchronous result of a function that
// returns a slice of strings.
type FutureStringSlice interface {
	Get() ([]string, error)
	MustGet() []string
	Future
}

type futureStringSlice struct {
	futureBase
	val []string
}

func (f *futureStringSlice) Get() ([]string, error) {
	f.BlockUntilReady()
	return f.val, f.err
}

func (f *futureStringSlice) MustGet() []string {
	val, err := f.Get()
	if err != nil {
		panic(err)
	}
	return val
}
