// Package simfdb is a deterministic, in-memory MVCC implementation of the
// fdb.BackendDatabase contract (RFC-199 Tier 1 — "SimFDB"). It is the third backend
// alongside the pure-Go client and libfdb_c, wired via
// recordlayer.NewFDBDatabaseWithBackend, so the entire record + relational stack can run
// single-goroutine, with no cluster and no Docker, reproducibly from one seed.
//
// Everything resolves synchronously: futures carry a ready value (no goroutine, no
// channel), the store is a sorted in-memory keyspace, and versions come from a monotonic
// logical counter. Conflict detection is serializable-snapshot-isolation over read/write
// conflict ranges, resolved against commit-version ordering (see conflict.go).
package simfdb

import fdb "fdb.dev/pkg/fdbgo/fdb"

// The five futures below are synchronous ready-values: the result is known at construction,
// so Get returns immediately, BlockUntilReady is a no-op, and the future is always ready.
// This is the RFC-199 "synchronous ready-futures" reuse — the pure-Go client's equivalents
// (future.go newReadyFutureByteSlice) are unexported, so SimFDB carries its own, as libfdbc
// does. A nil error means success; a non-nil error is an FDB error the caller unwraps.

type readyByteSlice struct {
	val []byte
	err error
}

func newReadyByteSlice(val []byte, err error) fdb.FutureByteSlice { return readyByteSlice{val, err} }

func (f readyByteSlice) Get() ([]byte, error) { return f.val, f.err }
func (f readyByteSlice) MustGet() []byte {
	if f.err != nil {
		panic(f.err)
	}
	return f.val
}
func (f readyByteSlice) BlockUntilReady() {}
func (f readyByteSlice) IsReady() bool    { return true }
func (f readyByteSlice) Cancel()          {}

type readyNil struct{ err error }

func newReadyNil(err error) fdb.FutureNil { return readyNil{err} }

func (f readyNil) Get() error { return f.err }
func (f readyNil) MustGet() {
	if f.err != nil {
		panic(f.err)
	}
}
func (f readyNil) BlockUntilReady() {}
func (f readyNil) IsReady() bool    { return true }
func (f readyNil) Cancel()          {}

type readyKey struct {
	val fdb.Key
	err error
}

func newReadyKey(val fdb.Key, err error) fdb.FutureKey { return readyKey{val, err} }

func (f readyKey) Get() (fdb.Key, error) { return f.val, f.err }
func (f readyKey) MustGet() fdb.Key {
	if f.err != nil {
		panic(f.err)
	}
	return f.val
}
func (f readyKey) BlockUntilReady() {}
func (f readyKey) IsReady() bool    { return true }
func (f readyKey) Cancel()          {}

type readyInt64 struct {
	val int64
	err error
}

func newReadyInt64(val int64, err error) fdb.FutureInt64 { return readyInt64{val, err} }

func (f readyInt64) Get() (int64, error) { return f.val, f.err }
func (f readyInt64) MustGet() int64 {
	if f.err != nil {
		panic(f.err)
	}
	return f.val
}
func (f readyInt64) BlockUntilReady() {}
func (f readyInt64) IsReady() bool    { return true }
func (f readyInt64) Cancel()          {}

type readyKeyArray struct {
	val []fdb.Key
	err error
}

func newReadyKeyArray(val []fdb.Key, err error) fdb.FutureKeyArray { return readyKeyArray{val, err} }

func (f readyKeyArray) Get() ([]fdb.Key, error) { return f.val, f.err }
func (f readyKeyArray) MustGet() []fdb.Key {
	if f.err != nil {
		panic(f.err)
	}
	return f.val
}
func (f readyKeyArray) BlockUntilReady() {}
func (f readyKeyArray) IsReady() bool    { return true }
func (f readyKeyArray) Cancel()          {}
