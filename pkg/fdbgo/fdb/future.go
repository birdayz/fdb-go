package fdb

import (
	"fdb.dev/pkg/fdbgo/client"
)

// Future represents a value (or error) available at some later time.
type Future interface {
	BlockUntilReady()
	IsReady() bool
	Cancel()
}

// futureBase provides goroutine-backed future implementation.
// All read operations (Get, GetRange, etc.) start a goroutine and store
// the result in the future. Get() blocks until the goroutine completes.
type futureBase struct {
	done chan struct{}
	err  error
}

func (f *futureBase) init() {
	f.done = make(chan struct{})
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

func (f *futureBase) Cancel() {
	// In the pure Go client, cancellation is a no-op on futures since
	// the underlying operation is already in-flight. The transaction's
	// Cancel() method is the proper way to cancel operations.
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
	f.val = val
	f.err = err
	close(f.done)
	return f
}

func newFutureByteSlice(fn func() ([]byte, error)) FutureByteSlice {
	f := &futureByteSlice{}
	f.init()
	go func() {
		defer close(f.done)
		defer recoverFuturePanic(func(e error) { f.err = e }) // RFC-110
		f.val, f.err = fn()
	}()
	return f
}

// newPendingFutureByteSlice creates a future backed by a PendingGet.
// No goroutine is spawned. Resolve() is called lazily on first Get()/MustGet().
//
// This is critical for pipelining: N tx.Get() calls write N frames to the
// buffer via SendFrameDeferred. If we spawned a goroutine per Get, it would
// race to call Flush() before all N frames are queued, defeating batching.
// With lazy resolve, the first MustGet() flushes all N frames at once.
func newPendingFutureByteSlice(pending *client.PendingGet) FutureByteSlice {
	return &pendingFutureByteSlice{pending: pending}
}

// pendingFutureByteSlice resolves lazily — no goroutine, no channel.
// Matches C++ client where futures are resolved by the network thread
// and Get() blocks on the result directly.
//
// Single-goroutine: each future is created by tx.Get() and resolved by
// the same goroutine. No sync.Once needed — plain bool flag suffices.
type pendingFutureByteSlice struct {
	pending  *client.PendingGet
	val      []byte
	err      error
	resolved bool
}

func (f *pendingFutureByteSlice) resolve() {
	if !f.resolved {
		var err error
		f.val, err = f.pending.Resolve()
		f.err = convertError(err)
		f.resolved = true
	}
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

func (f *pendingFutureByteSlice) IsReady() bool { return f.resolved }

func (f *pendingFutureByteSlice) Cancel() {}

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

func newReadyFutureNil(err error) FutureNil {
	f := &futureNil{}
	f.init()
	f.err = err
	close(f.done)
	return f
}

func newFutureNil(fn func() error) FutureNil {
	f := &futureNil{}
	f.init()
	go func() {
		defer close(f.done)
		defer recoverFuturePanic(func(e error) { f.err = e }) // RFC-110
		f.err = fn()
	}()
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
	f.val = val
	f.err = err
	close(f.done)
	return f
}

func newFutureKey(fn func() (Key, error)) FutureKey {
	f := &futureKey{}
	f.init()
	go func() {
		defer close(f.done)
		defer recoverFuturePanic(func(e error) { f.err = e }) // RFC-110
		f.val, f.err = fn()
	}()
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
	f.val = val
	f.err = err
	close(f.done)
	return f
}

func newFutureInt64(fn func() (int64, error)) FutureInt64 {
	f := &futureInt64{}
	f.init()
	go func() {
		defer close(f.done)
		defer recoverFuturePanic(func(e error) { f.err = e }) // RFC-110
		f.val, f.err = fn()
	}()
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

func newReadyFutureKeyArray(val []Key, err error) FutureKeyArray {
	f := &futureKeyArray{}
	f.init()
	f.val = val
	f.err = err
	close(f.done)
	return f
}

func newFutureKeyArray(fn func() ([]Key, error)) FutureKeyArray {
	f := &futureKeyArray{}
	f.init()
	go func() {
		defer close(f.done)
		defer recoverFuturePanic(func(e error) { f.err = e }) // RFC-110
		f.val, f.err = fn()
	}()
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

func newReadyFutureStringSlice(val []string, err error) FutureStringSlice {
	f := &futureStringSlice{}
	f.init()
	f.val = val
	f.err = err
	close(f.done)
	return f
}
